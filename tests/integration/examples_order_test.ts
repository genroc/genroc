import { createServer, type ServerResponse } from "http";
import type { AddressInfo } from "net";
import { readFileSync } from "node:fs";
import { join } from "path";
import { tmpdir } from "os";
import { load as loadYaml } from "js-yaml";
import { expect, test, beforeAll } from "vitest";
import { client, waitForInstance } from "../helpers/client.ts";
import { buildGenrocBinary, startGenroc } from "../helpers/server.ts";

// The definition under test is the real example file in examples/order-fulfilment/,
// applied verbatim — so this doubles as an executable check that the shipped example
// works. The point of the example is what happens when a worker dies mid-charge, so the
// interesting tests really kill a worker and let a second one take the work over.
// (Vitest's bundler can't `import` a .yaml file, so we read + parse the source instead.)
const EXAMPLES = new URL("../../examples/order-fulfilment/", import.meta.url);
const order: any = loadYaml(readFileSync(new URL("order.genroc.yaml", EXAMPLES), "utf8"));

// The sqlite and postgres vitest projects run this file in parallel and each spawns its
// own genroc pair, so offset the ports per project to keep them from colliding.
const PORT_OFFSET = (Number(process.env.GENROC_PORT ?? 8888) - 8888) * 4;
const SETTLED1_PORT = 20101 + PORT_OFFSET;
const SETTLED2_PORT = 20102 + PORT_OFFSET;
const RERUN1_PORT = 20111 + PORT_OFFSET;
const RERUN2_PORT = 20112 + PORT_OFFSET;

let genrocBin: string;
beforeAll(async () => {
  genrocBin = await buildGenrocBinary();
}, 120_000);

interface CommerceOptions {
  // Never answer the first charge, so the worker can be killed with it in flight.
  hangFirstCharge?: boolean;
  // Status for charges that do get answered. 200 (default) mints a charge id.
  chargeStatus?: number;
  // What the payment API reports when asked about the idempotency key afterwards —
  // i.e. whether the abandoned attempt actually took the money.
  chargedOnLookup?: boolean;
  // Refuse the reservation with 409 (out of stock).
  reserveStatus?: number;
}

// startCommerceService stands in for the three services the order talks to. The payment
// lookup is the important one: it is the system of record the example consults after an
// interruption, and the ONLY thing that can answer "did the money move?".
async function startCommerceService(opts: CommerceOptions = {}) {
  let chargeAttempts = 0;
  let lookupCount = 0;
  let releaseCount = 0;
  let shipCount = 0;
  let firstChargeSeen: () => void = () => {};
  const firstChargeReceived = new Promise<void>((r) => (firstChargeSeen = r));
  // Held open deliberately; answering would defeat the point of the hang.
  const hung: ServerResponse[] = [];

  const server = createServer((req, res) => {
    const chunks: Buffer[] = [];
    req.on("data", (c) => chunks.push(c as Buffer));
    req.on("end", () => {
      const send = (code: number, obj: unknown) => {
        res.writeHead(code, { "Content-Type": "application/json" });
        res.end(JSON.stringify(obj));
      };
      const url = req.url ?? "";

      if (url === "/inventory/reserve") {
        if (opts.reserveStatus && opts.reserveStatus !== 200) {
          return send(opts.reserveStatus, { error: "out of stock" });
        }
        return send(200, { reservation_id: "res-1" });
      }
      if (url === "/inventory/release") {
        releaseCount++;
        return send(200, {});
      }
      if (url === "/payments/charge") {
        chargeAttempts++;
        if (chargeAttempts === 1) firstChargeSeen();
        if (opts.hangFirstCharge && chargeAttempts === 1) {
          hung.push(res);
          return; // never answers
        }
        const status = opts.chargeStatus ?? 200;
        if (status !== 200) return send(status, { error: "declined" });
        return send(200, { charge_id: `ch-${chargeAttempts}` });
      }
      if (url.startsWith("/payments/by-key/")) {
        lookupCount++;
        return send(200, { charged: opts.chargedOnLookup === true });
      }
      if (url === "/shipping/dispatch") {
        shipCount++;
        return send(200, { tracking: `trk-${shipCount}` });
      }
      send(404, {});
    });
  });

  await new Promise<void>((r) => server.listen(0, r));
  return {
    port: (server.address() as AddressInfo).port,
    firstChargeReceived,
    chargeAttempts: () => chargeAttempts,
    lookupCount: () => lookupCount,
    releaseCount: () => releaseCount,
    // closeAllConnections is required, not tidiness: genroc's HTTP client keeps
    // connections alive (and one request here is deliberately never answered), so a
    // bare close() waits for them and hangs the test.
    stop: () =>
      new Promise<void>((r) => {
        hung.forEach((h) => h.destroy());
        server.closeAllConnections();
        server.close(() => r());
      }),
  };
}

type ApiClient = typeof client;

async function applyExample(api: ApiClient = client) {
  const { error } = await api.PUT("/definitions", { body: order as never });
  expect(error).toBeUndefined();
}

async function startOrder(
  port: number,
  orderRef: string,
  api: ApiClient = client,
): Promise<string> {
  const { data, error } = await api.POST("/instances", {
    body: {
      process: order.name,
      input: {
        base_url: `http://localhost:${port}`,
        order_ref: orderRef,
        customer: "cust-1",
        amount_cents: 2500,
        sku: "widget",
      },
    },
  });
  expect(error).toBeUndefined();
  return data!.id;
}

async function outputsOf(id: string, api: ApiClient = client): Promise<Record<string, any>> {
  const { data } = await api.GET("/instances/{id}/detail", { params: { path: { id } } });
  return ((data?.state as any)?.outputs ?? {}) as Record<string, any>;
}

// waitForInstance, but for a tick-only server (--poll 0).
async function waitForInstanceTicking(
  id: string,
  api: ApiClient,
  timeoutMs = 30_000,
): Promise<string> {
  const deadline = Date.now() + timeoutMs;
  while (Date.now() < deadline) {
    await api.POST("/tick", {});
    const { data } = await api.GET("/instances/{id}/detail", { params: { path: { id } } });
    const status = data?.status;
    if (status === "completed" || status === "failed" || status === "raised") return status;
    await new Promise((r) => setTimeout(r, 50));
  }
  throw new Error(`instance ${id} did not settle within ${timeoutMs}ms`);
}

// ── The uninterrupted paths, on the shared server ────────────────────────────

test("examples/order-fulfilment: an order reserves, charges once, and ships", async () => {
  const mock = await startCommerceService();
  try {
    await applyExample();
    const id = await startOrder(mock.port, `ord-${crypto.randomUUID().slice(0, 8)}`);

    expect(await waitForInstance(id, 30_000)).toBe("completed");
    const outputs = await outputsOf(id);
    expect(outputs.charge).toEqual({ charge_id: "ch-1" });
    expect(outputs.ship).toEqual({ tracking: "trk-1" });

    // Nothing was reconciled, because nothing was interrupted.
    expect(mock.chargeAttempts()).toBe(1);
    expect(mock.lookupCount()).toBe(0);
  } finally {
    await mock.stop();
  }
}, 60_000);

test("examples/order-fulfilment: a declined card releases the reservation and raises", async () => {
  const mock = await startCommerceService({ chargeStatus: 402 });
  try {
    await applyExample();
    const id = await startOrder(mock.port, `ord-${crypto.randomUUID().slice(0, 8)}`);

    // `raised`, not `failed`: a declined card is an anticipated outcome, and the
    // compensation ran before it was reported.
    expect(await waitForInstance(id, 30_000)).toBe("raised");
    expect(mock.releaseCount()).toBe(1);

    // A 402 is an ANSWER, so there is nothing to reconcile — only an interruption,
    // where nothing came back, sends the process to the payment API to ask.
    expect(mock.lookupCount()).toBe(0);
  } finally {
    await mock.stop();
  }
}, 60_000);

test("examples/order-fulfilment: an out-of-stock order never reaches the charge", async () => {
  const mock = await startCommerceService({ reserveStatus: 409 });
  try {
    await applyExample();
    const id = await startOrder(mock.port, `ord-${crypto.randomUUID().slice(0, 8)}`);

    expect(await waitForInstance(id, 30_000)).toBe("raised");
    expect(mock.chargeAttempts()).toBe(0);
  } finally {
    await mock.stop();
  }
}, 60_000);

// ── The interrupted paths, through a real worker crash ───────────────────────

// Kill a worker with the charge in flight, bring a second one up, and expire the lease.
// The engine refuses to repeat the only_once call and raises only_once.interrupted,
// which the definition catches and routes to `reconcile`.
async function crashMidCharge(
  mock: Awaited<ReturnType<typeof startCommerceService>>,
  port1: number,
  port2: number,
  tag: string,
) {
  // A temp SQLite file even under the postgres project: this test is about the example,
  // not the storage engine, and a file DB keeps the two workers on one database with no
  // per-run database to create and drop.
  const db = join(tmpdir(), `genroc_order_${tag}_${Date.now()}.db`);

  const genroc1 = await startGenroc(genrocBin, port1, db);
  await applyExample(genroc1.client as ApiClient);
  const orderRef = `ord-${crypto.randomUUID().slice(0, 8)}`;
  const id = await startOrder(mock.port, orderRef, genroc1.client as ApiClient);

  await Promise.race([
    mock.firstChargeReceived,
    new Promise<never>((_, reject) =>
      setTimeout(() => reject(new Error("charge was never attempted")), 20_000),
    ),
  ]);
  // The charge is in flight and unanswered. This is the crash the example exists for.
  genroc1.crash();

  const genroc2 = await startGenroc(genrocBin, port2, db, undefined, 0);
  // Push past the lease so the second worker may reclaim the row.
  await genroc2.client.POST("/tick", { body: { advance_ms: 12_000 } });
  return {
    id,
    api: genroc2.client as ApiClient,
    stop: () => {
      genroc1.crash();
      genroc2.stop();
    },
  };
}

test("examples/order-fulfilment: an interrupted charge that DID land is reconciled, not repeated", async () => {
  const mock = await startCommerceService({ hangFirstCharge: true, chargedOnLookup: true });
  let run: Awaited<ReturnType<typeof crashMidCharge>> | undefined;
  try {
    run = await crashMidCharge(mock, SETTLED1_PORT, SETTLED2_PORT, "settled");

    expect(await waitForInstanceTicking(run.id, run.api)).toBe("completed");

    // The whole point: the money had already moved, and the order shipped WITHOUT the
    // card being charged a second time. One attempt — the abandoned one.
    expect(mock.chargeAttempts()).toBe(1);
    expect(mock.lookupCount()).toBe(1);

    const outputs = await outputsOf(run.id, run.api);
    expect(outputs.reconcile).toEqual({ charged: true, settled_by: "reconciliation" });
    // `charge` never produced an output: its action never returned.
    expect(outputs.charge).toBeUndefined();
    expect(outputs.ship?.tracking).toMatch(/^trk-/);
  } finally {
    run?.stop();
    await mock.stop();
  }
}, 120_000);

test("examples/order-fulfilment: an interrupted charge that did NOT land is deliberately re-sent", async () => {
  const mock = await startCommerceService({ hangFirstCharge: true, chargedOnLookup: false });
  let run: Awaited<ReturnType<typeof crashMidCharge>> | undefined;
  try {
    run = await crashMidCharge(mock, RERUN1_PORT, RERUN2_PORT, "rerun");

    expect(await waitForInstanceTicking(run.id, run.api)).toBe("completed");

    // Two attempts: the abandoned one, and the one the DEFINITION asked for after
    // establishing it was safe. The engine never repeats an only_once call on its own,
    // but it does not stand in the way of an authored re-entry.
    expect(mock.chargeAttempts()).toBe(2);
    expect(mock.lookupCount()).toBe(1);

    const outputs = await outputsOf(run.id, run.api);
    expect(outputs.reconcile).toEqual({ charged: false, settled_by: "reconciliation" });
    expect(outputs.charge).toEqual({ charge_id: "ch-2" });
    expect(outputs.ship?.tracking).toMatch(/^trk-/);
  } finally {
    run?.stop();
    await mock.stop();
  }
}, 120_000);

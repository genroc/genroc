import { createServer } from "http";
import type { AddressInfo } from "net";
import { readFileSync } from "node:fs";
import { load as loadYaml } from "js-yaml";
import { expect, test } from "vitest";
import { client, waitForInstance } from "../helpers/client.ts";

// The definitions under test are the real example files in examples/batch-invoices/,
// applied verbatim — so this doubles as an executable check that the shipped example
// works. It covers both fan-out shapes (child_map for a fixed pair of named branches,
// child_list for one child per array element) and, more importantly, the distinction the
// example is built to teach: a per-item problem is a RESULT the batch reports, a run-wide
// problem is a RAISE that abandons the batch.
// (Vitest's bundler can't `import` a .yaml file, so we read + parse the source instead.)
const EXAMPLES = new URL("../../examples/batch-invoices/", import.meta.url);
function loadDef(file: string): any {
  return loadYaml(readFileSync(new URL(file, EXAMPLES), "utf8"));
}
const invoice = loadDef("invoice.genroc.yaml");
const rate = loadDef("rate.genroc.yaml");
const run = loadDef("run.genroc.yaml");

// startBillingService stands in for the invoicing API. The invoice id selects the
// outcome, so one mock drives every branch:
//   bad-*    -> 422  permanently unsendable  (child completes with ok:false)
//   locked-* -> 423  billing period locked   (child raises period_closed)
//   flaky-*  -> 503 twice, then 200          (child retries internally)
//   else     -> 200 { delivery_id }
async function startBillingService() {
  let sendCount = 0;
  const rateRequests: string[] = [];
  const attemptsById = new Map<string, number>();

  const server = createServer((req, res) => {
    const chunks: Buffer[] = [];
    req.on("data", (c) => chunks.push(c as Buffer));
    req.on("end", () => {
      const send = (code: number, obj: unknown) => {
        res.writeHead(code, { "Content-Type": "application/json" });
        res.end(JSON.stringify(obj));
      };
      const url = req.url ?? "";

      if (url.startsWith("/rates/")) {
        const currency = url.slice("/rates/".length);
        rateRequests.push(currency);
        return send(200, { rate: currency === "usd" ? 1 : 0.92 });
      }

      if (url === "/invoices/send") {
        sendCount++;
        const body = chunks.length ? JSON.parse(Buffer.concat(chunks).toString()) : {};
        const id = body.invoice_id as string;
        const attempt = (attemptsById.get(id) ?? 0) + 1;
        attemptsById.set(id, attempt);

        if (id.startsWith("bad-")) return send(422, { error: "no billing address" });
        if (id.startsWith("locked-")) return send(423, { error: "period locked" });
        if (id.startsWith("flaky-") && attempt <= 2) return send(503, {});
        return send(200, { delivery_id: `d-${id}` });
      }

      send(404, {});
    });
  });

  await new Promise<void>((r) => server.listen(0, r));
  return {
    port: (server.address() as AddressInfo).port,
    sendCount: () => sendCount,
    rateRequests: () => rateRequests,
    attemptsFor: (id: string) => attemptsById.get(id) ?? 0,
    // closeAllConnections is required, not tidiness: genroc's HTTP client keeps
    // connections alive, and a bare close() waits for them and hangs the test.
    stop: () =>
      new Promise<void>((r) => {
        server.closeAllConnections();
        server.close(() => r());
      }),
  };
}

// Children before the parent, so the parent's child references resolve at registration.
async function applyExample() {
  for (const def of [invoice, rate, run]) {
    const { error } = await client.PUT("/definitions", { body: def as never });
    expect(error).toBeUndefined();
  }
}

async function startRun(port: number, invoices: unknown[]): Promise<string> {
  const { data, error } = await client.POST("/instances", {
    body: {
      process: run.name,
      input: { base_url: `http://localhost:${port}`, period: "2026-07", invoices },
    },
  });
  expect(error).toBeUndefined();
  return data!.id;
}

async function outputsOf(id: string): Promise<Record<string, any>> {
  const { data } = await client.GET("/instances/{id}", { params: { path: { id } } });
  return ((data?.context as any)?.outputs ?? {}) as Record<string, any>;
}

function inv(invoice_id: string, amount_cents = 1000) {
  return { invoice_id, customer: `cust-${invoice_id}`, amount_cents };
}

test("examples/batch-invoices: a per-invoice failure is reported, not raised — every sibling still runs", async () => {
  const mock = await startBillingService();
  try {
    await applyExample();
    // The unsendable one sits in the MIDDLE, so a batch that aborted on it would visibly
    // lose the third invoice rather than merely reordering.
    const id = await startRun(mock.port, [inv("ok-1"), inv("bad-2"), inv("ok-3")]);

    expect(await waitForInstance(id, 30_000)).toBe("completed");

    // All three children completed and the batch collected in `over` order — the failed
    // one included, carrying its reason as data rather than as control flow.
    const outputs = await outputsOf(id);
    expect(outputs.send_all).toEqual([
      { invoice_id: "ok-1", ok: true, delivery_id: "d-ok-1", reason: "" },
      { invoice_id: "bad-2", ok: false, delivery_id: "", reason: "http.422" },
      { invoice_id: "ok-3", ok: true, delivery_id: "d-ok-3", reason: "" },
    ]);

    // The child_map ran both named branches concurrently and keyed the result by name.
    expect(outputs.rates).toEqual({ usd: 1, eur: 0.92 });
    expect([...mock.rateRequests()].sort()).toEqual(["eur", "usd"]);

    // The raise path was never taken.
    expect(outputs.halted).toBeUndefined();
  } finally {
    await mock.stop();
  }
}, 60_000);

test("examples/batch-invoices: a run-wide failure is raised, and the parent catches it by child_index", async () => {
  const mock = await startBillingService();
  try {
    await applyExample();
    const id = await startRun(mock.port, [inv("ok-1"), inv("locked-2"), inv("ok-3")]);

    expect(await waitForInstance(id, 30_000)).toBe("completed");

    // The batch produced NO output — a raise abandons it, so there is no partially
    // populated array. What the parent gets instead is the identity of the slot that
    // raised, which is the whole of `$error` for a fan-out: code and position, no data.
    const outputs = await outputsOf(id);
    expect(outputs.send_all).toBeUndefined();
    expect(outputs.halted).toEqual({
      halted_at: 1,
      code: "period_closed",
      detail: "billing period is locked; no invoice in this run can be sent",
    });
  } finally {
    await mock.stop();
  }
}, 60_000);

test("examples/batch-invoices: a transient failure is retried inside the child, not by the parent", async () => {
  const mock = await startBillingService();
  try {
    await applyExample();
    const id = await startRun(mock.port, [inv("flaky-1")]);

    expect(await waitForInstance(id, 30_000)).toBe("completed");

    // Two 503s then a 200: the child's own on_error absorbed them, so the parent saw a
    // plain success. `retries` is not available on a child_list task, which is exactly
    // why per-item retry policy has to live in the child.
    expect(mock.attemptsFor("flaky-1")).toBe(3);
    const outputs = await outputsOf(id);
    expect(outputs.send_all).toEqual([
      { invoice_id: "flaky-1", ok: true, delivery_id: "d-flaky-1", reason: "" },
    ]);
  } finally {
    await mock.stop();
  }
}, 60_000);

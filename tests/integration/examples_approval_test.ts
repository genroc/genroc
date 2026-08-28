import { claimInProcess } from "../helpers/external.ts";
import { parkedInProcess } from "../helpers/external.ts";
import { createServer } from "http";
import type { AddressInfo } from "net";
import { readFileSync } from "node:fs";
import { join } from "path";
import { tmpdir } from "os";
import { load as loadYaml } from "js-yaml";
import { expect, test, beforeAll } from "vitest";
import { client, waitForInstance } from "../helpers/client.ts";
import { buildGenrocBinary, startGenroc } from "../helpers/server.ts";

// The definition under test is the real example file in examples/expense-approval/,
// applied verbatim — so this doubles as an executable check that the shipped example
// works. It exercises the whole external-task contract: the park, both submission routes
// (queue token and instance signal), result_schema validation at the API boundary, and
// the timeout that escalates.
// (Vitest's bundler can't `import` a .yaml file, so we read + parse the source instead.)
const EXAMPLES = new URL("../../examples/expense-approval/", import.meta.url);
const approval: any = loadYaml(
  readFileSync(new URL("approval.genroc.yaml", EXAMPLES), "utf8"),
);

// The sqlite and postgres vitest projects run this file in parallel, so offset the
// dedicated server's port per project to keep the two from colliding.
const PORT_OFFSET = (Number(process.env.GENROC_PORT ?? 8888) - 8888) * 4;
const ESCALATE_PORT = 20091 + PORT_OFFSET;

let genrocBin: string;
beforeAll(async () => {
  genrocBin = await buildGenrocBinary();
}, 120_000);

// startExpenseService stands in for whatever the org uses to notify people and move
// money. genroc parks the process; it does not deliver the notification.
async function startExpenseService() {
  const notified: string[] = [];
  let paidCount = 0;

  const server = createServer((req, res) => {
    const chunks: Buffer[] = [];
    req.on("data", (c) => chunks.push(c as Buffer));
    req.on("end", () => {
      const send = (code: number, obj: unknown) => {
        res.writeHead(code, { "Content-Type": "application/json" });
        res.end(JSON.stringify(obj));
      };
      const body = chunks.length ? JSON.parse(Buffer.concat(chunks).toString()) : {};

      if (req.url === "/notify") {
        notified.push(body.to as string);
        return send(200, {});
      }
      if (req.url === "/reimburse") {
        paidCount++;
        return send(200, { payment_id: `pay-${paidCount}` });
      }
      send(404, {});
    });
  });

  await new Promise<void>((r) => server.listen(0, r));
  return {
    port: (server.address() as AddressInfo).port,
    notified: () => notified,
    // closeAllConnections is required, not tidiness: genroc's HTTP client keeps
    // connections alive, and a bare close() waits for them and hangs the test.
    stop: () =>
      new Promise<void>((r) => {
        server.closeAllConnections();
        server.close(() => r());
      }),
  };
}

type ApiClient = typeof client;

async function applyExample(api: ApiClient = client) {
  const { error } = await api.PUT("/definitions", { body: approval as never });
  expect(error).toBeUndefined();
}

async function startApproval(
  port: number,
  requester: string,
  api: ApiClient = client,
): Promise<string> {
  const { data, error } = await api.POST("/instances", {
    body: {
      process: approval.name,
      input: {
        base_url: `http://localhost:${port}`,
        requester,
        amount_cents: 4200,
        purpose: "conference ticket",
      },
    },
  });
  expect(error).toBeUndefined();
  return data!.id;
}

// Poll the external-task queue until this instance's entry for `taskId` is armed, and
// return it. The queue is the resolver's whole view of the process: an input snapshot,
// the schema the answer must satisfy, and a token — never the process context.
async function waitForQueued(
  marker: string,
  taskId: string,
  api: ApiClient = client,
  opts: { tick?: boolean; timeoutMs?: number } = {},
): Promise<any> {
  const deadline = Date.now() + (opts.timeoutMs ?? 20_000);
  while (Date.now() < deadline) {
    // A tick-only server (--poll 0) advances nothing on its own, so drive it here.
    if (opts.tick) await api.POST("/tick", {});
    const items = await parkedInProcess(approval.name, api);
    // eslint-disable-next-line @typescript-eslint/no-explicit-any
    const hit = items.find((t) => t.task_id === taskId && (t.input as any)?.requester === marker);
    if (hit) return hit;
    await new Promise((r) => setTimeout(r, 100));
  }
  throw new Error(`external task ${taskId} for ${marker} was not queued in time`);
}

// waitForInstance, but for a tick-only server.
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

async function outputsOf(id: string, api: ApiClient = client): Promise<Record<string, any>> {
  const { data } = await api.GET("/instances/{id}/detail", { params: { path: { id } } });
  return ((data?.state as any)?.outputs ?? {}) as Record<string, any>;
}

test("examples/expense-approval: an approval submitted by queue token resumes the process and pays", async () => {
  const mock = await startExpenseService();
  try {
    await applyExample();
    // The requester doubles as this run's marker, so the queue entry is identifiable
    // without the queue ever exposing an instance id.
    const marker = `req-${crypto.randomUUID().slice(0, 8)}`;
    const id = await startApproval(mock.port, marker);

    const queued = await waitForQueued(marker, "review");
    // What the resolver can see: the snapshot and the contract for the answer. Not the
    // process context, and not the other tasks' outputs.
    expect(queued.input).toEqual({
      requester: marker,
      amount_cents: 4200,
      purpose: "conference ticket",
    });
    // The shape a worker must answer with is published by the CLAIM, not by discovery.
    const [claimed] = await claimInProcess(approval.name);
    expect(claimed.result_schema?.required).toEqual(["approved", "reviewer"]);

    const { error } = await client.POST("/external-tasks/resolve", {
      body: { token: queued.token, result: { approved: true, reviewer: "alice" } },
    });
    expect(error).toBeUndefined();

    expect(await waitForInstance(id, 30_000)).toBe("completed");
    const outputs = await outputsOf(id);
    expect(outputs.review).toEqual({ approved: true, decided_by: "alice" });
    expect(outputs.pay?.payment_id).toMatch(/^pay-/);
    // Only the reviewer was notified — the escalation path was never taken.
    expect(mock.notified()).toEqual(["reviewer"]);
  } finally {
    await mock.stop();
  }
}, 60_000);

test("examples/expense-approval: a rejection raises expense_rejected", async () => {
  const mock = await startExpenseService();
  try {
    await applyExample();
    const marker = `req-${crypto.randomUUID().slice(0, 8)}`;
    const id = await startApproval(mock.port, marker);
    await waitForQueued(marker, "review");

    // The second submission route: address the instance + task directly, no token.
    const { error } = await client.POST("/external-tasks/signal", {
      body: { instance_id: id, task_id: "review", result: { approved: false, reviewer: "bob" } },
    });
    expect(error).toBeUndefined();

    // `raised`, not `failed`: a rejected expense is an anticipated outcome a parent
    // process could catch, not a defect.
    expect(await waitForInstance(id, 30_000)).toBe("raised");
  } finally {
    await mock.stop();
  }
}, 60_000);

test("examples/expense-approval: a result that violates the result_schema is refused at the API", async () => {
  const mock = await startExpenseService();
  try {
    await applyExample();
    const marker = `req-${crypto.randomUUID().slice(0, 8)}`;
    const id = await startApproval(mock.port, marker);
    const queued = await waitForQueued(marker, "review");

    // `approved` must be a boolean and `reviewer` is required. The park is what makes
    // this checkable: the contract is published before the answer is written.
    const { error } = await client.POST("/external-tasks/resolve", {
      body: { token: queued.token, result: { approved: "yes" } },
    });
    expect(error).toBeDefined();

    // Still parked, so a valid answer can still arrive.
    const { data } = await client.GET("/instances/{id}/detail", { params: { path: { id } } });
    expect(data?.status).toBe("running");
  } finally {
    await mock.stop();
  }
}, 60_000);

// The timeout runs on a DEDICATED server: expiring a 1-hour review window means shifting
// the server clock, and the shared test server is used concurrently by every other file.
test("examples/expense-approval: an unreviewed expense times out, escalates, and the manager decides", async () => {
  const mock = await startExpenseService();
  const db = join(tmpdir(), `genroc_approval_${Date.now()}.db`);
  // --poll 0: /tick is rejected unless the server is in manual mode, and shifting the
  // clock is the only way to expire a 1-hour review window without waiting one.
  const genroc = await startGenroc(genrocBin, ESCALATE_PORT, db, undefined, 0);
  const api = genroc.client as ApiClient;
  try {
    await applyExample(api);
    const marker = `req-${crypto.randomUUID().slice(0, 8)}`;
    const id = await startApproval(mock.port, marker, api);

    await waitForQueued(marker, "review", api, { tick: true });

    // Nobody reviews it. Push the clock past the 1-hour window in the definition.
    await api.POST("/tick", { body: { advance_ms: 3_700_000 } });

    // external.timeout routed to `escalate`, which notified the manager and parked on a
    // second external task — this one with no timeout at all.
    const escalated = await waitForQueued(marker, "manager_review", api, { tick: true });
    expect(escalated.input.escalated).toBe(true);

    const { error } = await api.POST("/external-tasks/resolve", {
      body: { token: escalated.token, result: { approved: true, reviewer: "manager" } },
    });
    expect(error).toBeUndefined();

    expect(await waitForInstanceTicking(id, api)).toBe("completed");
    const outputs = await outputsOf(id, api);
    expect(outputs.manager_review).toEqual({ approved: true, decided_by: "manager" });
    // Both notifications fired, in order: the reviewer first, then the escalation.
    expect(mock.notified()).toEqual(["reviewer", "manager"]);
  } finally {
    genroc.stop();
    await mock.stop();
  }
}, 120_000);

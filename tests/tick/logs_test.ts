/**
 * End-to-end coverage for the per-instance execution audit trail
 * (GET /instances/{id}/logs).
 *
 * Runs in manual-tick mode with --immediate-retries (via useTickEnv) so retries
 * are claimable on the very next tick with no backoff wait.
 *
 * Covers:
 *   1. A successful run records work_started → action_started → action_succeeded →
 *      task_completed → inst_completed; action_succeeded carries the response
 *      body in data and the HTTP status in meta, work_started names the worker.
 *   2. A failing task with one retry records retry_scheduled (warn) then
 *      instance_failed; the level filter narrows to just the warn entry.
 *   3. Time-based pruning: advancing the clock past the retention window drops
 *      old log rows on the next tick.
 */
import { expect, test, beforeAll, afterAll } from "vitest";
import { startMockService } from "../helpers/client.ts";
import { useTickEnv } from "./helpers.ts";

const PORT = 20018;
const ctx = useTickEnv(PORT);

let okMockPort: number;
let failMockPort: number;
let stopOk: (() => Promise<void>) | undefined;
let stopFail: (() => Promise<void>) | undefined;
let okProc: string;
let failProc: string;

beforeAll(async () => {
  const uid = crypto.randomUUID().slice(0, 8);
  okProc = `logs_ok_${uid}`;
  failProc = `logs_fail_${uid}`;

  const okMock = await startMockService(0, { statusCode: 200, response: { ok: true } });
  okMockPort = okMock.port;
  stopOk = okMock.stop;

  const failMock = await startMockService(0, { statusCode: 500 });
  failMockPort = failMock.port;
  stopFail = failMock.stop;

  // Two-task happy path so task_completed (mid-process routing) also appears.
  await ctx.env.define(okProc, [
    {
      id: "first",
      action: {
        type: "fetch" as const,
        url: `http://localhost:${okMockPort}/action`,
        result_schema: {
          type: "object",
          properties: { ok: { type: "boolean" } },
        },
      },
      timeout: 5_000,
      switch: [{ goto: "$second" }],
    },
    {
      id: "second",
      action: {
        type: "fetch" as const,
        url: `http://localhost:${okMockPort}/action`,
      },
      timeout: 5_000,
      switch: [{ goto: "end" }],
    },
  ]);

  // One retry → two attempts, then permanent failure.
  await ctx.env.define(failProc, [
    {
      id: "work",
      action: {
        type: "fetch" as const,
        url: `http://localhost:${failMockPort}/action`,
      },
      on_error: [{ code: ["http.%"], retry: 1 }],
      timeout: 5_000,
      switch: [{ goto: "end" }],
    },
  ]);
}, 60_000);

afterAll(async () => {
  await stopOk?.();
  await stopFail?.();
});

async function getLogs(
  id: string,
  query?: { level?: "debug" | "info" | "warn" | "error"; recursive?: boolean },
) {
  // order=asc: the endpoint sorts newest-first like every list, while these tests assert
  // the event sequence in the order the engine produced it.
  const { data, error } = await ctx.env.client.GET("/instances/{id}/logs", {
    params: { path: { id }, query: { ...query, order: "asc" } },
  });
  if (error) throw new Error(`get logs failed: ${JSON.stringify(error)}`);
  return data!.items ?? [];
}

test("successful run records task and completion events with response snippet", async () => {
  const id = await ctx.env.start(okProc);
  await ctx.env.tickUntilIdle();
  expect(await ctx.env.status(id)).toBe("completed");

  const logs = await getLogs(id);
  const events = logs.map((l) => l.event);

  // Oldest-first ordering, full lifecycle of a two-task run. work_started marks a
  // worker picking the instance up (one per task, since a call checkpoints and
  // yields); action_started/action_succeeded are the request/response;
  // task_completed is the per-task routing; inst_created/completed bookend.
  expect(events).toEqual([
    "inst_created",
    "work_started",
    "action_started",
    "action_succeeded",
    "task_completed",
    "work_started",
    "action_started",
    "action_succeeded",
    "inst_completed",
  ]);

  // work_started is info and names the worker that picked it up.
  const started = logs.find((l) => l.event === "work_started");
  expect(started?.level).toBe("info");
  expect(started?.task).toBe("first");
  expect(String(started?.meta?.worker ?? "")).not.toBe("");

  // action_succeeded carries the raw (capped) response body in data and the HTTP
  // status in the structured meta.
  const firstSucceeded = logs.find((l) => l.event === "action_succeeded");
  expect(firstSucceeded?.task).toBe("first");
  expect(firstSucceeded?.data).toContain("ok");
  expect(firstSucceeded?.meta?.status).toBe(200);

  // action_started records the action type in message, the request body in data,
  // and the fetch url in meta (headers are intentionally not logged).
  const firstStarted = logs.find((l) => l.event === "action_started");
  expect(firstStarted?.message).toBe("fetch");
  expect(String(firstStarted?.meta?.url)).toContain("/action");
});

test("failing task records retry_scheduled then instance_failed; level filter narrows", async () => {
  const id = await ctx.env.start(failProc);
  await ctx.env.tickUntilIdle();
  expect(await ctx.env.status(id)).toBe("failed");

  const logs = await getLogs(id);
  const events = logs.map((l) => l.event);
  expect(events).toContain("retry_scheduled");
  expect(events).toContain("inst_failed");

  const retry = logs.find((l) => l.event === "retry_scheduled");
  expect(retry?.level).toBe("warn");
  expect(retry?.code).toMatch(/^http\./);
  expect(retry?.message).toContain("attempt 1/1");

  // action_failed (debug) captures the raw call failure separately: the http code,
  // the status in structured meta, and the error body in data.
  const failed = logs.find((l) => l.event === "action_failed");
  expect(failed?.level).toBe("debug");
  expect(failed?.code).toMatch(/^http\./);
  expect(failed?.meta?.status).toBe(500);

  // Level filter returns only the warn-level entry (action_failed is debug).
  const warns = await getLogs(id, { level: "warn" });
  expect(warns).toHaveLength(1);
  expect(warns[0].event).toBe("retry_scheduled");
});

// Must run last: pruning is global and the clock advance persists for this server.
test("clock advance past retention prunes old logs on the next tick", async () => {
  const id = await ctx.env.start(okProc);
  await ctx.env.tickUntilIdle();
  expect((await getLogs(id)).length).toBeGreaterThan(0);

  // Default retention is 168h; jump well past it, then tick (prune runs first).
  const { error } = await ctx.env.client.POST("/tick", {
    body: { advance_ms: 200 * 60 * 60 * 1000 },
  });
  if (error) throw new Error(`tick failed: ${JSON.stringify(error)}`);

  expect(await getLogs(id)).toHaveLength(0);
});

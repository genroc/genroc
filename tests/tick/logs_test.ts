/** The per-instance audit trail (GET /instances/{id}/logs), under manual ticks with --immediate-retries. */
import { expect, test, beforeAll, afterAll } from "vitest";
import { startMockService } from "../helpers/client.ts";
import { useTickEnv } from "./helpers.ts";

const ctx = useTickEnv();

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
        method: "post",
        url: `http://localhost:${okMockPort}/action`,
        responses: { 200: {
          type: "object",
          properties: { ok: { type: "boolean" } },
        } },
        timeout: 5_000,
      },
      switch: [{ goto: "$second" }],
    },
    {
      id: "second",
      action: {
        type: "fetch" as const,
        method: "post",
        url: `http://localhost:${okMockPort}/action`,
        timeout: 5_000,
      },
      switch: [{ goto: "end" }],
    },
  ]);

  // One retry → two attempts, then permanent failure.
  await ctx.env.define(failProc, [
    {
      id: "work",
      action: {
        type: "fetch" as const,
        method: "post",
        url: `http://localhost:${failMockPort}/action`,
        timeout: 5_000,
      },
      on_error: [{ code: ["http.%"], retry: 1 }],
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

  // One work_started per task, since a call checkpoints and yields.
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

  // work_started is debug -- one per advance, and which worker holds a row is a question
  // about the engine rather than about the run -- and it names that worker.
  const started = logs.find((l) => l.event === "work_started");
  expect(started?.level).toBe("debug");
  expect(started?.task).toBe("first");
  expect(String(started?.meta?.worker ?? "")).not.toBe("");

  // action_succeeded carries the body in data (as a value) and the HTTP status in meta. Info, since
  // a call's request and response are the task's work, not engine bookkeeping.
  const firstSucceeded = logs.find((l) => l.event === "action_succeeded");
  expect(firstSucceeded?.level).toBe("info");
  expect(logs.find((l) => l.event === "action_started")?.level).toBe("info");
  expect(firstSucceeded?.task).toBe("first");
  expect(firstSucceeded?.data).toEqual({ ok: true });
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
  expect(retry?.message).toContain("retry 1/1");

  // action_failed is warn, so why a run failed is in its default view.
  const failed = logs.find((l) => l.event === "action_failed");
  expect(failed?.level).toBe("warn");
  expect(failed?.code).toMatch(/^http\./);
  expect(failed?.meta?.status).toBe(500);

  // The level filter is a floor: warn drops the debug work_started below it and keeps
  // everything at warn or above, so a caller asking about trouble is never shown less of it.
  const fromWarn = await getLogs(id, { level: "warn" });
  expect(fromWarn.map((l) => l.event)).toContain("retry_scheduled");
  expect(fromWarn.map((l) => l.event)).toContain("action_failed");
  expect(fromWarn.map((l) => l.event)).not.toContain("work_started");
  expect(fromWarn.every((l) => l.level === "warn" || l.level === "error")).toBe(true);
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

/** A task already executing when a cancel arrives runs to completion: the cancel is only RECORDED
 *  ('cancelling') and lands on that task's own write. One tick runs one task, so each step is checkable. */
import { expect, test, beforeAll, afterAll } from "vitest";
import { startMockService } from "../helpers/client.ts";
import { useTickEnv } from "./helpers.ts";

const ctx = useTickEnv();

let stopMocks: (() => Promise<void>) | undefined;
afterAll(() => stopMocks?.());

// A two-task process whose FIRST task hangs until the returned mock is released.
async function heldProcess(name: string, opts: { firstStatus?: number } = {}) {
  const held = await startMockService(0, {
    response: { ok: true },
    statusCode: opts.firstStatus ?? 200,
    firstRequestDelayMs: Infinity,
  });
  const next = await startMockService(0, { response: { done: true } });
  const prev = stopMocks;
  stopMocks = async () => {
    await prev?.();
    await held.stop();
    await next.stop();
  };

  await ctx.env.define(name, [
    {
      id: "held",
      action: { type: "fetch" as const, method: "post", url: `http://localhost:${held.port}/action`, timeout: 30_000 },
      switch: [{ goto: "next" }],
    },
    {
      id: "after",
      action: { type: "fetch" as const, method: "post", url: `http://localhost:${next.port}/action`, timeout: 5_000 },
      switch: [{ goto: "end" }],
    },
  ]);
  return { held, next };
}

test("cancel waits for an in-flight fetch, then lands — and the next task never runs", async () => {
  const name = `drain_${crypto.randomUUID().slice(0, 8)}`;
  const { held, next } = await heldProcess(name);
  const id = await ctx.env.start(name);

  // Not awaited: this tick claims the instance and blocks inside the fetch, which is the
  // only state in which a cancel has something to wait for.
  const inFlight = ctx.env.tick();
  await held.firstRequestReceived;

  // Recorded, not applied. The request is already out and genroc cannot know whether it
  // took effect, so the row is leased and the cancel can only be asked for.
  expect(await ctx.env.cancel(id)).toBe("accepted");
  expect(await ctx.env.status(id)).toBe("cancelling");
  expect(held.requestCount(), "the in-flight request is not aborted").toBe(1);

  // Let the held task finish. Its own write releases the lease, and the landing CASE turns
  // 'cancelling' into 'cancelled' on the way past.
  held.release();
  expect(await inFlight).toBe(1);
  expect(await ctx.env.status(id)).toBe("cancelled");

  // The drain finished the task it was holding and stopped there: `after` is the task the
  // cancel stood in front of, and it must never run.
  expect(next.requestCount(), "the next task must not run after a cancel").toBe(0);
  expect(await ctx.env.tick()).toBe(0);
  expect(await ctx.env.status(id)).toBe("cancelled");
  expect(next.requestCount()).toBe(0);
});

// The landing CASE fires only when the task's own write says 'running', so a terminal failure
// under a pending cancel stands as the outcome.
test("a task that fails terminally while cancelling reports the failure, not the cancel", async () => {
  const name = `drain_fail_${crypto.randomUUID().slice(0, 8)}`;
  const { held, next } = await heldProcess(name, { firstStatus: 500 });
  const id = await ctx.env.start(name);

  const inFlight = ctx.env.tick();
  await held.firstRequestReceived;
  expect(await ctx.env.cancel(id)).toBe("accepted");
  expect(await ctx.env.status(id)).toBe("cancelling");

  held.release();
  await inFlight;

  // No on_error and no retry budget, so the 500 is terminal for the instance.
  expect(await ctx.env.status(id)).toBe("failed");
  expect(next.requestCount()).toBe(0);
  expect(await ctx.env.tick()).toBe(0);
});

// A retry writes 'running' with a wake_at, so the landing CASE fires and the cancel wins;
// otherwise the row would keep re-arming and never stop.
test("a cancel breaks a retry loop rather than being re-armed by it", async () => {
  const boom = await startMockService(0, { statusCode: 500, firstRequestDelayMs: Infinity });
  const prev = stopMocks;
  stopMocks = async () => {
    await prev?.();
    await boom.stop();
  };

  const name = `drain_retry_${crypto.randomUUID().slice(0, 8)}`;
  await ctx.env.define(name, [
    {
      id: "call",
      action: { type: "fetch" as const, method: "post", url: `http://localhost:${boom.port}/boom`, timeout: 30_000 },
      on_error: [{ code: ["http.5%"], retry: { retries: 10, delay: "1s" } }],
      switch: "end",
    },
  ]);
  const id = await ctx.env.start(name);

  const inFlight = ctx.env.tick();
  await boom.firstRequestReceived;
  expect(await ctx.env.cancel(id)).toBe("accepted");
  expect(await ctx.env.status(id)).toBe("cancelling");

  boom.release();
  await inFlight;

  // 9 attempts still budgeted, and none of them will happen.
  expect(await ctx.env.status(id)).toBe("cancelled");

  // Past the retry timer: a cancelled row is outside the runnable index and the claim
  // predicate both, so the re-arm that would otherwise be due here never comes.
  await ctx.env.client.POST("/tick", { body: { advance_ms: 5_000 } });
  expect(await ctx.env.status(id)).toBe("cancelled");
  expect(boom.requestCount(), "no retry may run after a cancel").toBe(1);
});

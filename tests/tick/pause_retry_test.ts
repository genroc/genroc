/**
 * Tests that observe how pausing interacts with on_error retries.
 *
 * The point of these is the difference between resuming and retrying. Pausing an
 * instance that is sitting on a scheduled retry does not spend, skip or reset that
 * retry — the attempt the definition granted is still there when the process resumes.
 * Retrying, by contrast, is only for a process that already failed, and hands it an
 * attempt beyond its on_error budget.
 *
 * Three scenarios:
 *
 *   1. Pause issued while a retry is still pending
 *      → the instance parks in 'paused' with its retry intact; ticks do nothing while
 *        it is paused; resuming runs exactly that pending attempt.
 *
 *   2. All retries exhausted before any pause
 *      → the final failed attempt calls failInstance(); status is 'failed', and
 *        pausing a settled process is rejected.
 *
 *   3. retry on a paused process is rejected
 *      → the two verbs are not interchangeable; the error points at resume.
 *
 * Both processes run as root instances (no tree) so the timing is straightforward.
 * The server is started with --immediate-retries so retries are claimable on the
 * very next tick, with no backoff delay to wait for.
 */
import { expect, test, beforeAll, afterAll } from "vitest";
import { startMockService } from "../helpers/client.ts";
import { useTickEnv } from "./helpers.ts";

const PORT = 20016;
const ctx = useTickEnv(PORT);

let failMockPort: number;
let stopMock: (() => Promise<void>) | undefined;
let withRetriesName: string;
let exhaustedName: string;

beforeAll(async () => {
  const uid = crypto.randomUUID().slice(0, 8);
  withRetriesName = `with_retries_${uid}`;
  exhaustedName = `exhausted_${uid}`;

  const failMock = await startMockService(0, { statusCode: 500 });
  failMockPort = failMock.port;
  stopMock = failMock.stop;

  // Process with 2 retries — three total attempts before permanent failure.
  await ctx.env.define(withRetriesName, [
    {
      id: "work",
      action: {
        type: "fetch" as const,
        url: `http://localhost:${failMockPort}/action`,
      },
      on_error: [{ code: ["http.%"], retry: 2 }],
      timeout: 5_000,
      switch: [{ goto: "end" }],
    },
  ]);

  // Process with 1 retry — two total attempts, then permanent failure.
  await ctx.env.define(exhaustedName, [
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

afterAll(() => stopMock?.());

test("pause while a retry is pending — the retry waits, and resume runs it", async () => {
  const id = await ctx.env.start(withRetriesName);
  try {
    // tick: attempt 1 fails → retry scheduled (status stays 'running', wake_at set)
    await ctx.env.tick();
    expect(await ctx.env.status(id)).toBe("running");
    expect(await ctx.env.retryCount(id)).toBe(1);

    // Pause while the retry timer is counting down. The instance is not leased
    // between ticks, so it goes straight to 'paused' with no draining step.
    await ctx.env.pause(id);
    expect(await ctx.env.status(id)).toBe("paused");

    // The retry backoff is 0 (--immediate-retries), so this instance would be
    // claimable right now if it were running. Paused, it is not: ticking does not
    // fire the pending attempt, and the retry budget is left exactly as it was.
    await ctx.env.tick();
    expect(await ctx.env.status(id)).toBe("paused");
    expect(await ctx.env.retryCount(id)).toBe(1);

    // Resume puts it straight back to running, still holding that pending attempt.
    await ctx.env.resume(id);
    expect(await ctx.env.status(id)).toBe("running");

    // tick: the retry the pause was holding finally fires (attempt 2) and fails,
    // which schedules the last attempt the definition allows.
    await ctx.env.tick();
    expect(await ctx.env.status(id)).toBe("running");
    expect(await ctx.env.retryCount(id)).toBe(2);
  } finally {
    await ctx.env.tickUntilIdle();
  }
});

test("retries exhausted — process fails; pausing a settled process is rejected", async () => {
  const id = await ctx.env.start(exhaustedName);
  try {
    // tick: attempt 1 fails → retry 1 scheduled (retry: 1, so one more attempt allowed)
    await ctx.env.tick();
    expect(await ctx.env.status(id)).toBe("running");

    // tick: no backoff, attempt 2 fires immediately and fails.
    // RetryCount now equals Retries — no more retries available → failInstance.
    await ctx.env.tick();
    expect(await ctx.env.status(id)).toBe("failed");

    // There is nothing running left to suspend, so the pause is refused rather
    // than silently doing nothing.
    await expect(ctx.env.pause(id)).rejects.toThrow(/no running instances/);
    expect(await ctx.env.status(id)).toBe("failed");
  } finally {
    await ctx.env.tickUntilIdle();
  }
});

test("retry is rejected on a paused process — it points at resume instead", async () => {
  const id = await ctx.env.start(withRetriesName);
  try {
    await ctx.env.tick();
    await ctx.env.pause(id);
    expect(await ctx.env.status(id)).toBe("paused");

    // Retry exists to grant a failed process an attempt beyond its on_error budget.
    // A paused process has not failed and is owed nothing, so the two are not
    // interchangeable.
    await expect(ctx.env.retry(id)).rejects.toThrow(/paused, not failed/);
    expect(await ctx.env.status(id)).toBe("paused");

    await ctx.env.resume(id);
    expect(await ctx.env.status(id)).toBe("running");
  } finally {
    await ctx.env.tickUntilIdle();
  }
});

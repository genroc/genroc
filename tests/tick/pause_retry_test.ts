/** Pausing on a scheduled retry neither spends, skips nor resets it; retry is only for a failed process.
 *  The server runs --immediate-retries, so a pending retry is claimable on the very next tick. */
import { expect, test, beforeAll, afterAll } from "vitest";
import { startMockService } from "../helpers/client.ts";
import { useTickEnv } from "./helpers.ts";

const ctx = useTickEnv();

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
        method: "post",
        url: `http://localhost:${failMockPort}/action`,
        timeout: 5_000,
      },
      on_error: [{ code: ["http.%"], retry: 2 }],
      switch: [{ goto: "end" }],
    },
  ]);

  // Process with 1 retry — two total attempts, then permanent failure.
  await ctx.env.define(exhaustedName, [
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

    // Backoff is 0 (--immediate-retries), so only the pause keeps the attempt from firing here.
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

test("retries exhausted — process fails; pausing a settled process changes nothing", async () => {
  const id = await ctx.env.start(exhaustedName);
  try {
    // tick: attempt 1 fails → retry 1 scheduled (retry: 1, so one more attempt allowed)
    await ctx.env.tick();
    expect(await ctx.env.status(id)).toBe("running");

    // tick: no backoff, attempt 2 fires immediately and fails.
    // RetryCount now equals Retries — no more retries available → failInstance.
    await ctx.env.tick();
    expect(await ctx.env.status(id)).toBe("failed");

    // Nothing running left to suspend — which is what pause asserts, so it reports
    // `unchanged` rather than failing, and touches nothing. specs/id-list-commands.md.
    expect(await ctx.env.pause(id)).toBe("unchanged");
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

    // A paused process has not failed, so it is owed no extra attempt.
    await expect(ctx.env.retry(id)).rejects.toThrow(/paused, not failed/);
    expect(await ctx.env.status(id)).toBe("paused");

    await ctx.env.resume(id);
    expect(await ctx.env.status(id)).toBe("running");
  } finally {
    await ctx.env.tickUntilIdle();
  }
});

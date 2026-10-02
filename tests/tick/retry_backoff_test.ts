/** The authored retry curve is the one the engine parks on. Real backoff and a shifted clock; delays are
 *  minutes, far outside the default curve, so "still parked" proves the policy was read. */
import { expect, test, beforeAll, afterAll } from "vitest";
import { useTickEnv } from "./helpers.ts";
import { tick, startMockService } from "../helpers/client.ts";

const ctx = useTickEnv({ immediateRetries: false });

let mock: Awaited<ReturnType<typeof startMockService>>;
beforeAll(async () => {
  mock = await startMockService(0, { statusCode: 500 });
});
afterAll(async () => await mock?.stop());

async function defineFailing(name: string, retry: unknown) {
  await ctx.env.define(name, [
    {
      id: "call",
      action: { type: "fetch", method: "post", url: `http://localhost:${mock.port}/boom` },
      on_error: [{ code: ["http.5%"], retry }],
      switch: "end",
    },
  ]);
}

// max-concurrent 1, and earlier tests' instances are still parked on their own timers: drain so the
// instance under test is reached whatever is queued ahead.
async function advanceAndDrain(ms: number) {
  await tick(ctx.env.client, ms);
  await ctx.env.tickUntilIdle();
}

test("an authored delay parks past the default curve", async () => {
  const name = `retry_slow_${crypto.randomUUID()}`;
  await defineFailing(name, { retries: 2, delay: "10m", factor: 1 });
  const id = await ctx.env.start(name);

  await ctx.env.tickUntilIdle();
  expect(await ctx.env.retryCount(id)).toBe(1);

  // One minute is past every delay the default curve can produce (1s, then 2s) and short
  // of every delay this one can, so a retry firing here means the policy was ignored.
  await advanceAndDrain(60_000);
  expect(await ctx.env.retryCount(id)).toBe(1);

  await advanceAndDrain(11 * 60_000);
  expect(await ctx.env.retryCount(id)).toBe(2);
});

test("factor 1 keeps the delay constant across attempts", async () => {
  const name = `retry_constant_${crypto.randomUUID()}`;
  await defineFailing(name, { retries: 3, delay: "1m", factor: 1, max_delay: "1h" });
  const id = await ctx.env.start(name);

  await ctx.env.tickUntilIdle();
  // Jitter only shortens, so a constant 1m curve releases every attempt here; the default factor
  // would leave the third (4m nominal) parked.
  for (const attempt of [2, 3]) {
    await advanceAndDrain(70_000);
    expect(await ctx.env.retryCount(id)).toBe(attempt);
  }
});

test("max_delay caps a curve that would otherwise outgrow it", async () => {
  const name = `retry_capped_${crypto.randomUUID()}`;
  // Un-capped, the third wait would be 16m; the cap holds every wait at ≤ 5m.
  await defineFailing(name, {
    retries: 3,
    delay: "1m",
    factor: 4,
    max_delay: "5m",
  });
  const id = await ctx.env.start(name);

  await ctx.env.tickUntilIdle();
  for (const attempt of [2, 3]) {
    await advanceAndDrain(6 * 60_000);
    expect(await ctx.env.retryCount(id)).toBe(attempt);
  }
});

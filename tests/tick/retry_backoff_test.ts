/**
 * Tests that an authored retry curve is the one the engine parks on.
 *
 * Runs with the real backoff (immediateRetries: false) and moves the server clock instead
 * of waiting, so the assertions are about the scheduled wake-up rather than wall time.
 * The delays here are minutes, far outside anything the default curve produces, which is
 * what makes "still parked" evidence that the policy was read at all.
 */
import { expect, test, beforeAll, afterAll } from "vitest";
import { useTickEnv } from "./helpers.ts";
import { tick, startMockService } from "../helpers/client.ts";

const PORT = 20044;
const ctx = useTickEnv(PORT, { immediateRetries: false });

let mock: Awaited<ReturnType<typeof startMockService>>;
beforeAll(async () => {
  mock = await startMockService(0, { statusCode: 500 });
});
afterAll(async () => await mock?.stop());

async function defineFailing(name: string, retry: unknown) {
  await ctx.env.define(name, [
    {
      id: "call",
      action: { type: "fetch", url: `http://localhost:${mock.port}/boom` },
      on_error: [{ code: ["http.5%"], retry }],
      switch: "end",
    },
  ]);
}

// The server runs at max-concurrent 1, so one tick advances one instance and the earlier
// tests' instances are still parked on retry timers of their own. Advance the clock once,
// then tick until nothing more is claimable, so the instance under test is reached
// whatever else is queued ahead of it.
async function advanceAndDrain(ms: number) {
  await tick(ctx.env.client, ms);
  await ctx.env.tickUntilIdle();
}

test("an authored delay parks past the default curve", async () => {
  const name = `retry_slow_${crypto.randomUUID()}`;
  await defineFailing(name, { attempts: 2, delay: "10m", factor: 1 });
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
  await defineFailing(name, { attempts: 3, delay: "1m", factor: 1, max_delay: "1h" });
  const id = await ctx.env.start(name);

  await ctx.env.tickUntilIdle();
  // Jitter only shortens, so a constant curve never waits more than its 1m base. The same
  // advance therefore releases every attempt — under the default factor the third wait
  // would be 4m nominal and this would leave it parked.
  for (const attempt of [2, 3]) {
    await advanceAndDrain(70_000);
    expect(await ctx.env.retryCount(id)).toBe(attempt);
  }
});

test("max_delay caps a curve that would otherwise outgrow it", async () => {
  const name = `retry_capped_${crypto.randomUUID()}`;
  // Un-capped, the third wait would be 16m; the cap holds every wait at ≤ 5m.
  await defineFailing(name, {
    attempts: 3,
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

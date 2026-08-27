/**
 * The one §5.5 claim that only the shiftable clock can prove: a replacement's `wake_at` is
 * measured from the raised attempt's OWN conclusion, not from the moment the parent got round
 * to dispatching the round.
 *
 * The batch is the unit, so a slot can sit settled while its siblings finish — and that
 * wall-clock already served what a backoff is for. Anchoring at dispatch would charge the wait
 * twice. Here the gap is manufactured by moving the server clock between the child raising and
 * the parent resolving, which makes the two anchors land half an hour apart.
 */
import { expect, test } from "vitest";
import { useTickEnv } from "./helpers.ts";
import { tick } from "../helpers/client.ts";

const PORT = 20047;
const ctx = useTickEnv(PORT, { immediateRetries: false });

const MINUTE = 60_000;

test("the retry delay runs from the failure, not from the dispatch", async () => {
  const uid = crypto.randomUUID().slice(0, 8);
  const child = `anchor_child_${uid}`;
  const parent = `anchor_parent_${uid}`;

  await ctx.env.define(child, [
    { id: "go", switch: [{ raise: { code: "svc_down", message: "down" } }] },
  ]);
  await ctx.env.define(parent, [
    {
      id: "call",
      action: { type: "child", name: child },
      // factor 1 so the wait is flat; jitter only ever SHORTENS, into [delay/2, delay].
      on_error: [{ code: ["svc_down"], retry: { attempts: 1, delay: "10m", factor: 1 } }],
      switch: [{ goto: "end" }],
    },
  ]);

  const id = await ctx.env.start(parent);

  // Tick only until the child has raised — the parent has not resolved the batch yet.
  const raisedRow = () =>
    ctx.env.query<{ id: string; updated_at: number; status: string }>(
      "SELECT id, updated_at, status FROM process_instances WHERE parent_id = ? AND status = 'raised'",
      id,
    )[0];
  for (let i = 0; i < 10 && !raisedRow(); i++) await ctx.env.tick();
  const failed = raisedRow();
  expect(failed, "the child should have raised before the parent resolves").toBeTruthy();

  // Half an hour passes before the parent gets to its collect. Under the batch barrier this
  // is exactly the sibling wait the anchoring exists to absorb.
  await tick(ctx.env.client, 30 * MINUTE);

  // Tick only until the replacement EXISTS, then read it: it concludes by raising again, and
  // a raise clears wake_at, so the timer is observable for exactly this one window.
  const pendingReplacement = () =>
    ctx.env.query<{ wake_at: number }>(
      "SELECT wake_at FROM process_instances WHERE parent_id = ? AND superseded_at IS NULL AND wake_at IS NOT NULL",
      id,
    )[0];
  // Checked BEFORE ticking again: the clock-shift call is itself a tick, so the dispatch may
  // already have happened, and one more tick would run the replacement and clear its timer.
  let replacement = pendingReplacement();
  for (let i = 0; i < 10 && !replacement; i++) {
    await ctx.env.tick();
    replacement = pendingReplacement();
  }
  expect(replacement, "the raised slot should have been re-spawned with a timer").toBeTruthy();

  const waitedFromFailure = Number(replacement.wake_at) - Number(failed.updated_at);
  // Anchored at the failure: [5m, 10m] once jitter has shortened it.
  // Anchored at the dispatch it would be [35m, 40m] — the half hour charged a second time.
  expect(waitedFromFailure).toBeGreaterThanOrEqual(5 * MINUTE);
  expect(
    waitedFromFailure,
    `wake_at sits ${Math.round(waitedFromFailure / MINUTE)}m after the raise; anchoring at dispatch would put it past 30m`,
  ).toBeLessThanOrEqual(10 * MINUTE);
});

import { expect, test } from "vitest";
import { useTickEnv } from "./helpers.ts";
import { startMockService } from "../helpers/client.ts";

// Timeout behaviour that needs a controllable clock: deadlines already in the past, re-arm
// budgets across a retry, and the clock-offset trap. Driven in manual-tick mode.
const ctx = useTickEnv(20041);

const advance = (ms: number) => ctx.env.client.POST("/tick", { body: { advance_ms: ms } });

// A deadline behind now is legitimate — a re-arm after a retry, or a resume from a pause
// that outlasted the window — so an external clamps it and raises the code the definition
// is written against. Failing instead would hand an author who wrote
// `on_error: [external.timeout]` an uncatchable engine.expression.
test("an external until already past raises external.timeout, not an engine failure", async () => {
  await ctx.env.define("ext_past_until", [
    {
      id: "approval",
      action: { type: "external" },
      // A bare number is unix ms: November 2023, long behind any clock this runs on.
      timeout: { until: 1700000000000 },
      on_error: [{ code: ["external.timeout"], goto: "$handler" }],
      switch: "end",
    },
    { id: "handler", switch: [{ raise: { code: "expired", message: "window closed" } }] },
  ] as never);

  const id = await ctx.env.start("ext_past_until");
  await ctx.env.tickUntilIdle();
  // 'raised' is reachable only through the on_error route; a task that failed to arm would
  // be 'failed', and one that armed and never fired would still be parked.
  expect(await ctx.env.status(id), "a past deadline must route through external.timeout").toBe(
    "raised",
  );
});

// The counterpart on the fetch side, where the same clamp would be a lie: an expired context
// reports http.timeout for a request that was never sent, and http.timeout is unknowable —
// unretryable forever on an only_once task. So it is refused, and the catch-all cannot save
// it because the fault is the definition's, not the call's.
test("a fetch timeout resolving into the past fails rather than reporting a timeout", async () => {
  const mock = await startMockService(0, { response: { ok: true } });
  try {
    await ctx.env.define("fetch_past_timeout", [
      {
        id: "call",
        action: { type: "fetch", url: `http://localhost:${mock.port}/action` },
        timeout: 0,
        on_error: [{ goto: "$handled" }],
        switch: "end",
      },
      { id: "handled", switch: "end" },
    ] as never);

    const id = await ctx.env.start("fetch_past_timeout");
    await ctx.env.tickUntilIdle();
    expect(await ctx.env.status(id), "a zero budget is a definition bug, not a timeout").toBe(
      "failed",
    );
    expect(mock.requestCount(), "the request must never have been sent").toBe(0);
  } finally {
    await mock.stop();
  }
});

// A fetch deadline is resolved against db.Now(), which carries the test clock offset, but a
// context deadline is compared against the real time.Now(). Handing the resolved instant to
// context.WithDeadline would stretch every timeout by the offset — here by an hour, so the
// mock's 3s response would win and the task would succeed. Converting to a duration first
// cancels the offset out. This test is the only thing standing between that bug and a
// timeout that silently stops applying under an advanced clock.
test("a fetch timeout is not stretched by the test clock offset", async () => {
  const mock = await startMockService(0, { response: { ok: true }, firstRequestDelayMs: 3_000 });
  try {
    await advance(3_600_000);

    await ctx.env.define("fetch_offset_timeout", [
      {
        id: "call",
        action: { type: "fetch", url: `http://localhost:${mock.port}/action` },
        timeout: "300ms",
        on_error: [{ code: ["http.timeout"], goto: "$handler" }],
        switch: "end",
      },
      { id: "handler", switch: [{ raise: { code: "timed_out", message: "deadline fired" } }] },
    ] as never);

    const id = await ctx.env.start("fetch_offset_timeout");
    await ctx.env.tickUntilIdle();
    expect(
      await ctx.env.status(id),
      "300ms must stay 300ms however far the DB clock has been advanced",
    ).toBe("raised");
  } finally {
    await mock.stop();
  }
});

// An external timeout is resolved once per arm, so a re-arm after an external.timeout retry
// starts a fresh `for` budget rather than inheriting the spent one. If it did not re-resolve,
// the second arm would be due immediately and the retry would burn without any waiting.
test("a for budget restarts on re-arm after an external.timeout retry", async () => {
  await ctx.env.define("ext_rearm", [
    {
      id: "approval",
      action: { type: "external" },
      timeout: "1h",
      on_error: [{ code: ["external.timeout"], retry: 1 }],
      switch: "end",
    },
  ] as never);

  const id = await ctx.env.start("ext_rearm");
  await ctx.env.tick();
  expect(await ctx.env.status(id)).toBe("running external");

  // First window expires. The retry does not re-arm in the same tick — it waits out its
  // backoff first — so the clock is nudged past that before the task parks again.
  await advance(3_600_001);
  await advance(5_000);
  expect(await ctx.env.status(id), "the retry must re-arm, not resolve").toBe("running external");

  // Proof the new window is real: well short of an hour, nothing fires.
  await advance(60_000);
  expect(await ctx.env.status(id), "a fresh budget must not be already spent").toBe(
    "running external",
  );

  // And it does expire on its own schedule, exhausting the retry budget.
  await advance(3_600_001);
  await ctx.env.tickUntilIdle();
  expect(await ctx.env.status(id)).toBe("failed");
});

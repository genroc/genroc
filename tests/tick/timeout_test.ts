import { expect, test } from "vitest";
import { useTickEnv } from "./helpers.ts";
import { startMockService } from "../helpers/client.ts";

// Timeout behaviour that needs a controllable clock: deadlines already in the past, re-arm
// budgets across a retry, and the clock-offset trap. Driven in manual-tick mode.
const ctx = useTickEnv();

const advance = (ms: number) => ctx.env.client.POST("/tick", { body: { advance_ms: ms } });

// A past deadline is legitimate (a re-arm after retry, a long pause), so external clamps it;
// failing would hand `on_error: [external.timeout]` an uncatchable engine.expression.
test("an external until already past raises external.timeout, not an engine failure", async () => {
  await ctx.env.define("ext_past_until", [
    {
      id: "approval",
      // A bare number is unix ms: November 2023, long behind any clock this runs on.
      action: { type: "external", timeout: { until: 1700000000000 } },
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

// Clamping here would report http.timeout for a request never sent, and http.timeout is unknowable
// (unretryable on only_once). The fault is the definition's, so no catch-all saves it.
test("a fetch timeout resolving into the past fails rather than reporting a timeout", async () => {
  const mock = await startMockService(0, { response: { ok: true } });
  try {
    await ctx.env.define("fetch_past_timeout", [
      {
        id: "call",
        action: { type: "fetch", method: "post", url: `http://localhost:${mock.port}/action`, timeout: 0 },
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

// The deadline resolves against db.Now() (with the test-clock offset) but a context compares real
// time.Now(), so it must become a duration first or every timeout stretches by the offset.
test("a fetch timeout is not stretched by the test clock offset", async () => {
  const mock = await startMockService(0, { response: { ok: true }, firstRequestDelayMs: 3_000 });
  try {
    await advance(3_600_000);

    await ctx.env.define("fetch_offset_timeout", [
      {
        id: "call",
        action: { type: "fetch", method: "post", url: `http://localhost:${mock.port}/action`, timeout: "300ms" },
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

// Resolved once per arm: without re-resolving, the second arm would be due immediately.
test("a for budget restarts on re-arm after an external.timeout retry", async () => {
  await ctx.env.define("ext_rearm", [
    {
      id: "approval",
      action: { type: "external", timeout: "1h" },
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

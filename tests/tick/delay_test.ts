import { expect, test } from "vitest";
import { useTickEnv } from "./helpers.ts";

// `delay` parks by stamping next_retry_at and resumes once the server clock passes it. `for` is
// bare milliseconds here; the literal grammars are covered by internal/delayspec.
const ctx = useTickEnv();

// delay_armed rows read "<spec> -> <RFC3339 wake>", timestamped at arming. They flush on a 5ms
// ticker, so poll for `want`.
async function armLogs(id: string, want: number) {
  for (let attempt = 0; ; attempt++) {
    // order=asc: the endpoint defaults newest-first, and arms.at(-1) must be the latest arm.
    const { data, error } = await ctx.env.client.GET("/instances/{id}/logs", {
      params: { path: { id }, query: { limit: 100, order: "asc" } },
    });
    if (error) throw new Error(`get logs failed: ${JSON.stringify(error)}`);
    const arms = (data!.items ?? [])
      .filter((l) => l.event === "delay_armed")
      .map((l) => {
        const [spec, target] = (l.message ?? "").split(" -> ");
        return {
          spec: spec!,
          at: new Date(l.created_at!).getTime(),
          target: new Date(target!).getTime(),
        };
      });
    if (arms.length >= want || attempt >= 50) return arms;
    await new Promise((r) => setTimeout(r, 10));
  }
}

test("delay parks the instance until the clock advances past ms", async () => {
  await ctx.env.define("delay_done", [
    { id: "wait", action: { type: "delay", for: 60000 }, switch: "end" },
  ]);
  const id = await ctx.env.start("delay_done");

  // First tick arms the delay; the instance parks (running, timer in the future).
  expect(await ctx.env.tick()).toBe(1);
  expect(await ctx.env.status(id)).toBe("running");

  // While parked it is not claimable — a plain tick processes nothing.
  expect(await ctx.env.tick()).toBe(0);
  expect(await ctx.env.status(id)).toBe("running");

  // Advancing the clock past ms makes it claimable; it resumes and completes.
  await ctx.env.client.POST("/tick", { body: { advance_ms: 60000 } });
  expect(await ctx.env.status(id)).toBe("completed");
});

// Bracketing 2h30m a minute either side rules out zero, a dropped "30m", or "m" read as ms,
// without racing the real time that elapses between arming and the advance.
test("a `for` literal resolves to its stated duration", async () => {
  await ctx.env.define("delay_literal", [
    { id: "wait", action: { type: "delay", for: "2h30m" }, switch: "end" },
  ]);
  const id = await ctx.env.start("delay_literal");

  expect(await ctx.env.tick()).toBe(1); // arm
  expect(await ctx.env.status(id)).toBe("running");

  // A minute short of 2h30m it is still parked — so it is not 0, not 2h, not 2h+30ms.
  await ctx.env.client.POST("/tick", { body: { advance_ms: 2 * 3600_000 + 29 * 60_000 } });
  expect(await ctx.env.status(id)).toBe("running");

  // Crossing 2h30m makes it due — so it is not appreciably longer either.
  await ctx.env.client.POST("/tick", { body: { advance_ms: 2 * 60_000 } });
  expect(await ctx.env.status(id)).toBe("completed");
});

// Alignment is invisible to status, so it is asserted on the delay_armed log: on the grid, and
// within one period of the arm.
test("a stepped `until` arms on the field's grid, not on the arm time", async () => {
  await ctx.env.define("delay_step", [
    { id: "wait", action: { type: "delay", until: "*:*:0/5" }, switch: "end" },
  ]);
  const id = await ctx.env.start("delay_step");

  const wallBeforeArm = Date.now();
  expect(await ctx.env.tick()).toBe(1); // arm
  expect(await ctx.env.status(id)).toBe("running");

  const [armed] = await armLogs(id, 1);
  expect(armed, "the delay should have logged where it armed").toBeDefined();
  expect(armed!.spec).toBe("*:*:0/5");

  const at = new Date(armed!.target);
  expect(at.getSeconds() % 5, `armed at ${at.toISOString()}, off the five-second grid`).toBe(0);
  expect(at.getMilliseconds()).toBe(0);

  // Within one period of the arm time: a five-second schedule that resolved a minute or an
  // hour out would still be "on the grid".
  const wait = armed!.target - armed!.at;
  expect(wait).toBeGreaterThan(0);
  expect(wait).toBeLessThanOrEqual(5000);

  // The server clock is real time plus the /tick offset, so an arm just short of a grid point
  // may already be due; assert "still parked" only while the point is demonstrably ahead.
  if (wait - (Date.now() - wallBeforeArm) > 1000) {
    expect(await ctx.env.tick()).toBe(0);
  }
  await ctx.env.client.POST("/tick", { body: { advance_ms: 5000 } });
  expect(await ctx.env.status(id)).toBe("completed");
});

// Drift is a property of the loop: each arm can be right and the schedule still walk off the
// grid once the task re-arms behind its own runtime.
test("re-arming in a loop stays on the grid instead of drifting", async () => {
  await ctx.env.define("delay_loop", [
    {
      id: "wait",
      action: { type: "delay", until: "*:*:0/5" },
      output: { count: "$: (self.previous.count ?? 0) + 1" },
      switch: [{ case: "self.output.count >= 5", goto: "end" }, { goto: "$wait" }],
    },
  ]);
  const id = await ctx.env.start("delay_loop");

  // Advance by each arm's own remaining wait: a flat period would carry each round's real request
  // time forward until a round overshoots a grid point, which reads below as drift.
  for (let round = 1; round <= 5; round++) {
    await ctx.env.tick();
    const arms = await armLogs(id, round);
    expect(arms.length, `round ${round} did not arm`).toBeGreaterThanOrEqual(round);
    const last = arms.at(-1)!;
    await ctx.env.client.POST("/tick", { body: { advance_ms: last.target - last.at } });
  }
  expect(await ctx.env.status(id)).toBe("completed");

  const armed = (await armLogs(id, 5)).map((a) => a.target);

  expect(armed.length).toBe(5);
  for (const [n, at] of armed.entries()) {
    expect(new Date(at).getSeconds() % 5, `arm ${n} landed at ${new Date(at).toISOString()}`).toBe(0);
  }
  // Anchoring on arm time instead of the grid would show here as a growing gap.
  for (let n = 1; n < armed.length; n++) {
    expect(armed[n]! - armed[n - 1]!, `arms ${n - 1}→${n}`).toBe(5000);
  }
});

// An `until` already behind now clamps to now rather than failing — the rule pause/resume
// forces, since timers keep running while an instance is suspended.
test("an `until` in the past wakes immediately instead of failing", async () => {
  await ctx.env.define("delay_past", [
    { id: "wait", action: { type: "delay", until: "2020-01-01 08:00" }, switch: "end" },
  ]);
  const id = await ctx.env.start("delay_past");

  await ctx.env.tickUntilIdle();
  expect(await ctx.env.status(id)).toBe("completed");
});

test("pause takes effect immediately on a delayed instance — no drain tick needed", async () => {
  await ctx.env.client.PUT("/definitions", {
    body: {
      name: "delay_pause",
      tasks: [{ id: "wait", action: { type: "delay", for: 3600000 }, switch: "end" }],
      // eslint-disable-next-line @typescript-eslint/no-explicit-any
    } as any,
  });
  const id = await ctx.env.start("delay_pause");

  expect(await ctx.env.tick()).toBe(1); // arm a 1-hour delay
  expect(await ctx.env.status(id)).toBe("running");

  // An instance parked on a timer holds no lease, so there is no in-flight task to
  // wait out: it goes straight to 'paused' rather than through 'pausing'.
  await ctx.env.pause(id);
  expect(await ctx.env.status(id)).toBe("paused");
  expect(await ctx.env.tick()).toBe(0); // and it is not claimable while paused
});

test("delay does not resume before the full ms has elapsed", async () => {
  await ctx.env.define("delay_partial", [
    { id: "wait", action: { type: "delay", for: 60000 }, switch: "end" },
  ]);
  const id = await ctx.env.start("delay_partial");

  expect(await ctx.env.tick()).toBe(1); // arm

  await ctx.env.client.POST("/tick", { body: { advance_ms: 30000 } });
  expect(await ctx.env.status(id)).toBe("running");

  await ctx.env.client.POST("/tick", { body: { advance_ms: 30000 } });
  expect(await ctx.env.status(id)).toBe("completed");
});

test("resume continues a delay toward its original deadline", async () => {
  await ctx.env.client.PUT("/definitions", {
    body: {
      name: "delay_resume",
      tasks: [{ id: "wait", action: { type: "delay", for: 60000 }, switch: "end" }],
      // eslint-disable-next-line @typescript-eslint/no-explicit-any
    } as any,
  });
  const id = await ctx.env.start("delay_resume");

  expect(await ctx.env.tick()).toBe(1); // arm delay (deadline = T + 60s)
  await ctx.env.pause(id);
  expect(await ctx.env.status(id)).toBe("paused");

  await ctx.env.resume(id);
  // Resumed toward the original deadline, NOT re-armed: a re-arm would claim it once (tick() === 1).
  expect(await ctx.env.tick()).toBe(0);
  expect(await ctx.env.status(id)).toBe("running");

  await ctx.env.client.POST("/tick", { body: { advance_ms: 60000 } });
  expect(await ctx.env.status(id)).toBe("completed");
});

test("a delay whose deadline passes while paused is due the moment it resumes", async () => {
  await ctx.env.client.PUT("/definitions", {
    body: {
      name: "delay_passed",
      tasks: [{ id: "wait", action: { type: "delay", for: 5000 }, switch: "end" }],
      // eslint-disable-next-line @typescript-eslint/no-explicit-any
    } as any,
  });
  const id = await ctx.env.start("delay_passed");

  expect(await ctx.env.tick()).toBe(1); // arm delay (deadline = T + 5s)
  await ctx.env.pause(id);
  expect(await ctx.env.status(id)).toBe("paused");

  // Pausing suspends execution, not time: the deadline elapses while paused.
  await ctx.env.client.POST("/tick", { body: { advance_ms: 10000 } });
  expect(await ctx.env.status(id)).toBe("paused");

  // Freezing the remaining duration on pause would park it 5s out and never settle here.
  await ctx.env.resume(id);
  await ctx.env.tickUntilIdle();
  expect(await ctx.env.status(id)).toBe("completed");
});

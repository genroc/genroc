import { expect, test } from "vitest";
import { useTickEnv } from "./helpers.ts";

// Exercises the `delay` action: it parks the instance by stamping next_retry_at
// (releasing the worker) and the normal claim loop resumes it once the server
// clock advances past the resolved instant. Driven in manual-tick mode with
// /tick advance_ms. These use the bare-number form of `for` (milliseconds); the
// literal grammars are covered by internal/delayspec.
const ctx = useTickEnv(20019);

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

// The literal grammar has to reach wake_at, not just pass registration: "2h30m" must park
// for ~9,000,000ms. Bracketing it a minute either side rules out the ways a literal can go
// wrong (resolving to zero, dropping the "30m", or reading "m" as milliseconds) without
// racing the real time that elapses between arming and the advance.
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

  // Advancing only part of the way leaves it parked.
  await ctx.env.client.POST("/tick", { body: { advance_ms: 30000 } });
  expect(await ctx.env.status(id)).toBe("running");

  // Advancing the remainder resumes and completes it.
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
  // Resumed toward the original (still-future) deadline, NOT re-armed: a plain tick
  // claims nothing because the preserved timer has not elapsed. (A from-scratch
  // re-arm would instead claim it once to re-stamp the timer — i.e. tick() === 1.)
  expect(await ctx.env.tick()).toBe(0);
  expect(await ctx.env.status(id)).toBe("running");

  // Reaching the original deadline completes it.
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

  // Pausing suspends execution, not time: the clock keeps running against the
  // preserved wake_at, so this deadline elapses while the instance sits paused.
  // Ticking there still does nothing — a paused instance is never claimed.
  await ctx.env.client.POST("/tick", { body: { advance_ms: 10000 } });
  expect(await ctx.env.status(id)).toBe("paused");

  // On resume the timer is already in the past, so it runs straight through with no
  // further clock advance. (Freezing the remaining duration instead would park it
  // 5s into the future and never settle here.)
  await ctx.env.resume(id);
  await ctx.env.tickUntilIdle();
  expect(await ctx.env.status(id)).toBe("completed");
});

import { expect, test } from "vitest";
import { useTickEnv } from "./helpers.ts";

// Exercises buffered signals (the push/webhook model): POST /instances/{id}/signal
// delivers a result to an external task by id — resolving it if armed, else buffering
// FIFO until the task next arms. Driven in manual-tick mode.
const ctx = useTickEnv(20032);

// eslint-disable-next-line @typescript-eslint/no-explicit-any
const approvedSchema: any = {
  type: "object",
  properties: { approved: { type: "boolean" } },
  required: ["approved"],
};

// eslint-disable-next-line @typescript-eslint/no-explicit-any
async function signal(id: string, taskId: string, result: unknown) {
  return ctx.env.client.POST("/external-tasks/signal", {
    // eslint-disable-next-line @typescript-eslint/no-explicit-any
    body: { instance_id: id, task_id: taskId, result } as any,
  });
}

// eslint-disable-next-line @typescript-eslint/no-explicit-any
async function contextOf(id: string): Promise<any> {
  const { data } = await ctx.env.client.GET("/instances/{id}/detail", { params: { path: { id } } });
  return data!.state;
}

test("a signal that arrives before the task arms is buffered, then consumed on arming", async () => {
  await ctx.env.define("sig_early", [
    {
      id: "approval",
      action: { type: "external", result_schema: approvedSchema },
      output: "$: self.result",
      switch: "end",
    },
  ]);
  const id = await ctx.env.start("sig_early");

  // The process hasn't reached the external task yet — the signal buffers.
  const { data, error } = await signal(id, "approval", { approved: true });
  expect(error).toBeUndefined();
  expect((data as { delivered: boolean; buffered: boolean }).delivered).toBe(false);
  expect((data as { buffered: boolean }).buffered).toBe(true);

  // Arming pops the buffered signal and runs straight through to completion.
  await ctx.env.tickUntilIdle();
  expect(await ctx.env.status(id)).toBe("completed");
  expect((await contextOf(id)).outputs.approval).toEqual({ approved: true });
});

// A pause suspends execution, not delivery. A signal to a paused instance whose task is
// armed is delivered into the task's result slot — that write clears the wait but leaves
// the status alone, so the instance still does not advance until it is resumed. Buffering
// it instead would strand it: an already-armed task never re-arms, so nothing would ever
// pop the buffer.
test("a signal to a paused instance is delivered but does not advance it until resume", async () => {
  await ctx.env.define("sig_paused", [
    {
      id: "approval",
      action: { type: "external", result_schema: approvedSchema },
      output: "$: self.result",
      switch: "end",
    },
  ]);
  const id = await ctx.env.start("sig_paused");

  expect(await ctx.env.tick()).toBe(1); // arm/park on the external task
  expect(await ctx.env.status(id)).toBe("running external");

  await ctx.env.pause(id);
  expect(await ctx.env.status(id)).toBe("paused external");

  // The task is armed, so the result is delivered rather than buffered — the pause does
  // not block correlation.
  const { data, error } = await signal(id, "approval", { approved: true });
  expect(error).toBeUndefined();
  expect((data as { delivered: boolean }).delivered).toBe(true);
  expect((data as { buffered: boolean }).buffered).toBe(false);

  // Delivery un-parked the external wait, but the instance is still paused, so it is
  // not claimed and the process does not move on.
  expect(await ctx.env.status(id)).toBe("paused");
  expect(await ctx.env.tick()).toBe(0);
  expect(await ctx.env.status(id)).toBe("paused");

  // Resuming lets it consume the result that was waiting for it all along.
  await ctx.env.resume(id);
  await ctx.env.tickUntilIdle();
  expect(await ctx.env.status(id)).toBe("completed");
  expect((await contextOf(id)).outputs.approval).toEqual({ approved: true });
});

// The buffering path still applies when the paused instance is not sitting on that
// task: the result waits until the task arms after the resume.
test("a signal for a not-yet-reached task buffers while paused, and lands on arming", async () => {
  await ctx.env.define("sig_paused_early", [
    { id: "step1", switch: [{ goto: "next" }] },
    {
      id: "approval",
      action: { type: "external", result_schema: approvedSchema },
      output: "$: self.result",
      switch: "end",
    },
  ]);
  const id = await ctx.env.start("sig_paused_early");

  await ctx.env.pause(id);
  expect(await ctx.env.status(id)).toBe("paused");

  const { data, error } = await signal(id, "approval", { approved: false });
  expect(error).toBeUndefined();
  expect((data as { buffered: boolean }).buffered).toBe(true);

  await ctx.env.resume(id);
  await ctx.env.tickUntilIdle();
  expect(await ctx.env.status(id)).toBe("completed");
  expect((await contextOf(id)).outputs.approval).toEqual({ approved: false });
});

test("a signal to an already-armed task resolves it immediately", async () => {
  await ctx.env.define("sig_armed", [
    { id: "approval", action: { type: "external", result_schema: approvedSchema }, switch: "end" },
  ]);
  const id = await ctx.env.start("sig_armed");

  expect(await ctx.env.tick()).toBe(1); // arm/park
  expect(await ctx.env.status(id)).toBe("running external");

  const { data } = await signal(id, "approval", { approved: false });
  expect((data as { delivered: boolean }).delivered).toBe(true); // resolved, not buffered

  await ctx.env.tickUntilIdle();
  expect(await ctx.env.status(id)).toBe("completed");
});

test("signals are rejected for unknown, non-external, or schema-violating targets", async () => {
  await ctx.env.define("sig_reject", [
    { id: "fetch", action: { type: "fetch", url: "http://localhost:1/none" }, switch: "$wait" },
    { id: "wait", action: { type: "external", result_schema: approvedSchema }, switch: "end" },
  ]);
  const id = await ctx.env.start("sig_reject");

  // Unknown task id.
  expect((await signal(id, "ghost", { approved: true })).error).toBeDefined();
  // Existing task, but not an external one.
  expect((await signal(id, "fetch", { approved: true })).error).toBeDefined();
  // External task, but the payload violates its result_schema.
  expect((await signal(id, "wait", { approved: "yes" })).error).toBeDefined();
});

test("cancel-a-poller: a buffered cancel signal redirects the flow on the next arming", async () => {
  // A polling-style external wait that loops until a {cancel:true} signal arrives.
  await ctx.env.define("sig_poll", [
    {
      id: "poll",
      action: {
        type: "external",
        result_schema: {
          type: "object",
          properties: { cancel: { type: "boolean" } },
          required: ["cancel"],
        },
      },
      switch: [
        { case: "self.result.cancel == true", goto: "$cleanup" },
        { goto: "$poll" },
      ],
    },
    { id: "cleanup", switch: "end" },
  ]);
  const id = await ctx.env.start("sig_poll");

  // The user cancels before (or between) polls — the signal buffers and is consumed when
  // the poll task next arms, routing to cleanup instead of looping forever.
  const { data } = await signal(id, "poll", { cancel: true });
  expect((data as { buffered: boolean }).buffered).toBe(true);

  await ctx.env.tickUntilIdle();
  expect(await ctx.env.status(id)).toBe("completed");
});

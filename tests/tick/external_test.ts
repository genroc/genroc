import { parkedTask } from "../helpers/external.ts";
import { expect, test } from "vitest";
import { useTickEnv } from "./helpers.ts";

// Exercises the `external` action: the engine parks the instance (wait_state='external',
// no worker held), an outside caller discovers it via GET /external-tasks and submits a
// result to POST /external-tasks/resolve, and the process resumes. An optional timeout
// raises a catchable external.timeout. Driven in manual-tick mode.
const ctx = useTickEnv(20031);

// eslint-disable-next-line @typescript-eslint/no-explicit-any
const approvedSchema: any = {
  type: "object",
  properties: { approved: { type: "boolean" } },
  required: ["approved"],
};

// The parked task, assembled from the instance itself — the listing that used to hand these
// out is gone (helpers/external.ts).
const queueEntryFor = (id: string) => parkedTask(id, ctx.env.client);

// eslint-disable-next-line @typescript-eslint/no-explicit-any
async function resolve(token: string, result: unknown) {
  return ctx.env.client.POST("/external-tasks/resolve", {
    // eslint-disable-next-line @typescript-eslint/no-explicit-any
    body: { token, result } as any,
  });
}

// eslint-disable-next-line @typescript-eslint/no-explicit-any
async function contextOf(id: string): Promise<any> {
  const { data } = await ctx.env.client.GET("/instances/{id}/detail", { params: { path: { id } } });
  return data!.state;
}

test("external parks, is queued, and resumes when resolved", async () => {
  await ctx.env.define("ext_happy", [
    {
      id: "approval",
      action: { type: "external", input: { msg: "approve me" }, result_schema: approvedSchema },
      output: "$: self.result",
      switch: "end",
    },
  ]);
  const id = await ctx.env.start("ext_happy");

  // First tick arms the wait; the instance parks (running, wait_state='external').
  expect(await ctx.env.tick()).toBe(1);
  expect(await ctx.env.status(id)).toBe("running external");

  // While parked it is not claimable — a plain tick processes nothing.
  expect(await ctx.env.tick()).toBe(0);

  // Parked, with the evaluated input snapshot readable and a token derivable from the row.
  const entry = await queueEntryFor(id);
  expect(entry).toBeDefined();
  expect(entry!.task_id).toBe("approval");
  expect(entry!.input).toEqual({ msg: "approve me" });
  expect(entry!.token).toBe(`${id}.0`);

  // Submitting a valid result un-parks it; the next tick runs it to completion.
  const { error } = await resolve(entry!.token, { approved: true });
  expect(error).toBeUndefined();

  expect(await ctx.env.tick()).toBe(1);
  expect(await ctx.env.status(id)).toBe("completed");
  // The submitted result flowed through self.result into the task output.
  expect((await contextOf(id)).outputs.approval).toEqual({ approved: true });
});

test("resolve validates the result against result_schema", async () => {
  await ctx.env.define("ext_validate", [
    { id: "approval", action: { type: "external", result_schema: approvedSchema }, switch: "end" },
  ]);
  const id = await ctx.env.start("ext_validate");
  expect(await ctx.env.tick()).toBe(1);

  const entry = await queueEntryFor(id);
  // approved must be a boolean — a string is rejected and the task stays parked.
  const { error } = await resolve(entry!.token, { approved: "yes" });
  expect(error).toBeDefined();
  expect(await ctx.env.status(id)).toBe("running external");

  // A valid result still works afterwards.
  const ok = await resolve(entry!.token, { approved: false });
  expect(ok.error).toBeUndefined();
  expect(await ctx.env.tick()).toBe(1);
  expect(await ctx.env.status(id)).toBe("completed");
});

test("a stale/double resolve is rejected", async () => {
  await ctx.env.define("ext_double", [
    { id: "approval", action: { type: "external", result_schema: approvedSchema }, switch: "end" },
  ]);
  const id = await ctx.env.start("ext_double");
  expect(await ctx.env.tick()).toBe(1);

  const entry = await queueEntryFor(id);
  expect((await resolve(entry!.token, { approved: true })).error).toBeUndefined();
  // Second submit with the same token: the task is no longer waiting.
  expect((await resolve(entry!.token, { approved: false })).error).toBeDefined();
  await ctx.env.tickUntilIdle(); // drain the resolved instance so it does not bleed into later tests
});

test("timeout raises external.timeout, catchable in on_error", async () => {
  await ctx.env.define("ext_timeout", [
    {
      id: "approval",
      action: { type: "external", result_schema: approvedSchema },
      timeout: 60000,
      on_error: [{ code: ["external.timeout"], goto: "$handler" }],
      switch: "end",
    },
    { id: "handler", switch: "end" },
  ]);
  const id = await ctx.env.start("ext_timeout");

  expect(await ctx.env.tick()).toBe(1); // arm (deadline = T + 60s)
  expect(await ctx.env.status(id)).toBe("running external");

  // Not due yet: a plain tick claims nothing.
  expect(await ctx.env.tick()).toBe(0);
  expect(await ctx.env.status(id)).toBe("running external");

  // Advancing past the deadline fires the timeout, which routes to the handler.
  await ctx.env.client.POST("/tick", { body: { advance_ms: 60000 } });
  await ctx.env.tickUntilIdle();
  expect(await ctx.env.status(id)).toBe("completed");
});

// The counterpart to the delay case: an external task's timeout is a timer like any
// other, so it keeps running while the instance is paused and is simply due on resume.
test("an external timeout that elapses while paused fires on resume", async () => {
  await ctx.env.define("ext_timeout_paused", [
    {
      id: "approval",
      action: { type: "external", result_schema: approvedSchema },
      timeout: 60000,
      on_error: [{ code: ["external.timeout"], goto: "$handler" }],
      switch: "end",
    },
    { id: "handler", switch: "end" },
  ]);
  const id = await ctx.env.start("ext_timeout_paused");

  expect(await ctx.env.tick()).toBe(1); // arm (deadline = T + 60s)
  await ctx.env.pause(id);
  expect(await ctx.env.status(id)).toBe("paused external");

  // The deadline passes while suspended. A paused instance is never claimed, so the
  // timeout does not fire here — it is deferred, not cancelled.
  await ctx.env.client.POST("/tick", { body: { advance_ms: 90000 } });
  expect(await ctx.env.status(id)).toBe("paused external");

  // On resume the timer is already overdue, so it fires with no further clock advance
  // and routes through the on_error handler exactly as an un-paused timeout would.
  await ctx.env.resume(id);
  await ctx.env.tickUntilIdle();
  expect(await ctx.env.status(id)).toBe("completed");
});

test("a no-timeout external wait is never self-claimed", async () => {
  await ctx.env.define("ext_wait", [
    { id: "approval", action: { type: "external", result_schema: approvedSchema }, switch: "end" },
  ]);
  const id = await ctx.env.start("ext_wait");
  expect(await ctx.env.tick()).toBe(1); // arm, no timer

  // Advancing the clock far forward does not make it claimable (no timeout).
  await ctx.env.client.POST("/tick", { body: { advance_ms: 3600000 } });
  expect(await ctx.env.status(id)).toBe("running external");

  // Only a submitted result resumes it.
  const entry = await queueEntryFor(id);
  expect((await resolve(entry!.token, { approved: true })).error).toBeUndefined();
  expect(await ctx.env.tick()).toBe(1);
  expect(await ctx.env.status(id)).toBe("completed");
});

test("pausing an externally-waiting instance takes it out of the queue", async () => {
  await ctx.env.define("ext_pause", [
    { id: "approval", action: { type: "external", result_schema: approvedSchema }, switch: "end" },
  ]);
  const id = await ctx.env.start("ext_pause");
  expect(await ctx.env.tick()).toBe(1);
  expect(await ctx.env.status(id)).toBe("running external");

  // The external wait is preserved — only the status changes — but the task stops
  // being offered to external workers, because they could not resolve it anyway.
  await ctx.env.pause(id);
  expect(await ctx.env.status(id)).toBe("paused external");

  // "Out of the queue" is observable where the queue now lives: a claim does not offer it.
  const claimed = await ctx.env.client.POST("/external-tasks/claim", {
    body: { worker_id: "w-pause", limit: 10 },
  });
  // eslint-disable-next-line @typescript-eslint/no-explicit-any
  const claimedIds = ((claimed.data as any)?.items ?? []).map((t: any) => t.token.split(".")[0]);
  expect(claimedIds).not.toContain(id);

  // Resolving a paused task is rejected.
  const { data, error } = await ctx.env.client.POST("/external-tasks/resolve", {
    // eslint-disable-next-line @typescript-eslint/no-explicit-any
    body: { token: `${id}.whatever`, result: { approved: true } } as any,
  });
  expect(error ?? (data as { error?: string })?.error).toBeTruthy();

  // Resuming puts it back on exactly the same external wait, and claimable again.
  await ctx.env.resume(id);
  expect(await ctx.env.status(id)).toBe("running external");
  const again = await ctx.env.client.POST("/external-tasks/claim", {
    body: { worker_id: "w-resume", limit: 10 },
  });
  // eslint-disable-next-line @typescript-eslint/no-explicit-any
  expect(((again.data as any)?.items ?? []).map((t: any) => t.token.split(".")[0])).toContain(id);

  await ctx.env.pause(id);
});

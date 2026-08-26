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

// Find the single queue entry for an instance id (the token is `<id>.<nonce>`).
// eslint-disable-next-line @typescript-eslint/no-explicit-any
async function queueEntryFor(id: string): Promise<any | undefined> {
  const { data, error } = await ctx.env.client.GET("/external-tasks", {});
  if (error) throw new Error(`list external tasks failed: ${JSON.stringify(error)}`);
  // eslint-disable-next-line @typescript-eslint/no-explicit-any
  return ((data?.items ?? []) as any[]).find(
    (t) => typeof t.token === "string" && t.token.startsWith(`${id}.`),
  );
}

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

  // It appears on the queue with its input + result_schema + a token, no context.
  const entry = await queueEntryFor(id);
  expect(entry).toBeDefined();
  expect(entry.task_id).toBe("approval");
  expect(entry.process).toBe("ext_happy");
  expect(entry.input).toEqual({ msg: "approve me" });
  expect(entry.result_schema).toBeTruthy();
  expect(entry).not.toHaveProperty("context");

  // Submitting a valid result un-parks it; the next tick runs it to completion.
  const { error } = await resolve(entry.token, { approved: true });
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
  const { error } = await resolve(entry.token, { approved: "yes" });
  expect(error).toBeDefined();
  expect(await ctx.env.status(id)).toBe("running external");

  // A valid result still works afterwards.
  const ok = await resolve(entry.token, { approved: false });
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
  expect((await resolve(entry.token, { approved: true })).error).toBeUndefined();
  // Second submit with the same token: the task is no longer waiting.
  expect((await resolve(entry.token, { approved: false })).error).toBeDefined();
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
  expect((await resolve(entry.token, { approved: true })).error).toBeUndefined();
  expect(await ctx.env.tick()).toBe(1);
  expect(await ctx.env.status(id)).toBe("completed");
});

test("the queue filters by task id in SQL, so filtered pages stay full and counts stay accurate", async () => {
  // Two processes whose external tasks have distinct ids. Park 3 betas, then 3 alphas,
  // so the betas sort ahead (oldest-first). A task=pq_alpha filter must skip the betas
  // in SQL — not page over them and post-filter, which would under-fill the page and
  // count the skipped betas.
  await ctx.env.define("ext_pq_alpha", [
    { id: "pq_alpha", action: { type: "external", result_schema: approvedSchema }, switch: "end" },
  ]);
  await ctx.env.define("ext_pq_beta", [
    { id: "pq_beta", action: { type: "external", result_schema: approvedSchema }, switch: "end" },
  ]);

  const betas = [
    await ctx.env.start("ext_pq_beta"),
    await ctx.env.start("ext_pq_beta"),
    await ctx.env.start("ext_pq_beta"),
  ];
  const alphas = [
    await ctx.env.start("ext_pq_alpha"),
    await ctx.env.start("ext_pq_alpha"),
    await ctx.env.start("ext_pq_alpha"),
  ];
  // max-concurrent=1: exactly one instance arms per tick, in start order — so all six
  // park betas-before-alphas in updated_at (and UUIDv7 id) order.
  for (let i = 0; i < betas.length + alphas.length; i++) await ctx.env.tick();

  // First filtered page: two alphas, the three betas skipped entirely, one alpha after.
  const first = await ctx.env.client.GET("/external-tasks", {
    params: { query: { task: "pq_alpha", limit: 2 } },
  });
  if (first.error) throw new Error(`list failed: ${JSON.stringify(first.error)}`);
  const page1 = first.data!;
  expect(page1.items!.map((t) => t.task_id)).toEqual(["pq_alpha", "pq_alpha"]);
  expect(page1.page.items_before).toBe(0);
  expect(page1.page.items_after).toBe(1); // only the third alpha; betas are not counted
  expect(page1.page.after).toBeTruthy(); // more to come

  // Following the cursor yields the last alpha and nothing else.
  const second = await ctx.env.client.GET("/external-tasks", {
    params: { query: { task: "pq_alpha", limit: 2, after: page1.page.after } },
  });
  if (second.error) throw new Error(`list failed: ${JSON.stringify(second.error)}`);
  const page2 = second.data!;
  expect(page2.items!.map((t) => t.task_id)).toEqual(["pq_alpha"]);
  expect(page2.page.items_before).toBe(2);
  expect(page2.page.items_after).toBe(0);

  // Pause so these leave the queue and do not bleed into later tests.
  for (const id of [...betas, ...alphas]) await ctx.env.pause(id);
  await ctx.env.tickUntilIdle();
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

  // The queue identifies tasks by "<instance-id>.<nonce>" resolve token.
  const queuedIds = async () =>
    (await ctx.env.client.GET("/external-tasks", {})).data!.items!.map((t) =>
      t.token.split(".")[0],
    );
  expect(await queuedIds()).not.toContain(id);

  // Resolving a paused task is rejected.
  const { data, error } = await ctx.env.client.POST("/external-tasks/resolve", {
    // eslint-disable-next-line @typescript-eslint/no-explicit-any
    body: { token: `${id}.whatever`, result: { approved: true } } as any,
  });
  expect(error ?? (data as { error?: string })?.error).toBeTruthy();

  // Resuming puts it back in the queue on exactly the same external wait.
  await ctx.env.resume(id);
  expect(await ctx.env.status(id)).toBe("running external");
  expect(await queuedIds()).toContain(id);

  await ctx.env.pause(id);
});

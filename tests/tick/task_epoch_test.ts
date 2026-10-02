/** The task-epoch mechanism, read from SQL (child_loop_test.ts covers the symptom). The bump lives in
 *  enterTask, not advance: advance repoints inst.Task on every claim, including a parked parent resuming
 *  to collect, and a bump there would collect against an epoch none of its children carry. */
import { parkedTask } from "../helpers/external.ts";
import { expect, test } from "vitest";
import { useTickEnv } from "./helpers.ts";

const ctx = useTickEnv();

type Env = ReturnType<typeof useTickEnv>["env"];

/** Two tasks, so the child transitions once and moves its OWN epoch. */
async function defineLeaf(env: Env): Promise<string> {
  const leaf = `epoch_leaf_${crypto.randomUUID()}`;
  await env.define(leaf, [
    { id: "a", output: { step: "$: 1" }, switch: [{ goto: "$b" }] },
    { id: "b", output: { step: "$: 2" }, switch: [{ goto: "end" }] },
  ]);
  return leaf;
}

/** tick → call → (back to tick, or end). `call` spawns one child per pass. */
async function defineLoop(env: Env, passes: number): Promise<string> {
  const leaf = await defineLeaf(env);
  const parent = `epoch_loop_${crypto.randomUUID()}`;
  await env.define(parent, [
    { id: "tick", output: { i: "$: (self.previous.i ?? 0) + 1" }, switch: [{ goto: "$call" }] },
    {
      id: "call",
      action: { type: "child", name: leaf },
      switch: [{ case: `outputs.tick.i >= ${passes}`, goto: "end" }, { goto: "$tick" }],
    },
  ]);
  return parent;
}

test("task_epoch — parking, settling and resuming all leave the parent's epoch alone", async () => {
  const env = ctx.env;
  const id = await env.start(await defineLoop(env, 1));

  // One tick runs `tick` inline, transitions into `call`, spawns and parks.
  await env.tick();
  expect(await env.phase(id)).toBe("children");

  const atSpawn = env.epochs(id).task;
  const children = env.allChildrenOf(id, "call");
  expect(children).toHaveLength(1);
  expect(children[0].batch, "the child records the parent's epoch at spawn").toBe(atSpawn);

  // The child runs and settles, waking the parent. Not the parent entering a task.
  await env.tick();
  expect(env.epochs(id).task, "settling a child must not move the parent's epoch").toBe(atSpawn);

  // The collect tick resumes and ends with no transition: any movement is a resume miscounted
  // as an entry.
  await env.tick();
  expect(await env.status(id)).toBe("completed");
  expect(env.epochs(id).task, "resuming to collect is not a task entry").toBe(atSpawn);
});

test("task_epoch — a second pass spawns into a different batch", async () => {
  const env = ctx.env;
  const id = await env.start(await defineLoop(env, 2));

  await env.tick();
  const firstBatch = env.epochs(id).task;

  await env.tickUntilIdle(30);
  expect(await env.status(id)).toBe("completed");

  // Both passes' children live under the same (parent_id, spawn_task_id); only the batch tells them apart.
  const children = env.allChildrenOf(id, "call");
  expect(children).toHaveLength(2);
  expect(children[0].batch).toBe(firstBatch);
  expect(children[1].batch, "the second pass must not reuse the first pass's batch").toBeGreaterThan(
    firstBatch,
  );
});

test("task_epoch — a child's own epoch advances; the batch it belongs to never does", async () => {
  const env = ctx.env;
  const id = await env.start(await defineLoop(env, 1));
  await env.tick();

  const [child] = env.allChildrenOf(id, "call");
  const before = env.epochs(child.id);
  expect(before.batch, "parent_task_epoch is the batch, stamped once at insert").toBe(
    env.epochs(id).task,
  );

  await env.tickUntilIdle(30);

  const after = env.epochs(child.id);
  expect(after.batch, "the batch number is immutable after insert").toBe(before.batch);
  expect(after.task, "the child transitions a→b, so its own epoch moves").toBeGreaterThan(
    before.task,
  );
});

/** Batch numbers are the deterministic signal: an unscoped collect merges duplicate slots silently with
 *  a nondeterministic winner (no ORDER BY), so asserting on merged output would pass or fail by luck. */
async function batchesPerPass(env: Env, parent: string, taskId: string, expectedPerPass: number) {
  const id = await env.start(parent);
  await env.tickUntilIdle(40);
  const children = env.allChildrenOf(id, taskId);
  const byBatch = new Map<number, number>();
  for (const c of children) byBatch.set(c.batch, (byBatch.get(c.batch) ?? 0) + 1);
  return { id, children, batches: [...byBatch.keys()].sort((a, b) => a - b), byBatch, expectedPerPass };
}

test("task_epoch — child_map re-entered in a loop puts each pass in its own batch", async () => {
  const env = ctx.env;
  const leaf = await defineLeaf(env);
  const parent = `epoch_map_${crypto.randomUUID()}`;
  await env.define(parent, [
    { id: "tick", output: { i: "$: (self.previous.i ?? 0) + 1" }, switch: [{ goto: "$fan" }] },
    {
      id: "fan",
      action: { type: "child_map", children: { a: { name: leaf }, b: { name: leaf } } },
      switch: [{ case: "outputs.tick.i >= 3", goto: "end" }, { goto: "$tick" }],
    },
  ]);

  const r = await batchesPerPass(env, parent, "fan", 2);
  expect(await env.status(r.id)).toBe("completed");
  expect(r.children).toHaveLength(6); // 3 passes x 2 keys
  expect(r.batches, "three passes, three distinct batches").toHaveLength(3);
  // Two keys per batch and never more: a shared batch is what makes a key collide.
  for (const b of r.batches) expect(r.byBatch.get(b)).toBe(2);
});

test("task_epoch — a loop re-entered through a RAISED child's route gets a fresh batch", async () => {
  const env = ctx.env;
  const raiser = `epoch_raiser_${crypto.randomUUID()}`;
  await env.define(raiser, [
    { id: "t", switch: [{ raise: { code: "always_raises", message: "nope" } }] },
  ]);
  const parent = `epoch_raised_${crypto.randomUUID()}`;
  await env.define(parent, [
    {
      id: "call",
      action: { type: "child", name: raiser },
      // collect.go's own goto, not advance's switch — its own enterTask call site.
      on_error: [{ code: ["always_raises"], goto: "$again" }],
      switch: [{ goto: "end" }],
    },
    {
      id: "again",
      output: { i: "$: (self.previous.i ?? 0) + 1" },
      switch: [{ case: "self.output.i >= 3", goto: "end" }, { goto: "$call" }],
    },
  ]);

  const r = await batchesPerPass(env, parent, "call", 1);
  expect(await env.status(r.id)).toBe("completed");
  expect(r.children).toHaveLength(3);
  expect(r.batches, "each raised pass spawns into its own batch").toHaveLength(3);
});

/** Retry reconstructs onto the parent's OWN batch, so its epoch must not move: a bump orphans the kept
 *  children and a child_map merges {} (specs/child-error-handling.md §12). The no-batch revive that DOES
 *  bump is pinned by TestRetryProcess_BumpsEpochWithoutABatch. */
test("task_epoch — an operator retry reconstructs the existing batch", async () => {
  const env = ctx.env;
  // Nothing listens on port 1, so this leaf fails every time and poisons its parent.
  const leaf = `epoch_deadleaf_${crypto.randomUUID()}`;
  await env.define(leaf, [
    {
      id: "t",
      action: { type: "fetch", url: "http://localhost:1/x", method: "get", timeout: 2000 },
      switch: [{ goto: "end" }],
    },
  ]);
  const parent = `epoch_retry_${crypto.randomUUID()}`;
  await env.define(parent, [
    { id: "call", action: { type: "child", name: leaf }, switch: [{ goto: "end" }] },
  ]);

  const id = await env.start(parent);
  await env.tickUntilIdle(40);
  expect(await env.status(id)).toBe("failed");

  const first = env.allChildrenOf(id, "call");
  expect(first).toHaveLength(1);
  const epochBefore = env.epochs(id).task;

  await env.retry(id);
  await env.tickUntilIdle(40);

  expect(env.epochs(id).task, "the epoch addresses the batch, so reconstructing must not move it").toBe(
    epochBefore,
  );
  const after = env.allChildrenOf(id, "call");
  expect(after, "a failed child is revived in place, never re-spawned beside itself").toHaveLength(1);
  expect(new Set(after.map((c) => c.batch)).size, "still one batch").toBe(1);
  // The failure repeats, but as a clean failure — never a collect over two batches.
  const { data } = await env.client.GET("/instances/{id}", { params: { path: { id } } });
  expect(String(data?.error_message ?? ""), "must not be the multi-batch collect error").not.toContain(
    "expected exactly one child",
  );
});

/** The external token IS the task epoch, so a re-arm must move the epoch even though nothing
 *  transitioned; otherwise a stale result is accepted. */
test("task_epoch — a re-arm issues a new external token, and the stale one is refused", async () => {
  const env = ctx.env;
  const name = `epoch_ext_${crypto.randomUUID()}`;
  await env.define(name, [
    {
      id: "wait",
      action: { type: "external", timeout: 1000 },
      on_error: [{ code: ["external.timeout"], retry: 2, goto: "end" }],
      switch: [{ goto: "end" }],
    },
  ]);
  const id = await env.start(name);

  const tokenOf = async () => (await parkedTask(id, env.client))?.token ?? "";

  await env.tick();
  const first = await tokenOf();
  expect(first, "the token is derived from the epoch, not minted").toBe(`${id}.${env.epochs(id).task}`);

  // Push past the deadline: the claim raises external.timeout and the rule re-arms.
  await env.client.POST("/tick", { body: { advance_ms: 5000 } });
  await env.client.POST("/tick", { body: { advance_ms: 5000 } });

  const second = await tokenOf();
  expect(second).toBe(`${id}.${env.epochs(id).task}`);
  expect(second, "a re-arm is a new occurrence, so a new token").not.toBe(first);

  const stale = await env.client.POST("/external-tasks/resolve", {
    body: { token: first, result: { late: true } } as never,
  });
  expect(stale.error, "a stale token must be refused").toBeDefined();

  const fresh = await env.client.POST("/external-tasks/resolve", {
    body: { token: second, result: { late: false } } as never,
  });
  expect(fresh.error).toBeUndefined();
});

/**
 * The task-epoch mechanism, observed one tick at a time and read straight out of SQL.
 *
 * `tests/integration/child_loop_test.ts` covers the SYMPTOM (a loop over a child task
 * completes). These assert the mechanism it rests on, which no API surface exposes:
 *
 *   - task_epoch does not move while an instance is parked, nor when it resumes;
 *   - a child records the parent's task_epoch at spawn as parent_task_epoch;
 *   - a second pass through the same task produces a DIFFERENT batch number, which is what
 *     lets the collect pick one batch out of the several living under (parent, task).
 *
 * The first is the load-bearing one. advance points inst.Task at the task about to run on
 * every claim, including the one where a parked parent resumes to collect; if the bump lived
 * there instead of in enterTask, a parent would collect against an epoch none of its own
 * children carry, and every child task would hang.
 */
import { expect, test } from "vitest";
import { useTickEnv } from "./helpers.ts";

const ctx = useTickEnv(20095);

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
  expect(await env.waitState(id)).toBe("waiting");

  const atSpawn = env.epochs(id).task;
  const children = env.allChildrenOf(id, "call");
  expect(children).toHaveLength(1);
  expect(children[0].batch, "the child records the parent's epoch at spawn").toBe(atSpawn);

  // The child runs and settles, waking the parent. Not the parent entering a task.
  await env.tick();
  expect(env.epochs(id).task, "settling a child must not move the parent's epoch").toBe(atSpawn);

  // The collect tick resumes the parked task and this definition ends there, so no
  // transition happens at all. Any movement here would be the resume being miscounted as an
  // entry — the failure that makes a parent collect against an epoch its children lack.
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

  // Both passes' children still live under the same (parent_id, spawn_task_id) — nothing is
  // deleted. They are told apart only by the batch number, which is the whole mechanism.
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

/**
 * Batch-level coverage of the paths child_loop_test.ts cannot pin behaviourally.
 *
 * buildMapChildOutput overwrites by key and GetChildrenForTask has no ORDER BY, so an
 * unscoped child_map collect merges duplicate slots with NO error and a nondeterministic
 * winner; resolveRaisedBatch picks raised[0] the same way. A test asserting on the merged
 * output would pass or fail by luck. The batch numbers are the deterministic signal.
 */
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

/**
 * RetryProcess reconstructs a parent onto ITS OWN batch — a failed child is revived in place
 * — so the epoch that ADDRESSES that batch must not move. Bumping it orphans every child the
 * parent kept, and the collect that follows finds none of them: a child_map then merges {}
 * and reports 'completed'. specs/child-error-handling.md §12.
 *
 * The re-spawn this once guarded against cannot happen now that the walk scopes its lookup to
 * the current epoch: children at that epoch mean reconstruct-never-respawn, and no children
 * there means a spawn collides with nothing. The one revive that DOES move the epoch — a task
 * with no batch, which re-arms an external task on a token derived from it — is pinned by
 * TestRetryProcess_BumpsEpochWithoutABatch.
 */
test("task_epoch — an operator retry reconstructs the existing batch", async () => {
  const env = ctx.env;
  // Nothing listens on port 1, so this leaf fails every time and poisons its parent.
  const leaf = `epoch_deadleaf_${crypto.randomUUID()}`;
  await env.define(leaf, [
    {
      id: "t",
      action: { type: "fetch", url: "http://localhost:1/x", method: "GET" },
      timeout: 2000,
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

/**
 * The external token IS the task epoch. A submitted result must land on the arming it was
 * issued for, and a re-arm (after an external.timeout retry) is a new occurrence — so the
 * token has to change even though nothing transitioned. That is why the retry branch moves
 * the epoch: without it the token is identical across armings and a stale result is accepted.
 */
test("task_epoch — a re-arm issues a new external token, and the stale one is refused", async () => {
  const env = ctx.env;
  const name = `epoch_ext_${crypto.randomUUID()}`;
  await env.define(name, [
    {
      id: "wait",
      action: { type: "external" },
      timeout: 1000,
      on_error: [{ code: ["external.timeout"], retry: 2, goto: "end" }],
      switch: [{ goto: "end" }],
    },
  ]);
  const id = await env.start(name);

  // Read the token the QUEUE hands out, not one reconstructed here: external_data no
  // longer stores it, so this also checks the endpoint derives it from the row.
  const tokenOf = async () => {
    const { data } = await env.client.GET("/external-tasks", {});
    const mine = (data?.items ?? []).filter((t) => (t.token ?? "").startsWith(`${id}.`));
    return mine.length === 1 ? (mine[0].token as string) : "";
  };

  await env.tick();
  const first = await tokenOf();
  expect(first, "the token is derived from the epoch, not minted").toBe(`${id}.${env.epochs(id).task}`);

  // Push past the deadline: the claim raises external.timeout and the rule re-arms.
  await env.client.POST("/tick", { body: { advance_ms: 5000 } });
  await env.client.POST("/tick", { body: { advance_ms: 5000 } });

  const second = await tokenOf();
  expect(second).toBe(`${id}.${env.epochs(id).task}`);
  expect(second, "a re-arm is a new occurrence, so a new token").not.toBe(first);

  // The guarantee itself: the previous arming's token must no longer resolve this task.
  const stale = await env.client.POST("/external-tasks/resolve", {
    body: { token: first, result: { late: true } } as never,
  });
  expect(stale.error, "a stale token must be refused").toBeDefined();

  // ...while the current one still does.
  const fresh = await env.client.POST("/external-tasks/resolve", {
    body: { token: second, result: { late: false } } as never,
  });
  expect(fresh.error).toBeUndefined();
});

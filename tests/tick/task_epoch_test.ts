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

const ctx = useTickEnv(20051);

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

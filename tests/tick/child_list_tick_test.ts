/**
 * Tick-level observation of a child_list (array fan-out) task, one DB transition at
 * a time. The server runs in manual-tick mode (--poll 0, --max-concurrent 1) so each
 * tick claims exactly one instance and every state change is inspectable.
 *
 * ClaimInstances uses ORDER BY created_at ASC, and SpawnChildrenAndWait assigns each
 * child a strictly increasing created_at (element 0, 1, 2, …), so the children run in
 * input order — and the collected result array must come back in that same order.
 */
import { expect, test, beforeAll, afterAll } from "vitest";
import { startMockService, childrenOfTask } from "../helpers/client.ts";
import { useTickEnv } from "./helpers.ts";

const PORT = 20040;
const ctx = useTickEnv(PORT);

let doubler: string; // success leaf: {n} -> {doubled: n*2}
let failWorker: string; // leaf whose REST call always 500s
let fanout: string; // parent: child_list over input.items -> doubler
let failFanout: string; // parent: child_list over input.items -> failWorker
let stopMock: () => Promise<void>;

const itemsSchema = {
  type: "object" as const,
  properties: {
    items: {
      type: "array" as const,
      items: {
        type: "object" as const,
        properties: { n: { type: "integer" as const } },
        required: ["n"],
      },
    },
  },
  required: ["items"],
};

async function put(body: unknown) {
  const { error } = await ctx.env.client.PUT("/definitions", { body: body as never });
  if (error) throw new Error(`define failed: ${JSON.stringify(error)}`);
}

async function startWith(process: string, input: unknown): Promise<string> {
  const { data, error } = await ctx.env.client.POST("/instances", {
    body: { process, input } as never,
  });
  if (error) throw new Error(`start failed: ${JSON.stringify(error)}`);
  return data!.id;
}

async function outputOf(id: string): Promise<any> {
  const { data } = await ctx.env.client.GET("/instances/{id}/detail", { params: { path: { id } } });
  return (data?.state?.output as any) ?? null;
}

beforeAll(async () => {
  const uid = crypto.randomUUID().slice(0, 8);
  doubler = `cl_doubler_${uid}`;
  failWorker = `cl_fail_worker_${uid}`;
  fanout = `cl_fanout_${uid}`;
  failFanout = `cl_fail_fanout_${uid}`;

  const mock = await startMockService(0, { statusCode: 500 });
  stopMock = mock.stop;

  await put({
    name: doubler,
    input_schema: {
      type: "object",
      properties: { n: { type: "integer" } },
      required: ["n"],
    },
    tasks: [{ id: "done", switch: [{ goto: "end" }] }],
    output: { doubled: "$: input.n * 2" },
  });

  await put({
    name: failWorker,
    input_schema: {
      type: "object",
      properties: { n: { type: "integer" } },
      required: ["n"],
    },
    tasks: [
      {
        id: "boom",
        action: { type: "fetch", url: `http://localhost:${mock.port}/action` },
        timeout: 5_000,
        switch: [{ goto: "end" }],
      },
    ],
  });

  await put({
    name: fanout,
    input_schema: itemsSchema,
    tasks: [
      {
        id: "spread",
        action: {
          type: "child_list",
          name: doubler,
          over: "$: input.items",
          result_schema: {
            type: "object",
            properties: { doubled: { type: "number" } },
            required: ["doubled"],
          },
        },
        output: "$: self.result",
        switch: [{ goto: "end" }],
      },
    ],
    output: { results: "$: outputs.spread" },
  });

  await put({
    name: failFanout,
    input_schema: itemsSchema,
    tasks: [
      {
        id: "spread",
        action: { type: "child_list", name: failWorker, over: "$: input.items" },
        switch: [{ goto: "end" }],
      },
    ],
  });
}, 60_000);

afterAll(() => stopMock?.());

test("happy path — spawn, children complete in input order, collect ordered array", async () => {
  const root = await startWith(fanout, { items: [{ n: 1 }, { n: 2 }, { n: 3 }] });

  // tick: root evaluates child_list → spawns 3 children, parks itself (waiting).
  expect(await ctx.env.tick()).toBe(1);
  expect(await ctx.env.status(root)).toBe("running waiting");
  const kids = await ctx.env.listChildrenOf(root, "spread");
  expect(kids).toHaveLength(3);
  expect(await ctx.env.statuses({ c0: kids[0], c1: kids[1], c2: kids[2] })).toEqual({
    c0: "running",
    c1: "running",
    c2: "running",
  });

  // tick: c0 (earliest created_at) completes; root stays waiting.
  await ctx.env.tick();
  expect(await ctx.env.statuses({ root, c0: kids[0], c1: kids[1], c2: kids[2] })).toEqual({
    root: "running waiting",
    c0: "completed",
    c1: "running",
    c2: "running",
  });

  // tick: c1 completes.
  await ctx.env.tick();
  expect(await ctx.env.status(kids[1])).toBe("completed");
  expect(await ctx.env.status(root)).toBe("running waiting");

  // tick: c2 (last) completes → all siblings terminal → root woken to 'collecting'.
  await ctx.env.tick();
  expect(await ctx.env.statuses({ root, c2: kids[2] })).toEqual({
    root: "running collecting",
    c2: "completed",
  });

  // tick: root (collecting) merges the children's outputs into an ordered array,
  // advances to end → completed.
  await ctx.env.tick();
  expect(await ctx.env.status(root)).toBe("completed");

  // The result array is in input order, not completion order.
  expect((await outputOf(root)).results).toEqual([
    { doubled: 2 },
    { doubled: 4 },
    { doubled: 6 },
  ]);

  await ctx.env.tickUntilIdle();
});

test("empty over — no children spawned, completes in a single tick with []", async () => {
  const root = await startWith(fanout, { items: [] });

  // tick: over evaluates to [] → nothing to spawn, no park; the task yields [] inline
  // and the instance runs straight to completion in this one tick.
  expect(await ctx.env.tick()).toBe(1);
  expect(await ctx.env.status(root)).toBe("completed");
  expect((await outputOf(root)).results).toEqual([]);

  // No child instance was ever created for this parent.
  expect(await childrenOfTask(root, "spread", ctx.env.client)).toEqual([]);

  await ctx.env.tickUntilIdle();
});

test("a failing child fails the whole batch — root settles to 'failed'", async () => {
  const root = await startWith(failFanout, { items: [{ n: 1 }, { n: 2 }] });

  // tick: spawn 2 children, park root.
  await ctx.env.tick();
  expect(await ctx.env.status(root)).toBe("running waiting");
  const kids = await ctx.env.listChildrenOf(root, "spread");
  expect(kids).toHaveLength(2);

  // tick: first child runs its REST call, gets 500 → fails; FailInstanceAndAncestors
  // turns the still-waiting root to 'failing'.
  await ctx.env.tick();
  expect(await ctx.env.status(kids[0])).toBe("failed");
  expect(await ctx.env.waitState(root)).not.toBe("collecting");
  expect(await ctx.env.status(root)).toContain("failing");

  // Drain the rest of the tree: the sibling settles and the failing root becomes
  // 'failed' (a failing parent never merges outputs).
  await ctx.env.tickUntilIdle();
  expect(await ctx.env.status(kids[0])).toBe("failed");
  expect(await ctx.env.status(kids[1])).toBe("failed");
  expect(await ctx.env.status(root)).toBe("failed");
});

test("pause the root — its whole child_list fan-out suspends, and resume finishes it", async () => {
  const root = await startWith(fanout, { items: [{ n: 1 }, { n: 2 }, { n: 3 }] });

  // tick: spawn 3 children, park root.
  await ctx.env.tick();
  const kids = await ctx.env.listChildrenOf(root, "spread");
  expect(kids).toHaveLength(3);

  // Pause is atomic across the whole subtree: root + every child. Nothing is
  // leased between ticks, so they suspend outright with their wait states intact.
  await ctx.env.pause(root);
  expect(await ctx.env.status(root)).toBe("paused waiting");
  expect(
    await ctx.env.statuses({ c0: kids[0], c1: kids[1], c2: kids[2] }),
  ).toEqual({ c0: "paused", c1: "paused", c2: "paused" });

  // Nothing in the fan-out is claimable while it is suspended.
  expect(await ctx.env.tick()).toBe(0);

  // Resume, and the batch runs to completion as if it had never stopped —
  // including the root's collect of all three children's outputs.
  await ctx.env.resume(root);
  await ctx.env.tickUntilIdle();
  expect(
    await ctx.env.statuses({ root, c0: kids[0], c1: kids[1], c2: kids[2] }),
  ).toEqual({
    root: "completed",
    c0: "completed",
    c1: "completed",
    c2: "completed",
  });
});

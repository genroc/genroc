/** Cancel × failure in a gp → parent → {a: fails, b: succeeds} tree (conventions: tree_error_pause_test.ts).
 *  A cancel outranks a failure — the reverse of pause — because it is terminal: FailAncestors excludes
 *  both cancel states, so the operator's stop stands. */
import { expect, test, beforeAll, afterAll } from "vitest";
import { startMockService } from "../helpers/client.ts";
import { useTickEnv } from "./helpers.ts";

const ctx = useTickEnv();

let stopMocks: (() => Promise<void>) | undefined;
let failWorkerName: string;
let successWorkerName: string;
let parentName: string;
let gpName: string;

beforeAll(async () => {
  const uid = crypto.randomUUID().slice(0, 8);
  failWorkerName = `cfail_worker_${uid}`;
  successWorkerName = `csuccess_worker_${uid}`;
  parentName = `cerr_parent_${uid}`;
  gpName = `cerr_gp_${uid}`;

  const failMock = await startMockService(0, { statusCode: 500 });
  const successMock = await startMockService(0, { response: { ok: true } });
  stopMocks = async () => {
    await failMock.stop();
    await successMock.stop();
  };

  for (const [name, port] of [
    [failWorkerName, failMock.port],
    [successWorkerName, successMock.port],
  ] as const) {
    await ctx.env.define(name, [
      {
        id: "work",
        action: { type: "fetch" as const, method: "post", url: `http://localhost:${port}/action`, timeout: 5_000 },
        switch: [{ goto: "end" }],
      },
    ]);
  }

  await ctx.env.define(parentName, [
    {
      id: "run_children",
      action: {
        type: "child_map" as const,
        children: { a: { name: failWorkerName }, b: { name: successWorkerName } },
      },
      switch: [{ goto: "end" }],
    },
  ]);
  await ctx.env.define(gpName, [
    {
      id: "run_parent",
      action: { type: "child_map" as const, children: { out: { name: parentName } } },
      switch: [{ goto: "end" }],
    },
  ]);
}, 60_000);

afterAll(() => stopMocks?.());

async function buildTree() {
  const gp = await ctx.env.start(gpName);
  await ctx.env.tick();
  const parent = await ctx.env.childOf(gp, "run_parent");
  await ctx.env.tick();
  const { a, b } = await ctx.env.childrenOf(parent, "run_children");
  expect(await ctx.env.statuses({ gp, parent, a, b })).toEqual({
    gp: "running children",
    parent: "running children",
    a: "running",
    b: "running",
  });
  return { gp, parent, a, b };
}

test("cancelling a draining tree stops it as cancelled, not failed", async () => {
  const { gp, parent, a, b } = await buildTree();

  // tick: `a` fails, so its ancestors enter the drain and wait for `b`.
  await ctx.env.tick();
  expect(await ctx.env.statuses({ gp, parent, a, b })).toEqual({
    gp: "failing children",
    parent: "failing children",
    a: "failed",
    b: "running",
  });

  // The selector takes 'failing' because the drain is when an operator reaches for cancel;
  // pause's 'running'-only selector would write nothing here.
  expect(await ctx.env.cancel(gp)).toBe("applied");
  expect(await ctx.env.statuses({ gp, parent, a, b })).toEqual({
    gp: "cancelled children",
    parent: "cancelled children",
    a: "failed", // already terminal: the work really did break, and that is kept
    b: "cancelled",
  });

  // settleFailing must not run: it would overwrite the stop with the fault and make a cancelled
  // tree retryable.
  expect(await ctx.env.tick()).toBe(0);
  expect(await ctx.env.statuses({ gp, parent })).toEqual({
    gp: "cancelled children",
    parent: "cancelled children",
  });
});

test("a failure landing after the cancel cannot reopen the tree", async () => {
  const { gp, parent, a, b } = await buildTree();

  // Cancel BEFORE anything fails, so the doomed child is stopped mid-flight rather than
  // after. `a` is cancelled with its 500 never sent.
  expect(await ctx.env.cancel(gp)).toBe("applied");
  expect(await ctx.env.statuses({ gp, parent, a, b })).toEqual({
    gp: "cancelled children",
    parent: "cancelled children",
    a: "cancelled",
    b: "cancelled",
  });

  // Nothing is claimable, so the failure that would have happened never does -- and there
  // is no path by which FailAncestors could mark a cancelled ancestor 'failing' anyway.
  expect(await ctx.env.tick()).toBe(0);
  expect(await ctx.env.tick()).toBe(0);
  expect(await ctx.env.statuses({ gp, parent, a, b })).toEqual({
    gp: "cancelled children",
    parent: "cancelled children",
    a: "cancelled",
    b: "cancelled",
  });
});

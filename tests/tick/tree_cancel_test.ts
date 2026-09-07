/**
 * Cancel through the same 3-level tree tree_pause_test.ts uses:
 *
 *   grandparent
 *     └─ parent  (child call)
 *          ├─ a  (child_map)
 *          └─ b  (child_map)
 *
 * Manual-tick mode (--poll 0) so every transition is inspectable between ticks.
 *
 * The contrast with pause is the whole point of the file. Pause changes the status column
 * and nothing else, so the tree stays structurally mid-flight and resume is the same flip
 * in reverse. Cancel is terminal: the tree stops, no tick advances it again, and there is
 * no verb that takes it back.
 */
import { expect, test, beforeAll, afterAll } from "vitest";
import { startMockService } from "../helpers/client.ts";
import { useTickEnv } from "./helpers.ts";

const PORT = 20019;
const ctx = useTickEnv(PORT);

let stopMock: () => Promise<void>;
let workerName: string;
let parentName: string;
let gpName: string;

beforeAll(async () => {
  const uid = crypto.randomUUID().slice(0, 8);
  workerName = `cworker_${uid}`;
  parentName = `cparent_${uid}`;
  gpName = `cgp_${uid}`;

  const mock = await startMockService(0, { response: {} });
  stopMock = mock.stop;

  await ctx.env.define(workerName, [
    {
      id: "work",
      action: { type: "fetch" as const, url: `http://localhost:${mock.port}/action` },
      timeout: 5_000,
      switch: [{ goto: "end" }],
    },
  ]);
  await ctx.env.define(parentName, [
    {
      id: "run_children",
      action: {
        type: "child_map" as const,
        children: { a: { name: workerName }, b: { name: workerName } },
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

afterAll(() => stopMock?.());

async function buildTree() {
  const gp = await ctx.env.start(gpName);
  await ctx.env.tick();
  const parent = await ctx.env.childOf(gp, "run_parent");
  await ctx.env.tick();
  const { a, b } = await ctx.env.childrenOf(parent, "run_children");

  expect(await ctx.env.statuses({ gp, parent, a, b })).toEqual({
    gp: "running waiting",
    parent: "running waiting",
    a: "running",
    b: "running",
  });
  return { gp, parent, a, b };
}

test("cancel grandparent — the whole tree stops at once, and no tick revives it", async () => {
  const { gp, parent, a, b } = await buildTree();

  // Nothing is leased between ticks, so there is no in-flight task to drain and nothing
  // lands in 'cancelling': the subtree is stopped by the cancel itself.
  expect(await ctx.env.cancel(gp)).toBe("applied");
  expect(await ctx.env.statuses({ gp, parent, a, b })).toEqual({
    gp: "cancelled waiting",
    parent: "cancelled waiting",
    a: "cancelled",
    b: "cancelled",
  });

  // The wait states survive, exactly as under a pause: cancel writes the status column and
  // nothing else, so a stopped tree still records what each node was doing. gp and parent
  // were mid child-process cycle and still say so.
  expect(await ctx.env.waitState(gp)).toBe("waiting");
  expect(await ctx.env.waitState(parent)).toBe("waiting");

  // Terminal means terminal: no node is claimable, and repeated ticks change nothing.
  expect(await ctx.env.tick()).toBe(0);
  expect(await ctx.env.tick()).toBe(0);
  expect(await ctx.env.statuses({ gp, parent, a, b })).toEqual({
    gp: "cancelled waiting",
    parent: "cancelled waiting",
    a: "cancelled",
    b: "cancelled",
  });
});

test("cancel is root-only — a descendant is refused, leaving the tree running", async () => {
  const { gp, parent, a, b } = await buildTree();
  try {
    await expect(ctx.env.cancel(parent)).rejects.toThrow();
    // The refusal wrote nothing: a partially stopped tree is the state this rule exists
    // to prevent, since the surviving half has no one left to report to.
    expect(await ctx.env.statuses({ gp, parent, a, b })).toEqual({
      gp: "running waiting",
      parent: "running waiting",
      a: "running",
      b: "running",
    });
  } finally {
    await ctx.env.cancel(gp);
  }
});

test("cancel disposes of a paused tree, which nothing else can", async () => {
  const { gp, parent, a, b } = await buildTree();

  await ctx.env.pause(gp);
  expect(await ctx.env.statuses({ gp, parent, a, b })).toEqual({
    gp: "paused waiting",
    parent: "paused waiting",
    a: "paused",
    b: "paused",
  });

  // Pause suspends indefinitely and keeps every row, so without cancel this tree has no
  // way out except resuming it. That gap is the reason cancel exists.
  expect(await ctx.env.cancel(gp)).toBe("applied");
  expect(await ctx.env.statuses({ gp, parent, a, b })).toEqual({
    gp: "cancelled waiting",
    parent: "cancelled waiting",
    a: "cancelled",
    b: "cancelled",
  });
  expect(await ctx.env.tick()).toBe(0);
});

test("a cancel over a settled branch leaves the finished work alone", async () => {
  const { gp, parent, a, b } = await buildTree();

  // One tick settles `a` (spawned first); `b` is still running.
  await ctx.env.tick();
  expect(await ctx.env.statuses({ a, b })).toEqual({ a: "completed", b: "running" });

  await ctx.env.cancel(gp);
  // The completed child keeps its outcome: the work really did happen, and rewriting it
  // as cancelled would make the trail lie about what ran.
  expect(await ctx.env.statuses({ gp, parent, a, b })).toEqual({
    gp: "cancelled waiting",
    parent: "cancelled waiting",
    a: "completed",
    b: "cancelled",
  });
  expect(await ctx.env.tick()).toBe(0);
});

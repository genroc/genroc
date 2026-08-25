/**
 * Tests that observe how errors interact with pausing in a 3-level process tree:
 *
 *   grandparent
 *     └─ parent  (child call)
 *          ├─ a  (child_map)  ← always calls failWorker → HTTP 500 → fails
 *          └─ b  (child_map)  ← calls successWorker → HTTP 200 → completes
 *
 * Key invariants:
 *   - Ancestors drain through 'failing' (keeping wait_state) and settle to
 *     'failed' one level per tick, bottom-up — a root is 'failed' only once
 *     its whole tree is inactive, which is what makes it retryable.
 *   - A failure is an outcome and a pause is not, so failures propagate through
 *     paused ancestors: FailAncestors marks them 'failing' rather than letting a
 *     suspension hide a dead branch.
 *   - But a failing parent still waits for every child, and a paused child counts
 *     as active, so a tree that loses a branch while suspended sits at 'failing'
 *     until it is resumed. Resuming is what lets it settle — and only then is it
 *     retryable. That is why resume keys on the subtree rather than the root's own
 *     status: here the root is 'failing' while its descendants are paused.
 *
 * Same server/tick/ordering conventions as tree_pause_test.ts; see that file for details.
 *
 * buildTree() leaves the tree at:
 *   gp="running waiting", parent="running waiting", a="running", b="running"
 */
import { expect, test, beforeAll, afterAll } from "vitest";
import { startMockService } from "../helpers/client.ts";
import { useTickEnv } from "./helpers.ts";

const PORT = 20015;
const ctx = useTickEnv(PORT);

let failMockPort: number;
let successMockPort: number;
let stopMocks: (() => Promise<void>) | undefined;
let failWorkerName: string;
let successWorkerName: string;
let parentName: string;
let gpName: string;

beforeAll(async () => {
  const uid = crypto.randomUUID().slice(0, 8);
  failWorkerName = `fail_worker_${uid}`;
  successWorkerName = `success_worker_${uid}`;
  parentName = `parent_${uid}`;
  gpName = `gp_${uid}`;

  const failMock = await startMockService(0, { statusCode: 500 });
  const successMock = await startMockService(0, { response: { ok: true } });
  failMockPort = failMock.port;
  successMockPort = successMock.port;
  stopMocks = async () => {
    await failMock.stop();
    await successMock.stop();
  };

  await ctx.env.define(failWorkerName, [
    {
      id: "work",
      action: {
        type: "fetch" as const,
        url: `http://localhost:${failMockPort}/action`,
      },
      timeout: 5_000,
      switch: [{ goto: "end" }],
    },
  ]);

  await ctx.env.define(successWorkerName, [
    {
      id: "work",
      action: {
        type: "fetch" as const,
        url: `http://localhost:${successMockPort}/action`,
      },
      timeout: 5_000,
      switch: [{ goto: "end" }],
    },
  ]);

  await ctx.env.define(parentName, [
    {
      id: "run_children",
      action: {
        type: "child_map" as const,
        children: {
          a: { name: failWorkerName },
          b: { name: successWorkerName },
        },
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

// Builds the full tree and leaves it at:
//   gp="running waiting", parent="running waiting", a="running", b="running"
async function buildTree() {
  const gp = await ctx.env.start(gpName);

  // tick: gp spawns parent → gp transitions to running+wait_state=waiting
  await ctx.env.tick();
  const parent = await ctx.env.childOf(gp, "run_parent");

  // tick: parent spawns a and b → parent transitions to running+wait_state=waiting
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

test("a fails — ancestors drain through 'failing' and settle to 'failed' one level per tick", async () => {
  const { gp, parent, a, b } = await buildTree();
  try {
    // tick: a (smaller created_at) is claimed and executed; its REST call returns 500.
    // failInstance(a) → FailInstanceAndAncestors: a is failed (terminal), parent
    // and gp become 'failing' but keep wait_state='waiting' — b is still active.
    await ctx.env.tick();
    expect(await ctx.env.statuses({ gp, parent, a, b })).toEqual({
      gp: "failing waiting",
      parent: "failing waiting",
      a: "failed",
      b: "running",
    });

    // tick: b runs and completes normally. FinishChild(b): all batch children
    // terminal → parent woken (wait_state '', now claimable). Never
    // 'collecting' — a failing parent must not merge outputs.
    await ctx.env.tick();
    expect(await ctx.env.statuses({ gp, parent, a, b })).toEqual({
      gp: "failing waiting",
      parent: "failing",
      a: "failed",
      b: "completed",
    });

    // tick: parent (failing, claimable) settles to 'failed'; its terminal save
    // wakes gp (wait_state '').
    await ctx.env.tick();
    expect(await ctx.env.statuses({ gp, parent, a, b })).toEqual({
      gp: "failing",
      parent: "failed",
      a: "failed",
      b: "completed",
    });

    // tick: gp settles to 'failed' — the root is terminal only now that the
    // whole tree is inactive, so 'failed' means retryable.
    await ctx.env.tick();
    expect(await ctx.env.statuses({ gp, parent, a, b })).toEqual({
      gp: "failed",
      parent: "failed",
      a: "failed",
      b: "completed",
    });
  } finally {
    await ctx.env.tickUntilIdle();
  }
});

test("a fails while the tree is paused — failure propagates, and resume unblocks the settle", async () => {
  // A separate tree whose failing worker holds its first request open, so the
  // root can be paused while a's call is still in flight. The failure then
  // lands on paused ancestors and must override them to 'failing'.
  const uid = crypto.randomUUID().slice(0, 8);
  const holdMock = await startMockService(0, {
    statusCode: 500,
    firstRequestDelayMs: Infinity,
  });
  try {
    const holdWorker = `hold_worker_${uid}`;
    await ctx.env.define(holdWorker, [
      {
        id: "work",
        action: {
          type: "fetch" as const,
          url: `http://localhost:${holdMock.port}/action`,
        },
        timeout: 5_000,
        switch: [{ goto: "end" }],
      },
    ]);
    const parent2Name = `parent2_${uid}`;
    await ctx.env.define(parent2Name, [
      {
        id: "run_children",
        action: {
          type: "child_map" as const,
          children: {
            a: { name: holdWorker },
            b: { name: successWorkerName },
          },
        },
        switch: [{ goto: "end" }],
      },
    ]);
    const gp2Name = `gp2_${uid}`;
    await ctx.env.define(gp2Name, [
      {
        id: "run_parent",
        action: { type: "child_map" as const, children: { out: { name: parent2Name } } },
        switch: [{ goto: "end" }],
      },
    ]);

    const gp = await ctx.env.start(gp2Name);
    await ctx.env.tick();
    const parent = await ctx.env.childOf(gp, "run_parent");
    await ctx.env.tick();
    const { a, b } = await ctx.env.childrenOf(parent, "run_children");

    // Tick 3 claims a (spawned first); its call hangs on the held mock.
    const tickPromise = ctx.env.tick();
    await holdMock.firstRequestReceived;

    // Pause the root while a's call is in flight. a is leased, so it is the one
    // node that lands in 'pausing' — it still has a task to finish. Everyone else
    // has nothing in flight and is suspended outright.
    await ctx.env.pause(gp);
    expect(await ctx.env.statuses({ gp, parent, a, b })).toEqual({
      gp: "paused waiting",
      parent: "paused waiting",
      a: "pausing",
      b: "paused",
    });

    // The audit trail distinguishes the two: the nodes suspended outright say
    // inst_paused, the in-flight one says only inst_pausing (it is not paused yet),
    // and the root's request records how many were left draining.
    const eventsOn = async (id: string) => {
      const { data } = await ctx.env.client.GET("/instances/{id}/logs", {
        params: { path: { id }, query: { limit: 100 } },
      });
      return (data!.items ?? []).map((l) => l.event);
    };
    expect(await eventsOn(a)).toContain("inst_pausing");
    expect(await eventsOn(a)).not.toContain("inst_paused");
    expect(await eventsOn(b)).toContain("inst_paused");
    const { data: rootLogs } = await ctx.env.client.GET("/instances/{id}/logs", {
      params: { path: { id: gp }, query: { limit: 100 } },
    });
    expect(
      rootLogs!.items!.find((l) => l.event === "inst_pause_requested")!.meta,
    ).toMatchObject({ instances: 4, pausing: 1 });

    // Release the held 500. The engine still holds a as in-memory 'running', so the
    // failure path runs: failInstance(a) → FailAncestors, whose predicate includes
    // paused rows — a failure is a real outcome and must not be hidden by a
    // suspension. The ancestors become 'failing', keeping wait_state='waiting'.
    holdMock.release();
    await tickPromise;

    expect(await ctx.env.statuses({ gp, parent, a, b })).toEqual({
      gp: "failing waiting",
      parent: "failing waiting",
      a: "failed",
      b: "paused",
    });

    // The tree is now wedged, and legitimately so: parent is failing but still
    // waiting on b, and b is paused, so it will never settle on its own. Ticking
    // changes nothing, and retry is rejected — the root is draining, not failed.
    expect(await ctx.env.tick()).toBe(0);
    const { error: earlyRetryErr } = await ctx.env.client.POST(
      "/instances/{id}/retry",
      { params: { path: { id: gp } } },
    );
    expect(earlyRetryErr).toBeDefined();
    expect(JSON.stringify(earlyRetryErr)).toContain("not retryable");

    // Resume is what unblocks it. The root's own status is 'failing', not paused,
    // so this only works because resume looks for paused rows anywhere in the
    // subtree — here just b.
    await ctx.env.resume(gp);
    expect(await ctx.env.statuses({ gp, parent, a, b })).toEqual({
      gp: "failing waiting",
      parent: "failing waiting",
      a: "failed",
      b: "running",
    });

    // tick: b runs and completes. FinishChild(b): all batch children terminal →
    // parent woken (wait_state '' — a failing parent must not merge outputs).
    await ctx.env.tick();
    expect(await ctx.env.statuses({ gp, parent, a, b })).toEqual({
      gp: "failing waiting",
      parent: "failing",
      a: "failed",
      b: "completed",
    });

    // tick: parent settles failing → failed; its terminal save wakes gp.
    await ctx.env.tick();
    expect(await ctx.env.statuses({ gp, parent, a, b })).toEqual({
      gp: "failing",
      parent: "failed",
      a: "failed",
      b: "completed",
    });

    // tick: gp settles failing → failed — only now is the root retryable.
    await ctx.env.tick();
    expect(await ctx.env.statuses({ gp, parent, a, b })).toEqual({
      gp: "failed",
      parent: "failed",
      a: "failed",
      b: "completed",
    });

    // The error from 'a' is propagated to ancestors via FailAncestors.
    const { data: parentInst } = await ctx.env.client.GET("/instances/{id}", {
      params: { path: { id: parent } },
    });
    expect(parentInst?.error_message).toBeTruthy();
  } finally {
    await ctx.env.tickUntilIdle();
    await holdMock.stop();
  }
});

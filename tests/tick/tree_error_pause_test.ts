/** Errors × pausing in a gp → parent → {a: fails, b: succeeds} tree; conventions as tree_pause_test.ts.
 *  A failure propagates through paused ancestors, but a failing parent waits on a paused child, so
 *  the tree settles to 'failed' (and becomes retryable) only after resume. */
import { expect, test, beforeAll, afterAll } from "vitest";
import { startMockService } from "../helpers/client.ts";
import { useTickEnv } from "./helpers.ts";

const ctx = useTickEnv();

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
        method: "post",
        url: `http://localhost:${failMockPort}/action`,
        timeout: 5_000,
      },
      switch: [{ goto: "end" }],
    },
  ]);

  await ctx.env.define(successWorkerName, [
    {
      id: "work",
      action: {
        type: "fetch" as const,
        method: "post",
        url: `http://localhost:${successMockPort}/action`,
        timeout: 5_000,
      },
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

async function buildTree() {
  const gp = await ctx.env.start(gpName);

  // tick: gp spawns parent → gp transitions to running+phase=waiting
  await ctx.env.tick();
  const parent = await ctx.env.childOf(gp, "run_parent");

  // tick: parent spawns a and b → parent transitions to running+phase=waiting
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

test("a fails — ancestors drain through 'failing' and settle to 'failed' one level per tick", async () => {
  const { gp, parent, a, b } = await buildTree();
  try {
    // tick: a (smaller created_at) runs first and 500s; ancestors go 'failing' but keep
    // phase='children' while b is active.
    await ctx.env.tick();
    expect(await ctx.env.statuses({ gp, parent, a, b })).toEqual({
      gp: "failing children",
      parent: "failing children",
      a: "failed",
      b: "running",
    });

    // tick: b completes and wakes parent (phase ''). Never 'collecting' — a failing parent
    // must not merge outputs.
    await ctx.env.tick();
    expect(await ctx.env.statuses({ gp, parent, a, b })).toEqual({
      gp: "failing children",
      parent: "failing",
      a: "failed",
      b: "completed",
    });

    // tick: parent (failing, claimable) settles to 'failed'; its terminal save
    // wakes gp (phase '').
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
  // The failing worker holds its first request open, so the root can be paused mid-call.
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
          method: "post",
          url: `http://localhost:${holdMock.port}/action`,
          timeout: 5_000,
        },
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

    // a is leased, so it alone lands in 'pausing'; the rest have nothing in flight.
    await ctx.env.pause(gp);
    expect(await ctx.env.statuses({ gp, parent, a, b })).toEqual({
      gp: "paused children",
      parent: "paused children",
      a: "pausing",
      b: "paused",
    });

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

    // Nothing is left to write, so `accepted` comes entirely from CountDrainingInTree;
    // "unchanged" would read a tree with a worker mid-task as stopped.
    expect(await ctx.env.pause(gp)).toBe("accepted");

    // a is still in-memory 'running', so FailAncestors runs, and its predicate includes
    // paused rows: a failure must not be hidden by a suspension.
    holdMock.release();
    await tickPromise;

    expect(await ctx.env.statuses({ gp, parent, a, b })).toEqual({
      gp: "failing children",
      parent: "failing children",
      a: "failed",
      b: "paused",
    });

    // Wedged, legitimately: parent waits on b, which is paused. The root is draining, not failed.
    expect(await ctx.env.tick()).toBe(0);
    const { error: earlyRetryErr } = await ctx.env.client.POST(
      "/instances/{id}/retry",
      { params: { path: { id: gp } } },
    );
    expect(earlyRetryErr).toBeDefined();
    expect(JSON.stringify(earlyRetryErr)).toContain("not retryable");

    // The root itself is 'failing', so this works only because resume finds paused rows
    // anywhere in the subtree.
    await ctx.env.resume(gp);
    expect(await ctx.env.statuses({ gp, parent, a, b })).toEqual({
      gp: "failing children",
      parent: "failing children",
      a: "failed",
      b: "running",
    });

    // tick: b runs and completes. FinishChild(b): all batch children terminal →
    // parent woken (phase '' — a failing parent must not merge outputs).
    await ctx.env.tick();
    expect(await ctx.env.statuses({ gp, parent, a, b })).toEqual({
      gp: "failing children",
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

/** Errors × pausing in a gp → parent → {a: fails, b: succeeds} tree; conventions as tree_pause_test.ts.
 *  A failure pauses the rest of its tree and propagates through paused ancestors; a paused row is
 *  not busy, so the tree settles to 'failed' (and becomes retryable) as soon as nothing is in flight. */
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

test("a fails — the rest of the tree pauses, and the root fails in the same tick", async () => {
  const { gp, parent, a, b } = await buildTree();
  try {
    // tick: a (smaller created_at) runs first and 500s. b is not held, so it pauses outright and
    // nothing below parent is busy: the whole stack settles to 'failed' in a's own write.
    await ctx.env.tick();
    expect(await ctx.env.statuses({ gp, parent, a, b })).toEqual({
      gp: "failed",
      parent: "failed",
      a: "failed",
      b: "paused",
    });
    expect(await ctx.env.tick(), "a doomed sibling must not run").toBe(0);

    // Resume would run b under a parent that can no longer collect it; retry is the way back.
    const { error: resumeErr } = await ctx.env.client.POST("/instances/{id}/resume", {
      params: { path: { id: gp } },
    });
    expect(JSON.stringify(resumeErr)).toContain("retry");
    expect(await ctx.env.status(b)).toBe("paused");

    // Retry revives the failure and resumes what it paused, keeping b's place.
    await ctx.env.retry(gp);
    expect(await ctx.env.statuses({ gp, parent, a, b })).toEqual({
      gp: "running children",
      parent: "running children",
      a: "running",
      b: "running",
    });
  } finally {
    await ctx.env.tickUntilIdle();
  }
});

test("a fails while the tree is pausing — the failure outranks the pause and settles at once", async () => {
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

    // a is leased, so it and every row above it drain; b has nothing in flight and stops now.
    expect(await ctx.env.pause(gp)).toBe("accepted");
    expect(await ctx.env.statuses({ gp, parent, a, b })).toEqual({
      gp: "pausing children",
      parent: "pausing children",
      a: "pausing",
      b: "paused",
    });

    const eventsOn = async (id: string) => {
      const { data } = await ctx.env.client.GET("/instances/{id}/logs", {
        params: { path: { id }, query: { limit: 100 } },
      });
      return (data!.items ?? []).map((l) => l.event);
    };
    for (const id of [gp, parent, a]) {
      expect(await eventsOn(id), `inst_pausing on ${id}`).toContain("inst_pausing");
    }
    expect(await eventsOn(b)).toContain("inst_paused");
    const { data: rootLogs } = await ctx.env.client.GET("/instances/{id}/logs", {
      params: { path: { id: gp }, query: { limit: 100 } },
    });
    expect(
      rootLogs!.items!.find((l) => l.event === "inst_pause_requested")!.meta,
    ).toMatchObject({ instances: 4, pausing: 3 });

    // Nothing is left to write, and the root's own 'pausing' is what says the tree is still
    // draining; "unchanged" would read a tree with a worker mid-task as stopped.
    expect(await ctx.env.pause(gp)).toBe("accepted");

    // a is still in-memory 'running', so FailAncestors runs, and its predicate includes the
    // draining rows: a failure must not be hidden by a suspension. b is paused, not busy, so the
    // failure settles all the way up in a's write.
    holdMock.release();
    await tickPromise;
    expect(await ctx.env.statuses({ gp, parent, a, b })).toEqual({
      gp: "failed",
      parent: "failed",
      a: "failed",
      b: "paused",
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

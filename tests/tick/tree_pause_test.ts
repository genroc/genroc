/** Pausing a gp → parent → {a, b} tree under manual ticks; status() is "status phase", trimmed.
 *  A pause flips only the status column — every node keeps its phase, so resume reconstructs nothing. */
import { expect, test, beforeAll, afterAll } from "vitest";
import { startMockService } from "../helpers/client.ts";
import { useTickEnv } from "./helpers.ts";

const ctx = useTickEnv();

let mockPort: number;
let stopMock: () => Promise<void>;
let workerName: string;
let parentName: string;
let gpName: string;

beforeAll(async () => {
  const uid = crypto.randomUUID().slice(0, 8);
  workerName = `worker_${uid}`;
  parentName = `parent_${uid}`;
  gpName = `gp_${uid}`;

  const mock = await startMockService(0, { response: {} });
  mockPort = mock.port;
  stopMock = mock.stop;

  await ctx.env.define(workerName, [
    {
      id: "work",
      action: {
        type: "fetch" as const,
        method: "post",
        url: `http://localhost:${mockPort}/action`,
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

test("happy path — tree completes when ticked to completion", async () => {
  const { gp, parent, a, b } = await buildTree();
  try {
    // tick: a (spawned first) completes; b still running, parent stays waiting
    await ctx.env.tick();
    expect(await ctx.env.statuses({ gp, parent, a, b })).toEqual({
      gp: "running children",
      parent: "running children",
      a: "completed",
      b: "running",
    });

    // tick: b completes; count = 0 → parent.phase = 'collecting'
    await ctx.env.tick();
    expect(await ctx.env.statuses({ gp, parent, a, b })).toEqual({
      gp: "running children",
      parent: "running collecting",
      a: "completed",
      b: "completed",
    });

    // tick: parent (running+collecting) collects outputs, advances to end → completed
    //       FinishChild(parent): gp.phase = 'collecting'
    await ctx.env.tick();
    expect(await ctx.env.statuses({ gp, parent, a, b })).toEqual({
      gp: "running collecting",
      parent: "completed",
      a: "completed",
      b: "completed",
    });

    // tick: gp (running+collecting) collects output, advances to end → completed
    await ctx.env.tick();
    expect(await ctx.env.statuses({ gp, parent, a, b })).toEqual({
      gp: "completed",
      parent: "completed",
      a: "completed",
      b: "completed",
    });
  } finally {
    await ctx.env.tickUntilIdle();
  }
});

test("pause grandparent — whole tree suspends at once, keeping its wait states", async () => {
  const { gp, parent, a, b } = await buildTree();
  try {
    await ctx.env.pause(gp);

    // Nothing is leased between ticks, so nothing lands in 'pausing'; gp and parent keep
    // phase='children' — a pause does not unwind the child cycle.
    expect(await ctx.env.statuses({ gp, parent, a, b })).toEqual({
      gp: "paused children",
      parent: "paused children",
      a: "paused",
      b: "paused",
    });

    // Nothing in the tree is claimable, so ticking does not advance any of it.
    expect(await ctx.env.tick()).toBe(0);
    expect(await ctx.env.statuses({ gp, parent, a, b })).toEqual({
      gp: "paused children",
      parent: "paused children",
      a: "paused",
      b: "paused",
    });

    // Resume is the same flip in reverse: nothing restarts or re-spawns.
    await ctx.env.resume(gp);
    expect(await ctx.env.statuses({ gp, parent, a, b })).toEqual({
      gp: "running children",
      parent: "running children",
      a: "running",
      b: "running",
    });

    await ctx.env.tickUntilIdle();
    expect(await ctx.env.statuses({ gp, parent, a, b })).toEqual({
      gp: "completed",
      parent: "completed",
      a: "completed",
      b: "completed",
    });
  } finally {
    await ctx.env.tickUntilIdle();
  }
});

test("pause mid-flight — children that completed stay completed on resume", async () => {
  const { gp, parent, a, b } = await buildTree();
  try {
    // tick: a completes, b is still running.
    await ctx.env.tick();

    await ctx.env.pause(gp);
    // Completed work is untouched by a pause — only live nodes are suspended.
    expect(await ctx.env.statuses({ gp, parent, a, b })).toEqual({
      gp: "paused children",
      parent: "paused children",
      a: "completed",
      b: "paused",
    });

    await ctx.env.resume(gp);
    // b picks up its pending task; a is never re-run.
    await ctx.env.tickUntilIdle();
    expect(await ctx.env.statuses({ gp, parent, a, b })).toEqual({
      gp: "completed",
      parent: "completed",
      a: "completed",
      b: "completed",
    });
  } finally {
    await ctx.env.tickUntilIdle();
  }
});

// A parent woken to 'collecting' has not merged yet; losing that phase on pause would skip the
// collect and drop its children's results.
test("pause while collecting — the merge still happens on resume", async () => {
  const { gp, parent, a, b } = await buildTree();
  try {
    // Two ticks: a and b both complete, which wakes parent to 'collecting'.
    await ctx.env.tick();
    await ctx.env.tick();
    expect(await ctx.env.status(parent)).toBe("running collecting");

    await ctx.env.pause(gp);
    expect(await ctx.env.statuses({ gp, parent, a, b })).toEqual({
      gp: "paused children",
      parent: "paused collecting",
      a: "completed",
      b: "completed",
    });
    expect(await ctx.env.tick()).toBe(0);

    await ctx.env.resume(gp);
    expect(await ctx.env.status(parent)).toBe("running collecting");

    await ctx.env.tickUntilIdle();
    expect(await ctx.env.statuses({ gp, parent, a, b })).toEqual({
      gp: "completed",
      parent: "completed",
      a: "completed",
      b: "completed",
    });
  } finally {
    await ctx.env.tickUntilIdle();
  }
});

// Pause is audited on the root (info: its outcome can be deferred) and per node (debug); resume
// is atomic, so it is per node only.
test("pause and resume are recorded on every instance, with a root entry for the pause", async () => {
  const { gp, parent, a, b } = await buildTree();
  try {
    await ctx.env.pause(gp);
    await ctx.env.resume(gp);

    // flat: a root id otherwise answers with its whole tree's trail.
    const logsOf = async (id: string, flat = true) => {
      const { data } = await ctx.env.client.GET("/instances/{id}/logs", {
        params: { path: { id }, query: { limit: 100, flat } },
      });
      return data!.items ?? [];
    };

    // Nothing was leased between ticks, so none were left draining.
    const rootLogs = await logsOf(gp, false); // the tree read, which carries the root's own rows too
    const requested = rootLogs.find((l) => l.event === "inst_pause_requested");
    expect(requested).toBeDefined();
    expect(requested!.level).toBe("info");
    expect(requested!.meta).toMatchObject({ instances: 4, pausing: 0 });

    expect(rootLogs.map((l) => l.event)).not.toContain("inst_resume_requested");

    for (const id of [gp, parent, a, b]) {
      const items = await logsOf(id);
      const paused = items.find((l) => l.event === "inst_paused");
      expect(paused, `inst_paused on ${id}`).toBeDefined();
      expect(paused!.level).toBe("debug");
      const resumed = items.find((l) => l.event === "inst_resumed");
      expect(resumed, `inst_resumed on ${id}`).toBeDefined();
      expect(resumed!.level).toBe("debug");
    }

    // A rejected call leaves no trace: logging happens only after the commit.
    await ctx.env.client.POST("/instances/{id}/resume", {
      params: { path: { id: gp } },
    });
    expect(
      (await logsOf(gp)).filter((l) => l.event === "inst_resumed"),
    ).toHaveLength(1);

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

test("pause/resume non-root — rejected naming the root; tree unaffected", async () => {
  const { gp, parent, a, b } = await buildTree();
  try {
    // Suspending is a whole-tree decision: only the root is accepted.
    for (const path of ["pause", "resume"] as const) {
      for (const id of [parent, a]) {
        const { error } = await ctx.env.client.POST(`/instances/{id}/${path}`, {
          params: { path: { id } },
        });
        expect(error).toBeDefined();
        expect(JSON.stringify(error)).toContain(gp);
      }
    }

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

test("resume changes nothing when the tree is already advancing", async () => {
  const { gp, parent, a, b } = await buildTree();
  try {
    // A live tree already satisfies resume, so it writes nothing; a settled one stays a
    // conflict. specs/id-list-commands.md.
    expect(await ctx.env.resume(gp)).toBe("unchanged");

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

// A pause landing while a child is mid-call drains up the tree: the child's own write keeps the
// pause, its next claim settles it, and that settle climbs, each parent logging its own landing.
test("pause mid-call — the ancestors drain, then settle together once the child lands", async () => {
  const uid = crypto.randomUUID().slice(0, 8);
  const held = await startMockService(0, { response: {}, firstRequestDelayMs: Infinity });
  try {
    const leaf = `held_leaf_${uid}`;
    await ctx.env.define(leaf, [
      {
        id: "work",
        action: { type: "fetch" as const, method: "post", url: `http://localhost:${held.port}/a`, timeout: 30_000 },
        switch: [{ goto: "next" }],
      },
      { id: "after", switch: [{ goto: "end" }] },
    ]);
    const mid = `held_mid_${uid}`;
    await ctx.env.define(mid, [
      { id: "run_children", action: { type: "child_map" as const, children: { a: { name: leaf } } }, switch: [{ goto: "end" }] },
    ]);
    const top = `held_gp_${uid}`;
    await ctx.env.define(top, [
      { id: "run_parent", action: { type: "child_map" as const, children: { out: { name: mid } } }, switch: [{ goto: "end" }] },
    ]);
    const gp = await ctx.env.start(top);
    await ctx.env.tick();
    const parent = await ctx.env.childOf(gp, "run_parent");
    await ctx.env.tick();
    const a = await ctx.env.childOf(parent, "run_children");

    const inFlight = ctx.env.tick();
    await held.firstRequestReceived;
    expect(await ctx.env.pause(gp)).toBe("accepted");
    expect(await ctx.env.statuses({ gp, parent, a })).toEqual({
      gp: "pausing children",
      parent: "pausing children",
      a: "pausing",
    });

    held.release();
    await inFlight;
    // a's task finished and moved on; its write kept the pause, so nothing has stopped yet.
    expect(await ctx.env.statuses({ gp, parent, a })).toEqual({
      gp: "pausing children",
      parent: "pausing children",
      a: "pausing",
    });

    expect(await ctx.env.tick()).toBe(1); // a's settle, which climbs to gp in the same write
    expect(await ctx.env.statuses({ gp, parent, a })).toEqual({
      gp: "paused children",
      parent: "paused children",
      a: "paused",
    });
    expect(await ctx.env.pause(gp)).toBe("unchanged");

    // Audit rows flush on a short ticker, so poll for the settle-up's entries.
    const settledOn = async (id: string) => {
      for (let i = 0; i < 40; i++) {
        const { data } = await ctx.env.client.GET("/instances/{id}/logs", {
          params: { path: { id }, query: { limit: 100, flat: true, level: "debug" } as never },
        });
        const hit = (data?.items ?? []).find((l) => l.event === "inst_paused");
        if (hit) return hit;
        await new Promise((r) => setTimeout(r, 50));
      }
      return undefined;
    };
    for (const id of [gp, parent, a]) {
      expect(await settledOn(id), `inst_paused on ${id}: every landing must be on the trail`).toBeDefined();
    }

    await ctx.env.resume(gp);
    await ctx.env.tickUntilIdle();
    expect(await ctx.env.status(gp)).toBe("completed");
  } finally {
    held.release();
    await ctx.env.tickUntilIdle();
    await held.stop();
  }
});

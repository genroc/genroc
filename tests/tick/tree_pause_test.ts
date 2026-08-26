/**
 * Tests that observe how pausing propagates through a 3-level process tree:
 *
 *   grandparent
 *     └─ parent  (child call)
 *          ├─ a  (child_map)
 *          └─ b  (child_map)
 *
 * The server runs in manual-tick mode (--poll 0, --max-concurrent 1) so every
 * DB state transition is inspectable between ticks.
 *
 * status() returns "status wait_state".trim(), e.g. "running waiting", "paused waiting".
 * Instances with no wait_state show just their status, e.g. "running", "paused".
 *
 * The thing to notice throughout: pausing changes the status column and nothing else.
 * Every node keeps the wait_state it had, so the tree is still structurally mid-flight
 * while suspended, and resuming needs no reconstruction — it is the same status flip
 * in reverse.
 *
 * buildTree() leaves the tree at:
 *   gp="running waiting", parent="running waiting", a="running", b="running"
 */
import { expect, test, beforeAll, afterAll } from "vitest";
import { startMockService } from "../helpers/client.ts";
import { useTickEnv } from "./helpers.ts";

const PORT = 20014;
const ctx = useTickEnv(PORT);

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
        url: `http://localhost:${mockPort}/action`,
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

test("happy path — tree completes when ticked to completion", async () => {
  const { gp, parent, a, b } = await buildTree();
  try {
    // tick: a (spawned first) completes; b still running, parent stays waiting
    await ctx.env.tick();
    expect(await ctx.env.statuses({ gp, parent, a, b })).toEqual({
      gp: "running waiting",
      parent: "running waiting",
      a: "completed",
      b: "running",
    });

    // tick: b completes; count = 0 → parent.wait_state = 'collecting'
    await ctx.env.tick();
    expect(await ctx.env.statuses({ gp, parent, a, b })).toEqual({
      gp: "running waiting",
      parent: "running collecting",
      a: "completed",
      b: "completed",
    });

    // tick: parent (running+collecting) collects outputs, advances to end → completed
    //       FinishChild(parent): gp.wait_state = 'collecting'
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

    // No node is leased between ticks, so there is no in-flight task to wait out and
    // nothing lands in 'pausing': the whole subtree is suspended by the pause itself.
    // gp and parent keep wait_state='waiting' — pausing suspends them, it does not
    // unwind the child-process cycle they are in the middle of.
    expect(await ctx.env.statuses({ gp, parent, a, b })).toEqual({
      gp: "paused waiting",
      parent: "paused waiting",
      a: "paused",
      b: "paused",
    });

    // Nothing in the tree is claimable, so ticking does not advance any of it.
    expect(await ctx.env.tick()).toBe(0);
    expect(await ctx.env.statuses({ gp, parent, a, b })).toEqual({
      gp: "paused waiting",
      parent: "paused waiting",
      a: "paused",
      b: "paused",
    });

    // Resume is the same flip in reverse: every node is exactly where it was, so the
    // tree carries on rather than restarting or re-spawning anything.
    await ctx.env.resume(gp);
    expect(await ctx.env.statuses({ gp, parent, a, b })).toEqual({
      gp: "running waiting",
      parent: "running waiting",
      a: "running",
      b: "running",
    });

    // And it runs to completion from there on the normal schedule.
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
      gp: "paused waiting",
      parent: "paused waiting",
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

// The third wait state: a parent whose children all finished has been woken to
// 'collecting' but has not merged their outputs yet. Pausing there must preserve that
// too, or the resumed parent would skip the collect and lose its children's results.
test("pause while collecting — the merge still happens on resume", async () => {
  const { gp, parent, a, b } = await buildTree();
  try {
    // Two ticks: a and b both complete, which wakes parent to 'collecting'.
    await ctx.env.tick();
    await ctx.env.tick();
    expect(await ctx.env.status(parent)).toBe("running collecting");

    await ctx.env.pause(gp);
    expect(await ctx.env.statuses({ gp, parent, a, b })).toEqual({
      gp: "paused waiting",
      parent: "paused collecting",
      a: "completed",
      b: "completed",
    });
    expect(await ctx.env.tick()).toBe(0);

    await ctx.env.resume(gp);
    expect(await ctx.env.status(parent)).toBe("running collecting");

    // The collect the pause interrupted runs, so the parent's output carries both
    // children — nothing was dropped by suspending mid-cycle.
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

// Pausing is audited at two granularities: the operator's action once on the root (info,
// because its outcome can be deferred), and the per-instance consequence on every node it
// touched (debug). Resuming is atomic, so it only has the per-instance half.
test("pause and resume are recorded on every instance, with a root entry for the pause", async () => {
  const { gp, parent, a, b } = await buildTree();
  try {
    await ctx.env.pause(gp);
    await ctx.env.resume(gp);

    const logsOf = async (id: string) => {
      const { data } = await ctx.env.client.GET("/instances/{id}/logs", {
        params: { path: { id }, query: { limit: 100 } },
      });
      return data!.items ?? [];
    };

    // The root carries the pause as an operator action, with the count of what was
    // actually live. Nothing was leased between ticks, so none were left draining.
    const rootLogs = await logsOf(gp);
    const requested = rootLogs.find((l) => l.event === "inst_pause_requested");
    expect(requested).toBeDefined();
    expect(requested!.level).toBe("info");
    expect(requested!.meta).toMatchObject({ instances: 4, pausing: 0 });

    // Resuming is atomic, so it has no root-level counterpart.
    expect(rootLogs.map((l) => l.event)).not.toContain("inst_resume_requested");

    // Every instance in the tree carries its own transition, at debug level.
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
      gp: "running waiting",
      parent: "running waiting",
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

    // The rejected calls left the tree untouched.
    expect(await ctx.env.statuses({ gp, parent, a, b })).toEqual({
      gp: "running waiting",
      parent: "running waiting",
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
    // Live and unpaused: "make this tree advance" already holds, so the assertion is
    // satisfied rather than refused — and writes nothing. A settled tree is the other
    // side of that split and stays a conflict. specs/id-list-commands.md.
    expect(await ctx.env.resume(gp)).toBe("unchanged");

    expect(await ctx.env.statuses({ gp, parent, a, b })).toEqual({
      gp: "running waiting",
      parent: "running waiting",
      a: "running",
      b: "running",
    });
  } finally {
    await ctx.env.tickUntilIdle();
  }
});

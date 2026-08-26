import { afterAll, beforeAll, describe, expect, test } from "vitest";
import { buildGenrocBinary, startGenroc, type GenrocProcess } from "../helpers/server.ts";
import { listAllInstances } from "../helpers/client.ts";

// A child task RE-ENTERED by a loop, under a real worker fleet.
//
// multi_worker_test.ts stresses recursion, which spawns forward: every batch belongs to a
// fresh instance, so it never exercises the case where one instance spawns repeatedly under
// the same (parent_id, spawn_task_id). That case is scoped by task_epoch, and everything
// that makes it interesting is concurrency: the parent's spawn and its collect are two
// different claims, by two different worker processes, with a lease handover and a
// pause/resume window in between. If the epoch is ever written by the wrong claim — or the
// resume is miscounted as a task entry — the parent collects a batch that is not its own.
//
// The exactly-once checksum here is a COUNT rather than an aggregate: a loop of N passes
// must leave exactly N children under the task, no more. A batch collected twice, a pass
// re-spawned after a pause, or a lost update that replays a spawn all show up as a surplus.
//
// Chaos is pause/resume ONLY, deliberately. A retry is an operator override that re-enters
// the task and legitimately spawns another batch (see tests/tick/task_epoch_test.ts), so
// including it would make the exact count meaningless — the property under test would be
// unstated rather than merely unasserted.
//
// Postgres only, for the same reason as the rest of this suite: a worker fleet is separate
// processes relying on FOR UPDATE SKIP LOCKED.

const DSN = process.env.POSTGRES_DSN;

const WORKER_COUNT = 3;
const ROOT_COUNT = 6;
const PASSES = 4; // children per root, one per loop pass
const CHAOS_MS = 4_000;
const SETTLE_MS = 60_000;

const sleep = (ms: number) => new Promise((r) => setTimeout(r, ms));
// `paused`/`pausing` are absent on purpose: a pause is not an outcome, just a tree nobody
// is advancing, so it never counts as settled.
const isTerminal = (s?: string) => s === "completed" || s === "failed";

describe.runIf(!!DSN)("child task in a loop — worker fleet, postgres", () => {
  let workers: GenrocProcess[] = [];

  beforeAll(async () => {
    const bin = await buildGenrocBinary();
    process.env.GENROC_PG_MAX_OPEN_CONNS = "8";
    // Sequential: the first process runs migrations before any other opens the DB.
    for (let i = 0; i < WORKER_COUNT; i++) {
      workers.push(await startGenroc(bin, 8960 + i, "", DSN, 5, 5, true));
    }
  }, 60_000);

  afterAll(() => {
    for (const w of workers) w.stop();
    workers = [];
  });

  test(
    "every pass collects its own batch while workers pause, resume and hand off leases",
    async () => {
      const api = workers[0].client;
      const leaf = `stress_loop_leaf_${crypto.randomUUID()}`;
      const looper = `stress_loop_${crypto.randomUUID()}`;

      await api.PUT("/definitions", {
        body: {
          name: leaf,
          tasks: [{ id: "t", output: { ok: "$: true" }, switch: [{ goto: "end" }] }],
          output: "$: outputs.t",
        } as never,
      });
      await api.PUT("/definitions", {
        body: {
          name: looper,
          tasks: [
            { id: "tick", output: { i: "$: (self.previous.i ?? 0) + 1" }, switch: [{ goto: "$call" }] },
            {
              id: "call",
              action: { type: "child", name: leaf },
              switch: [{ case: `outputs.tick.i >= ${PASSES}`, goto: "end" }, { goto: "$tick" }],
            },
          ],
          output: { rounds: "$: outputs.tick.i" },
        } as never,
      });

      const rootIds: string[] = [];
      for (let i = 0; i < ROOT_COUNT; i++) {
        const { data, error } = await api.POST("/instances", { body: { process: looper } as never });
        expect(error).toBeUndefined();
        rootIds.push(data!.id);
      }
      const randomRoot = () => rootIds[Math.floor(Math.random() * rootIds.length)];

      // Pause/resume window: the gap is what lands a pause between a spawn and its collect,
      // which is the handover this scoping has to survive. Errors (pausing a completed root,
      // resuming a running one) are part of the contention and ignored.
      let chaosOn = true;
      const pauser = (async () => {
        while (chaosOn) {
          const id = randomRoot();
          await api.POST("/instances/{id}/pause", { params: { path: { id } } }).catch(() => {});
          await sleep(20 + Math.random() * 40);
          await api.POST("/instances/{id}/resume", { params: { path: { id } } }).catch(() => {});
          await sleep(30 + Math.random() * 70);
        }
      })();

      await sleep(CHAOS_MS);
      chaosOn = false;
      await pauser;

      // Nothing advances a paused tree, so sweep before waiting for settlement.
      for (const id of rootIds) {
        await api.POST("/instances/{id}/resume", { params: { path: { id } } }).catch(() => {});
      }

      const mine = (i: { process?: string }) => i.process === looper || i.process === leaf;
      const deadline = Date.now() + SETTLE_MS;
      let instances: Awaited<ReturnType<typeof listAllInstances>> = [];
      while (Date.now() < deadline) {
        instances = (await listAllInstances(api)).filter(mine);
        for (const inst of instances) {
          if (inst.status === "paused" || inst.status === "pausing") {
            await api
              .POST("/instances/{id}/resume", { params: { path: { id: inst.id } } })
              .catch(() => {});
          }
        }
        if (instances.length > 0 && instances.every((i) => isTerminal(i.status))) break;
        await sleep(250);
      }

      const stuck = instances.filter((i) => !isTerminal(i.status));
      expect(stuck.map((i) => `${i.id}:${i.status}`), "nothing left mid-flight").toEqual([]);

      // No root may FAIL: pause/resume is non-destructive, and the collect error this whole
      // mechanism exists to prevent surfaces exactly here.
      const failed = instances.filter((i) => i.status === "failed");
      expect(failed.map((i) => `${i.id}:${i.error_message}`), "no tree failed").toEqual([]);

      for (const id of rootIds) {
        const { data } = await api.GET("/instances/{id}/detail", { params: { path: { id } } });
        expect(data?.status).toBe("completed");
        expect((data?.state?.output as { rounds?: number } | undefined)?.rounds).toBe(PASSES);
      }

      // The checksum: one child per pass and not one more.
      const children = instances.filter((i) => i.process === leaf);
      expect(children.length, "exactly one child per pass, per root").toBe(ROOT_COUNT * PASSES);
    },
    SETTLE_MS + CHAOS_MS + 60_000,
  );
});

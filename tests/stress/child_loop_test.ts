import { afterAll, beforeAll, describe, expect, test } from "vitest";
import { startGenroc, type GenrocProcess } from "../helpers/server.ts";
import { listAllInstances } from "../helpers/client.ts";

// A child task RE-ENTERED by a loop, its spawn and collect claimed by different workers (scoped by
// task_epoch). The checksum is a COUNT: N passes, N children. Chaos is pause/resume ONLY: a retry
// legitimately spawns another batch (tests/tick/task_epoch_test.ts).

const DSN = process.env.POSTGRES_DSN;

const WORKER_COUNT = 3;
const ROOT_COUNT = 6;
const PASSES = 4; // children per root, one per loop pass
const CHAOS_MS = 4_000;
const SETTLE_MS = 60_000;

const sleep = (ms: number) => new Promise((r) => setTimeout(r, ms));
// Not `paused`/`pausing`: a pause is not an outcome.
const isTerminal = (s?: string) => s === "completed" || s === "failed";

describe.runIf(!!DSN)("child task in a loop — worker fleet, postgres", () => {
  let workers: GenrocProcess[] = [];

  beforeAll(async () => {
    process.env.GENROC_PG_MAX_OPEN_CONNS = "8";
    // Sequential: the first process runs migrations before any other opens the DB.
    for (let i = 0; i < WORKER_COUNT; i++) {
      workers.push(await startGenroc({ pg: DSN, poll: 5, maxConcurrent: 5, immediateRetries: true }));
    }
  }, 60_000);

  afterAll(async () => {
    await Promise.all(workers.map((w) => w.stop()));
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

      // The gap lands a pause between a spawn and its collect. Errors are contention; ignored.
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

      // Pause/resume is non-destructive, so a failure here is the collect error epochs prevent.
      const failed = instances.filter((i) => i.status === "failed");
      expect(failed.map((i) => `${i.id}:${i.error_message}`), "no tree failed").toEqual([]);

      for (const id of rootIds) {
        const { data } = await api.GET("/instances/{id}/detail", { params: { path: { id } } });
        expect(data?.status).toBe("completed");
        expect((data?.output as { rounds?: number } | undefined)?.rounds).toBe(PASSES);
      }

      // The checksum: one child per pass and not one more.
      const children = instances.filter((i) => i.process === leaf);
      expect(children.length, "exactly one child per pass, per root").toBe(ROOT_COUNT * PASSES);
    },
    SETTLE_MS + CHAOS_MS + 60_000,
  );
});

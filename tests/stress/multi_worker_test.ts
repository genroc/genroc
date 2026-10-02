import { afterAll, beforeAll, describe, expect, test } from "vitest";
import { startGenroc, type GenrocProcess } from "../helpers/server.ts";
import { listAllInstances } from "../helpers/client.ts";

// A worker fleet (separate genroc processes, one Postgres) under pause/resume/retry chaos. Each root
// is a recursive tree of 2^(D+1)-1 instances whose `output.processes` re-counts it: exactly once.
// Not SQLite: processes sharing a file wedge; internal/db/dbtest/stress_test.go covers it in-process.

const DSN = process.env.POSTGRES_DSN;

const WORKER_COUNT = 3;
const ROOT_COUNT = 6;
const TTL = 3; // each root → 2^(TTL+1)-1 = 15 instances
const NODES_PER_ROOT = 2 ** (TTL + 1) - 1;
const CHAOS_MS = 4_000;
const SETTLE_MS = 60_000;

interface Backend {
  name: string;
  enabled: boolean;
  pollMs: number;
  db: string; // sqlite file path (shared by all workers); "" for postgres
  pgDSN?: string;
  env?: Record<string, string>;
}

const backends: Backend[] = [
  {
    name: "postgres",
    enabled: !!DSN,
    pollMs: 5,
    db: "",
    pgDSN: DSN,
    // Small pools, so WORKER_COUNT of them stay under Postgres' max_connections.
    env: { GENROC_PG_MAX_OPEN_CONNS: "8" },
  },
];

const sleep = (ms: number) => new Promise((r) => setTimeout(r, ms));
// Not `paused`/`pausing`: a pause is not an outcome.
const isTerminal = (s?: string) => s === "completed" || s === "failed";

for (const backend of backends) {
  describe.runIf(backend.enabled)(`multi-worker chaos — ${backend.name}`, () => {
    let workers: GenrocProcess[] = [];

    beforeAll(async () => {
      for (const [k, v] of Object.entries(backend.env ?? {})) process.env[k] = v;
      // Sequential: the first process runs migrations before any other opens the DB.
      for (let i = 0; i < WORKER_COUNT; i++) {
        workers.push(
          await startGenroc({
            db: backend.db,
            pg: backend.pgDSN,
            poll: backend.pollMs,
            maxConcurrent: 5,
            immediateRetries: true, // no backoff — maximise contention
          }),
        );
      }
    }, 60_000);

    afterAll(async () => {
      await Promise.all(workers.map((w) => w.stop()));
      workers = [];
    });

    test(
      "recursive trees survive random pause/resume/retry across separate processes",
      async () => {
        const api = workers[0].client;
        const processName = `stress_chaos_${crypto.randomUUID()}`;

        await api.PUT("/definitions", {
          body: {
            name: processName,
            input_schema: {
              type: "object",
              properties: { ttl: { type: "integer" } },
              required: ["ttl"],
            },
            tasks: [
              {
                id: "recursion_condition",
                switch: [
                  { case: "input.ttl > 0", goto: "$recursion" },
                  { goto: "end" },
                ],
              },
              {
                id: "recursion",
                action: {
                  type: "child_map" as const,
                  children: {
                    first: {
                      name: processName,
                      input: { ttl: "$: input.ttl - 1" },
                      result_schema: {
                        type: "object",
                        properties: { processes: { type: "number" } },
                        required: ["processes"],
                      },
                    },
                    second: {
                      name: processName,
                      input: { ttl: "$: input.ttl - 1" },
                      result_schema: {
                        type: "object",
                        properties: { processes: { type: "number" } },
                        required: ["processes"],
                      },
                    },
                  },
                },
                output: "$: self.result",
                switch: [{ goto: "end" }],
              },
            ],
            output: {
              processes:
                "$: (outputs.recursion.first.processes ?? 0) + (outputs.recursion.second.processes ?? 0) + 1",
            },
          },
        });

        const rootIds: string[] = [];
        for (let i = 0; i < ROOT_COUNT; i++) {
          const { data, error } = await api.POST("/instances", {
            body: { process: processName, input: { ttl: TTL } },
          });
          expect(error).toBeUndefined();
          rootIds.push(data!.id);
        }
        const randomRoot = () => rootIds[Math.floor(Math.random() * rootIds.length)];

        // Errors (pausing a completed root, retrying a non-failed one) are contention; ignored.
        let chaosOn = true;
        // Every pause is resumed a beat later; the gap races the workers mid-task. A resume lost
        // to a lost update is caught by the sweep after the window.
        const pauser = (async () => {
          while (chaosOn) {
            const id = randomRoot();
            await api
              .POST("/instances/{id}/pause", { params: { path: { id } } })
              .catch(() => {});
            await sleep(20 + Math.random() * 40);
            await api
              .POST("/instances/{id}/resume", { params: { path: { id } } })
              .catch(() => {});
            await sleep(30 + Math.random() * 70);
          }
        })();
        const retrier = (async () => {
          while (chaosOn) {
            await api
              .POST("/instances/{id}/retry", {
                params: { path: { id: randomRoot() }, query: { force: Math.random() < 0.5 } },
              })
              .catch(() => {});
            await sleep(30 + Math.random() * 70);
          }
        })();

        await sleep(CHAOS_MS);
        chaosOn = false;
        await Promise.all([pauser, retrier]);

        // Nothing advances a paused tree, so resume every root before waiting to settle.
        for (const id of rootIds) {
          await api
            .POST("/instances/{id}/resume", { params: { path: { id } } })
            .catch(() => {});
        }

        // Settle: resume paused roots and force-retry failed ones until every tree completes.
        const byProcess = (i: { process?: string }) => i.process === processName;
        const deadline = Date.now() + SETTLE_MS;
        let allDone = false;
        while (Date.now() < deadline) {
          const insts = (await listAllInstances(api)).filter(byProcess);
          const byId = new Map(insts.map((i) => [i.id, i]));

          let rootsCompleted = true;
          for (const id of rootIds) {
            const r = byId.get(id);
            if (r?.status === "completed") continue;
            rootsCompleted = false;
            // A `pausing` root is re-swept each iteration until the in-flight task's
            // write lands it in `paused` and the resume actually takes.
            if (r && (r.status === "paused" || r.status === "pausing")) {
              await api
                .POST("/instances/{id}/resume", { params: { path: { id } } })
                .catch(() => {});
            } else if (r?.status === "failed") {
              await api
                .POST("/instances/{id}/retry", {
                  params: { path: { id }, query: { force: true } },
                })
                .catch(() => {});
            }
          }
          if (rootsCompleted && insts.every((i) => isTerminal(i.status))) {
            allDone = true;
            break;
          }
          await sleep(150);
        }
        expect(allDone, "all roots completed and every instance terminal").toBe(true);

        // Exactly-once under contention: every tree aggregated to its exact size.
        const finalInsts = (await listAllInstances(api)).filter(byProcess);
        expect(finalInsts.every((i) => isTerminal(i.status))).toBe(true);

        for (const id of rootIds) {
          const { data } = await api.GET("/instances/{id}/detail", { params: { path: { id } } });
          expect(data?.status).toBe("completed");
          expect((data?.output as { processes?: number })?.processes).toBe(
            NODES_PER_ROOT,
          );
        }
      },
      120_000,
    );
  });
}

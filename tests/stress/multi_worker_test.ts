import { afterAll, beforeAll, describe, expect, test } from "vitest";
import { buildGenrocBinary, startGenroc, type GenrocProcess } from "../helpers/server.ts";
import { listAllInstances } from "../helpers/client.ts";

// Multi-worker collision stress. Several independent `genroc` processes — each its
// own OS process and its own connection pool/handle — poll the same database
// while a chaos loop randomly pauses, resumes and retries roots. Unlike the in-process
// Go engine stress tests (engines as goroutines sharing one *db.DB), this is the
// real shape of a worker fleet: correctness rests on the claim/lease/child-finish
// locks holding across separate processes.
//
// Workload: a recursive child_map process. With ttl=D every instance spawns
// two children with ttl=D-1 until ttl hits 0, so each root grows a binary tree of
// exactly 2^(D+1)-1 instances, and the root's aggregated `output.processes`
// re-counts that subtree bottom-up — a built-in exactly-once checksum.
//
// Note pause is not an outcome and not terminal: a paused tree just stops being
// advanced, keeping its wait_state/wake_at/retry_count/context, and only a resume
// starts it moving again. So the chaos loop always pairs a pause with a resume, and
// a final sweep resumes anything still paused — otherwise the settle wait below
// would block forever on a tree nobody is advancing.
//
// Collision signals asserted after the chaos settles:
//   1. every instance is terminal — no instance left stuck running/waiting/
//      collecting by a lost update or a pause racing a spawn;
//   2. every root reaches completed once the chaos stops (driven green by resumes
//      and forced retries) — no tree wedged by cross-worker contention;
//   3. each completed root's output.processes == subtree size — no worker
//      double-spawned a child or double-counted an output.
//
// Postgres only. A worker fleet is a Postgres-only deployment: separate processes
// rely on FOR UPDATE SKIP LOCKED claims and per-row FOR UPDATE child-finish locks.
// SQLite is single-writer/single-process — running several genroc processes against
// one file wedges under chaos (a pause-cascade transaction lost to
// SQLITE_BUSY_SNAPSHOT strands a pausing|waiting parent, which then never reaches
// paused and so never takes a resume). The SQLite *supported* multi-worker model is
// multiple engines in ONE process; the equivalent lifecycle-vs-lifecycle contention
// is covered in-process by the Go stress suite (internal/db/dbtest/stress_test.go).

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
  basePort: number;
  pollMs: number;
  db: string; // sqlite file path (shared by all workers); "" for postgres
  pgDSN?: string;
  env?: Record<string, string>;
}

// Each worker is an independent process, all opening the same DSN with a small
// pool each (so WORKER_COUNT pools stay well under Postgres' max_connections).
const backends: Backend[] = [
  {
    name: "postgres",
    enabled: !!DSN,
    basePort: 8920,
    pollMs: 5,
    db: "",
    pgDSN: DSN,
    // Small pool per worker (passed to genroc as --pg-max-open-conns) so
    // WORKER_COUNT pools stay well under Postgres' max_connections.
    env: { GENROC_PG_MAX_OPEN_CONNS: "8" },
  },
];

const sleep = (ms: number) => new Promise((r) => setTimeout(r, ms));
// `paused`/`pausing` are deliberately absent: a pause is not an outcome, only a
// tree that has stopped being advanced, so it never counts as settled.
const isTerminal = (s?: string) => s === "completed" || s === "failed";

let binPromise: Promise<string> | undefined;
const genrocBin = () => (binPromise ??= buildGenrocBinary());

for (const backend of backends) {
  describe.runIf(backend.enabled)(`multi-worker chaos — ${backend.name}`, () => {
    let workers: GenrocProcess[] = [];

    beforeAll(async () => {
      const bin = await genrocBin();
      for (const [k, v] of Object.entries(backend.env ?? {})) process.env[k] = v;
      // Spawn sequentially: the first process runs migrations before any other
      // opens the DB, avoiding a concurrent-migration race on the same file.
      for (let i = 0; i < WORKER_COUNT; i++) {
        workers.push(
          await startGenroc(
            bin,
            backend.basePort + i,
            backend.db,
            backend.pgDSN,
            backend.pollMs,
            5, // max-concurrent
            true, // immediate retries (no backoff) — maximise contention
          ),
        );
      }
    }, 60_000);

    afterAll(() => {
      for (const w of workers) w.stop();
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

        // Chaos window: hammer random roots with pauses, resumes and retries while
        // the workers race to advance, pause, and re-spawn the same trees. All
        // errors (pause of a completed root, resume of a running one, retry of a
        // non-failed one) are expected and ignored — they are part of the contention.
        let chaosOn = true;
        // Each pauser tick pauses a root and resumes it again a beat later: a paused
        // tree is not advanced by anyone, so leaving one behind would strand the
        // settle loop. The gap is what races the workers mid-task (running → pausing
        // → paused → running). Losing a resume to a lost update is still possible;
        // the sweep after the window is the backstop.
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

        // Resume sweep: nothing advances a paused tree, so before waiting for
        // settlement every root gets an unconditional resume (a no-op error on the
        // ones that are already running or terminal).
        for (const id of rootIds) {
          await api
            .POST("/instances/{id}/resume", { params: { path: { id } } })
            .catch(() => {});
        }

        // Settlement: service is calm now; resume any root the chaos left paused and
        // force-retry any failed one until every tree is completed and nothing is
        // left mid-flight.
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

        // Final state: no instance stuck, every root green, every surviving tree
        // aggregated to its exact size (exactly-once under contention).
        const finalInsts = (await listAllInstances(api)).filter(byProcess);
        expect(finalInsts.every((i) => isTerminal(i.status))).toBe(true);

        for (const id of rootIds) {
          const { data } = await api.GET("/instances/{id}/detail", { params: { path: { id } } });
          expect(data?.status).toBe("completed");
          expect((data?.state?.output as { processes?: number })?.processes).toBe(
            NODES_PER_ROOT,
          );
        }
      },
      120_000,
    );
  });
}

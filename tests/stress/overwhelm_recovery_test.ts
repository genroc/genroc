import { afterAll, beforeAll, describe, expect, test } from "vitest";
import {
  buildGenrocBinary,
  startGenroc,
  startSupervisedWorker,
  type GenrocProcess,
} from "../helpers/server.ts";
import { listAllInstances } from "../helpers/client.ts";

// Single-worker overwhelm + recovery, against real Postgres processes.
//
// A worker that can't keep its leases alive under load re-claims its own in-flight
// work, detects it (engine.OverwhelmError), and exits non-zero by design; a
// supervisor restarts it. We drive that on purpose with ONE processing worker at a
// time, which is the honest way to test it: with a single worker there is no peer to
// steal an expired lease, so nothing is ever advanced by two workers at once — the
// overwhelm-exit fires (and fires immediately, since the worker always re-claims its
// own lapsed lease), the in-flight work drains, and on restart the abandoned leases
// expire and are reclaimed. No double-processing, so a correct engine loses nothing.
//
//   Phase 1 (overwhelm): one supervised worker with a tiny lease and a starved pool
//   churns through the trees, overwhelming and being restarted by the supervisor.
//   Phase 2 (recovery): it is replaced by one normally-configured worker that drives
//   every tree to completion.
//
// An API-only node (--poll 0: serves HTTP, never advances) keeps the API reachable
// while the processing worker crash-loops, and is never a second processor.
//
// Asserted after recovery: the overwhelm actually happened (>=1 restart), every
// instance is terminal, every root completed, and each tree aggregated to its exact
// size — overwhelm churn never dropped or double-counted a subtree.
//
// Postgres only (a worker fleet is a Postgres deployment). It also needs the database
// to itself: any foreign worker polling the same DSN is a second processor, which both
// voids the single-processor premise above and drains the trees before the crippled
// worker can overwhelm on them. The stress project therefore runs in its own vitest
// invocation, with no other project's shared server alive — see vitest.config.ts.

const DSN = process.env.POSTGRES_DSN;

const ROOT_COUNT = 8;
const TTL = 4; // each root -> 2^(TTL+1)-1 = 31 instances; 16 leaves runnable at once
const NODES_PER_ROOT = 2 ** (TTL + 1) - 1;
const OVERWHELM_DEADLINE_MS = 25_000; // wait up to this long for the worker to overwhelm
const SETTLE_MS = 60_000;

const sleep = (ms: number) => new Promise((r) => setTimeout(r, ms));
// `paused`/`pausing` are deliberately absent: a pause is not an outcome, only a tree
// that has stopped being advanced, so it never counts as settled. Nothing here pauses
// anything, so a paused instance would be a bug, not a state to wait out.
const isTerminal = (s?: string) => s === "completed" || s === "failed";

let binPromise: Promise<string> | undefined;
const genrocBin = () => (binPromise ??= buildGenrocBinary());

describe.runIf(!!DSN)("single-worker overwhelm recovery — postgres", () => {
  let control: GenrocProcess; // --poll 0: serves the API, never advances
  let recovery: GenrocProcess | undefined;
  let bin = "";

  beforeAll(async () => {
    bin = await genrocBin();
    control = await startGenroc(bin, 8940, "", DSN, 0 /* poll=0 -> API only */, 1);
  }, 60_000);

  afterAll(() => {
    recovery?.stop();
    control?.stop();
  });

  test(
    "a single worker survives a forced overwhelm and finishes exactly once",
    async () => {
      const api = control.client;
      const processName = `overwhelm_${crypto.randomUUID()}`;

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

      // Phase 1: a single overwhelm-prone worker — tiny lease, huge concurrency,
      // starved pool — so its renewer falls behind, it re-claims its own in-flight
      // work, and the supervisor restarts it. The lease must stay shorter than how long
      // an advance is stuck waiting on the starved pool; since batched audit logging cut
      // each advance to ~1 DB round-trip (from ~6), the lease was tightened and the
      // concurrency raised to keep that margin negative and reliably overwhelm.
      const worker = await startSupervisedWorker(bin, 8941, {
        pgDSN: DSN!,
        pollMs: 1,
        maxConcurrent: 500,
        pgMaxOpenConns: 3,
        leaseDurationMs: 25,
        leaseRenewMs: 18,
        immediateRetries: true,
      });

      const rootIds: string[] = [];
      for (let i = 0; i < ROOT_COUNT; i++) {
        const { data, error } = await api.POST("/instances", {
          body: { process: processName, input: { ttl: TTL } },
        });
        expect(error).toBeUndefined();
        rootIds.push(data!.id);
      }

      // Wait until the worker has actually overwhelmed at least once (it re-claims its
      // own lapsed lease and the supervisor restarts it), rather than guessing a window.
      const overwhelmDeadline = Date.now() + OVERWHELM_DEADLINE_MS;
      while (worker.restarts() === 0 && Date.now() < overwhelmDeadline) {
        await sleep(200);
      }
      await worker.stop();
      const restarts = worker.restarts();
      console.log(`single worker overwhelmed and restarted ${restarts} time(s)`);
      expect(restarts, "the worker actually overwhelmed and was restarted").toBeGreaterThan(0);

      // Phase 2: one normal worker recovers everything (a different processor, but
      // still only ever one at a time — no peer can double-advance).
      recovery = await startGenroc(bin, 8942, "", DSN, 5 /* poll */, 20 /* max-concurrent */);

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
          // Retry only takes a `failed` root; the overwhelm churn never pauses
          // anything, so there is nothing to resume here.
          if (r?.status === "failed") {
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

      // Exactly-once: every tree aggregated to its exact size.
      for (const id of rootIds) {
        const { data } = await api.GET("/instances/{id}", { params: { path: { id } } });
        expect(data?.status).toBe("completed");
        expect((data?.context?.output as { processes?: number })?.processes).toBe(
          NODES_PER_ROOT,
        );
      }
    },
    120_000,
  );
});

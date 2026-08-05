import { afterAll, beforeAll, describe, expect, test } from "vitest";
import {
  buildGenrocBinary,
  startGenroc,
  startSupervisedWorker,
  type GenrocProcess,
} from "../helpers/server.ts";
import { listAllInstances } from "../helpers/client.ts";

// Single-worker lease pressure + crash recovery, against real Postgres processes.
// (Formerly overwhelm_recovery_test.ts, built around the fatal OverwhelmError exit;
// the lease fence retired that exit, and this asserts what replaced it.)
//
// A worker that cannot keep its leases alive under load no longer dies: the stale-lease
// gate repairs its own leases before claiming, and any advance whose row was re-granted
// mid-flight has its write refused (a lease_lost audit entry) instead of clobbering or
// killing the process — see specs/lease-fencing.md. Phase 1 drives exactly that state on
// purpose: ONE processing worker with a tiny lease, huge concurrency and a starved pool,
// churning under pressure that used to be fatal, and the assertion is that its
// supervisor NEVER has to restart it. Phase 1b then kills that worker twice (SIGKILL —
// an OOM kill, the one way a worker still dies), so the abandoned-lease → expiry →
// reclaim path runs end to end across real process boundaries.
//
// A single processing worker at a time is the honest way to run this: with no peer to
// steal an expired lease, nothing is ever advanced by two workers at once, so a correct
// engine loses nothing however hard it thrashes. An API-only node (--poll 0: serves
// HTTP, never advances) keeps the API reachable throughout, and is never a second
// processor.
//
//   Phase 1 (pressure): a supervised worker with a tiny lease and a starved pool churns
//   the trees; the fence and the gate keep it alive — zero supervisor restarts.
//   Phase 1b (crashes): SIGKILL it twice; the supervisor brings it back each time and
//   the restarted process reclaims the abandoned leases.
//   Phase 2 (recovery): it is replaced by one normally-configured worker that drives
//   every tree to completion.
//
// Asserted after recovery: the worker survived the pressure (0 unforced restarts, 2
// forced ones), every instance is terminal, every root completed, and each tree
// aggregated to its exact size — pressure churn and kills never dropped or
// double-counted a subtree.
//
// Postgres only (a worker fleet is a Postgres deployment). It also needs the database
// to itself: any foreign worker polling the same DSN is a second processor, which voids
// the single-processor premise above. The stress project therefore runs in its own
// vitest invocation, with no other project's shared server alive — see vitest.config.ts.

const DSN = process.env.POSTGRES_DSN;

const ROOT_COUNT = 8;
const TTL = 4; // each root -> 2^(TTL+1)-1 = 31 instances; 16 leaves runnable at once
const NODES_PER_ROOT = 2 ** (TTL + 1) - 1;
const PRESSURE_MS = 8_000; // how long the crippled worker churns before we check on it
const CRASHES = 2;
const SETTLE_MS = 60_000;

const sleep = (ms: number) => new Promise((r) => setTimeout(r, ms));
// `paused`/`pausing` are deliberately absent: a pause is not an outcome, only a tree
// that has stopped being advanced, so it never counts as settled. Nothing here pauses
// anything, so a paused instance would be a bug, not a state to wait out.
const isTerminal = (s?: string) => s === "completed" || s === "failed";

let binPromise: Promise<string> | undefined;
const genrocBin = () => (binPromise ??= buildGenrocBinary());

describe.runIf(!!DSN)("single-worker lease pressure — postgres", () => {
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
    "a single worker rides out lease pressure, survives two kills, and finishes exactly once",
    async () => {
      const api = control.client;
      const processName = `pressure_${crypto.randomUUID()}`;

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

      // Phase 1: a single pressure-prone worker — tiny lease, huge concurrency, starved
      // pool — so advances routinely outlive their leases. Before the fence this
      // configuration was built to die (the overwhelm exit); now the gate repairs what
      // it can and the fence refuses the rest, and the process must simply keep going.
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

      await sleep(PRESSURE_MS);
      expect(
        worker.restarts(),
        "lease pressure must not be fatal: the gate repairs and the fence refuses stale writes, the worker never exits",
      ).toBe(0);

      // Phase 1b: the one way a worker still dies — killed from outside. Each kill
      // abandons whatever leases were in flight; the supervisor's replacement reclaims
      // them once they expire.
      for (let i = 1; i <= CRASHES; i++) {
        worker.crash();
        const deadline = Date.now() + 10_000;
        while (worker.restarts() < i && Date.now() < deadline) {
          await sleep(100);
        }
        expect(worker.restarts(), `the supervisor restarted the worker after kill #${i}`).toBe(i);
        await sleep(1_500); // let the replacement claim and churn before the next kill
      }
      await worker.stop();
      console.log(`worker survived ${PRESSURE_MS}ms of lease pressure and ${CRASHES} forced kill(s)`);

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
          // Retry only takes a `failed` root; the pressure churn never pauses
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

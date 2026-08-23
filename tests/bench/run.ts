// Generalized spawn benchmark runner. A workload is a readable YAML file under
// workloads/ describing a genroc process (pure orchestration, no external HTTP calls)
// plus a small bench header; this runner applies it, drives it across the selected
// engines, and reports throughput. It isolates engine + DB throughput and compares
// SQLite (single writer) vs Postgres (concurrent workers).
//
//   node bench/run.ts recursive                             # SQLite only
//   POSTGRES_DSN=postgres://… node bench/run.ts recursive   # + Postgres compare
//   make bench-recursive | bench-deep | bench-drain            # via Makefile
//
// Every workload runs the SAME two-phase way: (1) load — a tick-only server (--poll 0)
// preloads `roots` root instances that nothing advances, so they pile up as a backlog;
// (2) drain — the server is restarted with the real poll loop and the time to work the
// whole backlog off (no instance left running) is measured. recursive/deep preload one
// root that fans out into a big tree; drain preloads thousands of independent roots.
// Only the drain phase is timed; the load phase is setup.
//
// Workloads (workloads/<name>.yaml):
//   recursive — one root, full binary tree (wide); concurrent throughput ceiling.
//   deep      — one root, narrow/tall tree; per-spawn depth cost.
//   drain     — many independent roots; steady-state queue-drain throughput.
//   drain_big — like drain, but each root carries a ~16 KiB input echoed to its output,
//               so both externalize into process_objects; isolates object-store overhead.
//
// Instances processed are counted from each root's SELF-REPORTED subtree size
// (output[count_field], summed) when the process defines one (recursive/deep); a
// process with no count_field is a single childless instance, so the count is just the
// number of roots (drain).
//
// Each YAML's `bench` section is the single source of truth for the run: input, roots,
// count_field, load_concurrency, poll_ms, runs, and per-engine concurrency (under
// `sqlite:`/`postgres:`). Concurrency is per engine — SQLite's single writer overwhelms
// at high concurrency while Postgres' concurrent workers thrive on it. The runner reads
// NO numeric env overrides, so there is exactly one place to look for a knob.
//
// Env is only for invocation, never workload numbers: BENCH_WORKLOAD (or argv[2])
// selects the workload, BENCH_ENGINES filters which engines run, BENCH_JSON sets the
// results output path, GENROC_SQLITE_SYNCHRONOUS sets SQLite durability (default FULL).

import { readFileSync, writeFileSync } from "node:fs";
import { load as loadYaml } from "js-yaml";
import { join } from "node:path";
import { arch, cpus, platform, release, totalmem } from "node:os";
import {
  buildGenrocBinary,
  startGenroc,
  tmpPath,
  type GenrocProcess,
} from "../helpers/server.ts";

// Read rather than imported: no bundler here parses .yaml, and a static import of one
// resolves at typecheck and then fails at run. Adding a workload = a new YAML + one line.
const readWorkload = (name: string): Workload =>
  loadYaml(readFileSync(new URL(`./workloads/${name}.yaml`, import.meta.url), "utf8")) as Workload;

const WORKLOADS: Record<string, Workload> = {
  recursive: readWorkload("recursive"),
  deep: readWorkload("deep"),
  drain: readWorkload("drain"),
  drain_big: readWorkload("drain_big"),
};

// Per-engine knobs (under bench.sqlite / bench.postgres).
interface EngineConfig {
  concurrency?: number; // max in-flight instances on this engine
}

// A workload file: a `bench` section (the whole run config) plus the genroc definition(s).
interface BenchConfig {
  input?: Record<string, unknown>; // per-instance (root) input
  roots?: number; // number of root instances to preload (default 1)
  count_field?: string; // output field holding each root's subtree size (omit ⇒ 1 per root)
  load_concurrency?: number; // parallel inserts during the load phase (default 64)
  poll_ms?: number; // server + client poll interval (default 10)
  runs?: number; // repeats per engine (default 1)
  timeout_ms?: number; // whole-backlog drain timeout (default 180000)
  concurrency?: number; // shared fallback when an engine omits its own (default 20)
  sqlite?: EngineConfig;
  postgres?: EngineConfig;
}

interface Workload {
  bench: BenchConfig;
  process?: unknown; // single genroc definition
  defs?: unknown[]; // or several (applied in order)
}

const DEFAULT_POLL_MS = 10;
const DEFAULT_RUNS = 1;
const DEFAULT_TIMEOUT_MS = 180_000;
const DEFAULT_CONCURRENCY = 20;
const DEFAULT_LOAD_CONCURRENCY = 64; // parallel inserts during the load phase
const BENCH_PORT = 8890; // distinct from the test servers (8888 sqlite, 8889 pg)
const BENCH_ENGINES = process.env.BENCH_ENGINES ?? "sqlite,postgres";

// Host fingerprint: printed and stamped onto every result so a task-change in the
// charts can be told apart from a runner/hardware change (e.g. GitHub swaps CPUs).
const HOST = (() => {
  const c = cpus();
  const model = c[0]?.model?.trim() ?? "unknown";
  return `${model} · ${c.length} cores · ${Math.round(totalmem() / 1024 ** 3)}GB · ${platform()} ${arch()} ${release()}`;
})();

const sleep = (ms: number) => new Promise((r) => setTimeout(r, ms));

type Client = GenrocProcess["client"];

// BENCH_WORKLOAD (or argv[2]) selects which workload YAML to run.
const NAME = process.argv[2] ?? process.env.BENCH_WORKLOAD ?? "recursive";
const workload = WORKLOADS[NAME];
if (!workload) {
  console.error(
    `unknown workload "${NAME}"; available: ${Object.keys(WORKLOADS).join(", ")}`,
  );
  process.exit(1);
}

const bench = workload.bench;
if (!bench) throw new Error(`workload "${NAME}" has no bench section`);

const INPUT: Record<string, unknown> = { ...(bench.input ?? {}) };
const ROOTS = bench.roots ?? 1;
const LOAD_CONCURRENCY = bench.load_concurrency ?? DEFAULT_LOAD_CONCURRENCY;
// Optional: each root self-reports its subtree size here (recursive/deep). Without it,
// every root is a single childless instance and the count is just the root count.
const COUNT_FIELD = bench.count_field;
const POLL_MS = bench.poll_ms ?? DEFAULT_POLL_MS;
const RUNS = bench.runs ?? DEFAULT_RUNS;
const TIMEOUT_MS = bench.timeout_ms ?? DEFAULT_TIMEOUT_MS;
const DEFS = workload.defs ?? (workload.process ? [workload.process] : []);
if (DEFS.length === 0) throw new Error(`workload "${NAME}" defines no process`);

// The process name to start: the first applied definition is the root.
const DEFS_NAME = (DEFS[0] as { name?: string }).name;
if (!DEFS_NAME) throw new Error(`workload "${NAME}" root definition has no name`);

// Per-engine concurrency: the engine's own `concurrency`, else the workload's shared
// fallback, else the built-in default.
function concurrencyFor(engine: string): number {
  const perEngine = engine === "sqlite" ? bench.sqlite : bench.postgres;
  return perEngine?.concurrency ?? bench.concurrency ?? DEFAULT_CONCURRENCY;
}

// countInstances returns how many process instances the run actually worked off. With
// a count_field, each root self-reports its subtree size (recursive/deep), so the total
// is those summed; without one, every root is a single childless instance (drain), so
// the total is just the number of roots. waitDrained has already returned, so every
// root is terminal — a non-completed root here is a genuine failure.
async function countInstances(client: Client, rootIds: string[]): Promise<number> {
  if (!COUNT_FIELD) return rootIds.length;
  let total = 0;
  for (const id of rootIds) {
    const { data, error } = await client.GET("/instances/{id}", {
      params: { path: { id } },
    });
    if (error) throw new Error(`get_instance failed: ${JSON.stringify(error)}`);
    if (data!.status !== "completed") {
      throw new Error(`root ${id} ended ${data!.status}: ${data!.error ?? ""}`);
    }
    const out = data!.context?.output as Record<string, number> | undefined;
    const n = out?.[COUNT_FIELD];
    if (typeof n !== "number") {
      throw new Error(
        `root ${id}: expected numeric output.${COUNT_FIELD}, got ${JSON.stringify(n)}`,
      );
    }
    total += n;
  }
  return total;
}

interface EngineResult {
  engine: string;
  durations: number[];
  instances: number;
  concurrency: number;
}

// preload inserts `roots` root instances as fast as the API allows (a fixed pool of
// `conc` concurrent POSTs) and returns their ids. The target server is tick-only, so
// the instances pile up as a backlog instead of being advanced.
async function preload(
  client: Client,
  roots: number,
  conc: number,
): Promise<string[]> {
  const ids = new Array<string>(roots);
  let next = 0;
  async function worker() {
    for (;;) {
      const i = next++;
      if (i >= roots) return;
      const { data, error } = await client.POST("/instances", {
        body: { process: DEFS_NAME, input: INPUT } as never,
      });
      if (error) throw new Error(`preload insert failed: ${JSON.stringify(error)}`);
      ids[i] = data!.id;
    }
  }
  await Promise.all(Array.from({ length: Math.min(conc, roots) }, () => worker()));
  return ids;
}

// anyWithStatus reports whether at least one instance currently has the given status.
async function anyWithStatus(
  client: Client,
  status: "running" | "failed",
): Promise<boolean> {
  const { data, error } = await client.GET("/instances", {
    params: { query: { status, limit: 1 } },
  });
  if (error) throw new Error(`list instances failed: ${JSON.stringify(error)}`);
  return (data?.items ?? []).length > 0;
}

// waitDrained blocks until no instance is left running, then asserts none failed. A
// parent parked on its children keeps status='running' (only its wait_state changes),
// so an empty status=running page means every tree has fully collapsed and every
// childless root is done — for both the trees and the drain backlog. The failed-check
// stops a broken workload from masquerading as a fast drain (a failed instance is also
// "not running").
async function waitDrained(client: Client) {
  const deadline = Date.now() + TIMEOUT_MS;
  while (Date.now() < deadline) {
    if (!(await anyWithStatus(client, "running"))) {
      if (await anyWithStatus(client, "failed")) {
        throw new Error("backlog drained but some instances failed");
      }
      return;
    }
    await sleep(POLL_MS);
  }
  throw new Error(`backlog did not drain within ${TIMEOUT_MS}ms`);
}

// benchEngine runs the workload against one engine in two phases per repeat: (1) load —
// a tick-only server (--poll 0) preloads `roots` root instances that nothing advances,
// so they pile up as a backlog; (2) drain — the server is restarted with the real poll
// loop and the time to work the backlog off is measured. Both phases share the same
// database (SQLite file / Postgres DSN), so the backlog survives the restart and the
// definitions persist. Only the drain phase is timed.
async function benchEngine(
  engine: string,
  dbPath: string,
  dsn: string | undefined,
): Promise<EngineResult> {
  const concurrency = concurrencyFor(engine);
  const bin = await buildGenrocBinary();
  const durations: number[] = [];
  let instances = 0;
  for (let run = 0; run < RUNS; run++) {
    // Phase 1 — load. poll=0 ⇒ manual-tick mode: the engine never auto-advances.
    const loader = await startGenroc(bin, BENCH_PORT, dbPath, dsn, 0, concurrency);
    let rootIds: string[];
    try {
      for (const def of DEFS) {
        const { error } = await loader.client.PUT("/definitions", {
          body: def as never,
        });
        if (error) throw new Error(`register failed: ${JSON.stringify(error)}`);
      }
      rootIds = await preload(loader.client, ROOTS, LOAD_CONCURRENCY);
    } finally {
      // Await the exit (not a fixed sleep): the drainer rebinds the SAME port on the
      // next line, so the loader must have fully released it. Its exit also flushes
      // the SQLite checkpoint (database.Close runs on shutdown) before we reopen.
      await loader.stop();
    }

    // Phase 2 — drain. Restart with the normal poll loop and time the work-off.
    const drainer = await startGenroc(bin, BENCH_PORT, dbPath, dsn, POLL_MS, concurrency);
    try {
      const start = Date.now();
      await waitDrained(drainer.client);
      durations.push(Date.now() - start);
      instances = await countInstances(drainer.client, rootIds);
    } finally {
      // Await the exit so the next run's loader (or the next engine's load phase)
      // can rebind the port cleanly.
      await drainer.stop();
    }
  }
  return { engine, durations, instances, concurrency };
}

function fmt(n: number, width: number) {
  return String(n).padStart(width);
}

// describeInput summarizes the input for the config line: a long string value (e.g.
// drain_big's ~16 KiB blob) is shown as "<N bytes>" so the report stays one readable line
// instead of dumping the whole payload to the terminal.
function describeInput(input: Record<string, unknown>): string {
  const shown = Object.fromEntries(
    Object.entries(input).map(([k, v]) =>
      typeof v === "string" && v.length > 64 ? [k, `<${v.length} bytes>`] : [k, v],
    ),
  );
  return JSON.stringify(shown);
}

function report(results: EngineResult[]) {
  const total = results[0]?.instances ?? 0;
  const throughput = (ms: number) => Math.round((total / ms) * 1000);

  console.log(
    "\nconfig: " +
      `workload=${NAME} input=${describeInput(INPUT)} roots=${ROOTS} ` +
      `load_concurrency=${LOAD_CONCURRENCY} instances=${total} poll_ms=${POLL_MS} runs=${RUNS}`,
  );
  // Both engines run fully durable by default (matched comparison): SQLite fsyncs the
  // WAL on every commit, Postgres commits synchronously. GENROC_SQLITE_SYNCHRONOUS
  // overrides SQLite's level (e.g. NORMAL for the faster, process-crash-only setting).
  const sqliteSync = process.env.GENROC_SQLITE_SYNCHRONOUS ?? "FULL";
  console.log(
    `durability: sqlite synchronous=${sqliteSync}, postgres synchronous_commit=on`,
  );
  console.log(`host:   ${HOST}\n`);

  console.log(
    "engine".padEnd(10) +
      "runs".padStart(6) +
      "instances".padStart(11) +
      "conc".padStart(6) +
      "min_ms".padStart(9) +
      "avg_ms".padStart(9) +
      "max_ms".padStart(9) +
      "thrpt(inst/s)".padStart(15),
  );

  const avgThr: Record<string, number> = {};
  for (const r of results) {
    const min = Math.min(...r.durations);
    const max = Math.max(...r.durations);
    const avg = Math.round(
      r.durations.reduce((a, b) => a + b, 0) / r.durations.length,
    );
    avgThr[r.engine] = throughput(avg);
    console.log(
      r.engine.padEnd(10) +
        fmt(r.durations.length, 6) +
        fmt(r.instances, 11) +
        fmt(r.concurrency, 6) +
        fmt(min, 9) +
        fmt(avg, 9) +
        fmt(max, 9) +
        fmt(throughput(avg), 15),
    );
  }

  if (avgThr.sqlite && avgThr.postgres) {
    const ratio = (avgThr.postgres / avgThr.sqlite).toFixed(2);
    console.log(`\npostgres/sqlite throughput ratio: ${ratio}x`);
  }
}

// When BENCH_JSON is set, write the results as a github-action-benchmark
// customBiggerIsBetter array (so CI can chart throughput per commit over time).
function writeBenchJSON(path: string, results: EngineResult[]) {
  const entries = results.map((r) => {
    const avg = Math.round(
      r.durations.reduce((a, b) => a + b, 0) / r.durations.length,
    );
    return {
      name: `spawn ${NAME} ${r.engine}`,
      unit: "inst/s",
      value: Math.round((r.instances / avg) * 1000),
      extra: HOST, // shown in github-action-benchmark chart tooltips
    };
  });
  writeFileSync(path, JSON.stringify(entries, null, 2));
}

async function main() {
  const results: EngineResult[] = [];

  // BENCH_ENGINES selects which engines to run (comma-separated), e.g.
  // BENCH_ENGINES=postgres to skip the slow SQLite pass when tuning Postgres.
  const engines = BENCH_ENGINES.split(",")
    .map((s) => s.trim().toLowerCase())
    .filter(Boolean);

  if (engines.includes("sqlite")) {
    const sqliteDb = tmpPath("genroc_bench", ".db");
    results.push(await benchEngine("sqlite", sqliteDb, undefined));
  }

  if (engines.includes("postgres")) {
    const dsn = process.env.POSTGRES_DSN;
    if (dsn) {
      results.push(await benchEngine("postgres", "", dsn));
    } else {
      console.log(
        "\n(POSTGRES_DSN not set — skipping postgres; set it to compare)",
      );
    }
  }

  report(results);
  const jsonPath = process.env.BENCH_JSON;
  if (jsonPath) writeBenchJSON(jsonPath, results);
}

main().catch((err) => {
  console.error(err);
  process.exit(1);
});

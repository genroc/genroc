// `node bench/run.ts <workload>`: times draining a workloads/<name>.yaml backlog on SQLite, and on
// Postgres when POSTGRES_DSN is set. Every numeric knob lives in the YAML's `bench` section; env only
// selects (BENCH_WORKLOAD, BENCH_ENGINES, BENCH_JSON, GENROC_SQLITE_SYNCHRONOUS).

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
  iterate: readWorkload("iterate"),
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

function concurrencyFor(engine: string): number {
  const perEngine = engine === "sqlite" ? bench.sqlite : bench.postgres;
  return perEngine?.concurrency ?? bench.concurrency ?? DEFAULT_CONCURRENCY;
}

// Runs after waitDrained, so every root is terminal and a non-completed one is a genuine failure.
async function countInstances(client: Client, rootIds: string[]): Promise<number> {
  if (!COUNT_FIELD) return rootIds.length;
  let total = 0;
  for (const id of rootIds) {
    const { data, error } = await client.GET("/instances/{id}/detail", {
      params: { path: { id } },
    });
    if (error) throw new Error(`get_instance failed: ${JSON.stringify(error)}`);
    if (data!.status !== "completed") {
      throw new Error(`root ${id} ended ${data!.status}: ${data!.error_message ?? ""}`);
    }
    const out = data!.output as Record<string, number> | undefined;
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

// Onto a tick-only server, so the instances pile up as a backlog instead of being advanced.
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

// Both bounds matter on a reused Postgres DSN: the name excludes dbtest fixtures (future-dated by
// AdvanceClock), and `since` excludes an earlier failed run of this workload.
async function anyWithStatus(
  client: Client,
  status: "running" | "failed",
  since: number,
): Promise<boolean> {
  const { data, error } = await client.GET("/instances", {
    params: { query: { status, process: DEFS_NAME, created_after: since, limit: 1 } },
  });
  if (error) throw new Error(`list instances failed: ${JSON.stringify(error)}`);
  return (data?.items ?? []).length > 0;
}

// A parent parked on its children stays `running`, so an empty running page means all collapsed.
// The failed check stops a broken workload passing as a fast drain.
async function waitDrained(client: Client, since: number) {
  const deadline = Date.now() + TIMEOUT_MS;
  while (Date.now() < deadline) {
    if (!(await anyWithStatus(client, "running", since))) {
      if (await anyWithStatus(client, "failed", since)) {
        throw new Error("backlog drained but some instances failed");
      }
      return;
    }
    await sleep(POLL_MS);
  }
  throw new Error(`backlog did not drain within ${TIMEOUT_MS}ms`);
}

// Load on a tick-only server, then restart with the poll loop and time only the drain. Both phases
// share the database, so the backlog survives the restart.
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
    // Taken before the first insert, so the drain verdict covers exactly this run's
    // instances and no earlier one's (see anyWithStatus).
    const since = Date.now();
    // Phase 1 — load. poll=0 ⇒ manual-tick mode: the engine never auto-advances.
    const loader = await startGenroc({ bin, port: BENCH_PORT, db: dbPath, pg: dsn, poll: 0, maxConcurrent: concurrency });
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
      // Await the exit: the drainer rebinds the SAME port next, and exit flushes the SQLite checkpoint.
      await loader.stop();
    }

    // Phase 2 — drain. Restart with the normal poll loop and time the work-off.
    const drainer = await startGenroc({ bin, port: BENCH_PORT, db: dbPath, pg: dsn, poll: POLL_MS, maxConcurrent: concurrency });
    try {
      const start = Date.now();
      await waitDrained(drainer.client, since);
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

// A long string value (drain_big's ~16 KiB blob) prints as "<N bytes>", keeping one readable line.
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
  // Both engines fully durable by default. fullfsync is printed because on macOS it separates a
  // real fsync from a 185x-faster no-op (specs/durability-levels.md §1).
  const sqliteSync = process.env.GENROC_SQLITE_SYNCHRONOUS ?? "FULL";
  const fullFsync = process.env.GENROC_SQLITE_FULLFSYNC ? "on" : "off";
  const commitDelay = process.env.GENROC_PG_COMMIT_DELAY ?? "0";
  const level = process.env.GENROC_DURABILITY ?? "strict";
  console.log(
    `durability: level=${level}, sqlite synchronous=${sqliteSync} fullfsync=${fullFsync}, ` +
      `postgres synchronous_commit=on commit_delay=${commitDelay}us`,
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

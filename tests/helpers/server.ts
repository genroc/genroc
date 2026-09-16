import { spawnSync, spawn, type ChildProcess } from "child_process";
import { randomUUID } from "crypto";
import { createServer, type AddressInfo } from "net";
import { join } from "path";
import { tmpdir } from "os";
import { inject } from "vitest";
import type { TestProject } from "vitest/node";
import { BASE_URL, PORT } from "./constants.ts";
import { createClientTyped } from "./client.ts";

declare module "vitest" {
  export interface ProvidedContext {
    // The genroc binary this run built, handed from globalSetup to every worker so a file
    // that needs a private server does not build one of its own.
    genrocBin: string;
  }
}

const ROOT = new URL("../../", import.meta.url).pathname;

// A temp path no other test process can pick. Date.now() alone collides whenever two
// Vitest workers start within the same millisecond, and two servers sharing one SQLite
// file wreck each other: a co-tenant's ticks claim these instances, and its log pruning
// (retention against *its* clock offset) deletes this server's whole audit trail.
export function tmpPath(prefix: string, suffix = ""): string {
  return join(tmpdir(), `${prefix}_${Date.now()}_${process.pid}_${randomUUID().slice(0, 8)}${suffix}`);
}

export async function buildGenrocBinary(): Promise<string> {
  const bin = tmpPath("genroc");
  const result = spawnSync("go", ["build", "-o", bin, "./cmd/genroc"], {
    cwd: ROOT,
    env: { ...process.env, CGO_ENABLED: "1" },
    stdio: ["ignore", "ignore", "inherit"],
  });
  if (result.status !== 0) throw new Error("Failed to build genroc binary");
  return bin;
}

// The binary a private server runs: the one globalSetup built and provided, or — in a project
// with no globalSetup (stress) — one built once per worker. Nothing in a test file builds.
let workerBin: Promise<string> | undefined;
export function getBin(): Promise<string> {
  let provided: string | undefined;
  try {
    provided = inject("genrocBin");
  } catch {
    // Outside a worker (globalSetup itself) there is nothing to inject.
  }
  if (provided) return Promise.resolve(provided);
  return (workerBin ??= buildGenrocBinary());
}

// A port the OS just handed out and nothing holds. Vitest runs files in parallel, and every
// hand-picked number was a collision waiting for the right pair of files to overlap — the
// readiness probe then answers from the NEIGHBOUR's server and the failure is an assertion
// about behaviour, never about a port. The window between close and the child's bind is real
// but ephemeral ports are not reissued that fast; assertPortFree still guards it.
export async function freePort(): Promise<number> {
  const probe = createServer();
  return new Promise<number>((resolve, reject) => {
    probe.once("error", reject);
    probe.listen(0, () => {
      const { port } = probe.address() as AddressInfo;
      probe.close(() => resolve(port));
    });
  });
}

// Everything a private server can be started with. Every field is optional: the default is a
// fresh SQLite file on a free port with the engine's own poll interval, which is what a test
// that merely needs its own server wants.
export interface StartOptions {
  port?: number; // omit for a free one; pass a server's own `port` back to respawn it in place
  db?: string; // SQLite file; defaults to a fresh temp path
  pg?: string; // Postgres DSN instead of SQLite
  poll?: number; // ms; 0 is manual-tick mode
  maxConcurrent?: number;
  immediateRetries?: boolean;
  log?: string; // --log level; the default is error, and stdout is discarded either way
  // Extra environment for this process only — config baked at start (GENROC_GLOBAL_*), a
  // secret to redact. A test whose server needs one of these owns its server.
  env?: Record<string, string>;
  // Receives the process's console stream (stderr) as it arrives. Only a test about what
  // reaches the operator's console needs it; everything else keeps stdio ignored.
  onStderr?: (chunk: string) => void;
  bin?: string; // defaults to the run's shared binary
}

function spawnProc(bin: string, port: number, o: StartOptions): ChildProcess {
  const dbArgs = o.pg ? ["--pg", o.pg] : ["--db", o.db!];
  const pollArgs = o.poll !== undefined ? ["--poll", String(o.poll)] : [];
  const concArgs = o.maxConcurrent !== undefined ? ["--max-concurrent", String(o.maxConcurrent)] : [];
  const retryArgs = o.immediateRetries ? ["--immediate-retries"] : [];
  // Auth mode via env, so the auth suite can start an authenticated server without adding a
  // parameter to a chain every other caller passes positionally.
  const authArgs = [
    ...(process.env.GENROC_TEST_AUTH ? ["--auth", process.env.GENROC_TEST_AUTH] : []),
    ...(process.env.GENROC_TEST_BOOTSTRAP_TOKEN
      ? ["--bootstrap-token", process.env.GENROC_TEST_BOOTSTRAP_TOKEN]
      : []),
  ];
  // Optional lease overrides via env (used by the benchmark to tune the lease).
  const leaseArgs = [
    ...(process.env.GENROC_LEASE_DURATION ? ["--lease-duration", process.env.GENROC_LEASE_DURATION] : []),
    ...(process.env.GENROC_LEASE_RENEW_INTERVAL ? ["--lease-renew-interval", process.env.GENROC_LEASE_RENEW_INTERVAL] : []),
  ];
  // Optional pool sizing via env (used by the stress test to keep a fleet within max_connections).
  const poolArgs = [
    ...(process.env.GENROC_PG_MAX_OPEN_CONNS
      ? ["--pg-max-open-conns", process.env.GENROC_PG_MAX_OPEN_CONNS]
      : []),
    // Group-commit width, the Postgres counterpart of the SQLite durability knobs below:
    // it trades latency for batch width and spends no durability. Ignored for SQLite.
    ...(process.env.GENROC_PG_COMMIT_DELAY
      ? ["--pg-commit-delay", process.env.GENROC_PG_COMMIT_DELAY]
      : []),
  ];
  // Optional SQLite durability via env (used by the benchmark to compare engines at
  // matched durability, e.g. GENROC_SQLITE_SYNCHRONOUS=FULL). Ignored for Postgres.
  const syncArgs = [
    ...(process.env.GENROC_SQLITE_SYNCHRONOUS
      ? ["--sqlite-synchronous", process.env.GENROC_SQLITE_SYNCHRONOUS]
      : []),
    // On macOS a plain fsync(2) does not flush the drive cache, so an unset flag here
    // makes synchronous=FULL benchmark ~20x faster than it would on real storage.
    ...(process.env.GENROC_SQLITE_FULLFSYNC ? ["--sqlite-fullfsync"] : []),
    // The durability ladder (specs/durability-levels.md): unset means strict, which is
    // what every test other than the benchmark wants.
    ...(process.env.GENROC_DURABILITY ? ["--durability", process.env.GENROC_DURABILITY] : []),
  ];
  const proc = spawn(bin, [...dbArgs, "--http", `:${port}`, "--log", o.log ?? "error", ...pollArgs, ...concArgs, ...retryArgs, ...authArgs, ...leaseArgs, ...poolArgs, ...syncArgs], {
    stdio: ["ignore", "ignore", o.onStderr ? "pipe" : "ignore"],
    // Fixed config fixtures for the config e2e test. The test's process names are
    // random, so we use the global tier (GENROC_GLOBAL_<NAME> → config.<NAME>). A fixture
    // that names a PORT does not belong here: a server-wide value cannot know a port the
    // test has not bound yet, so such a test starts its own server with `env`.
    env: {
      ...process.env,
      GENROC_GLOBAL_E2E_URL: "https://config.example.test",
      GENROC_GLOBAL_E2E_PORT: "8080",
      GENROC_GLOBAL_E2E_TOKEN: "supersecret-token-value",
      // A secret config value for the API-redaction test.
      GENROC_GLOBAL_API_KEY: "supersecret-api-key",
      // Retry policy driven from the environment (retry_policy_test). The schema default
      // is 0, so a run that retries at all proves the env value reached the curve.
      GENROC_GLOBAL_E2E_RETRY_ATTEMPTS: "2",
      ...o.env,
    },
  });
  if (o.onStderr) proc.stderr!.on("data", (c: Buffer) => o.onStderr!(c.toString()));
  return proc;
}

// Refuse to start on a port something else already holds. The exit check below is not
// enough on its own: a readiness probe answered by the *other* server can win the race
// against noticing our own process died binding, and the caller then drives someone else's
// engine — which surfaces as an assertion about behaviour, never as a port error. Ports are
// OS-assigned now, so reaching this means either the freePort→bind window was lost (rerun)
// or a caller passed an explicit port that something else — a stale server — still holds.
async function assertPortFree(port: number): Promise<void> {
  const probe = createServer();
  await new Promise<void>((resolve, reject) => {
    probe.once("error", (err: NodeJS.ErrnoException) =>
      reject(
        err.code === "EADDRINUSE"
          ? new Error(
              `port ${port} is already in use — a stale genroc from an earlier run, or a ` +
                `free port something else grabbed first; kill the former, rerun for the latter`,
            )
          : err,
      ),
    );
    probe.listen(port, () => probe.close(() => resolve()));
  });
}

async function waitUntilReady(
  port: number,
  proc: ChildProcess,
  timeoutMs = 10_000,
): Promise<void> {
  const deadline = Date.now() + timeoutMs;
  while (Date.now() < deadline) {
    // Fail fast (and clearly) if the process died during startup — e.g. it could
    // not bind the port — instead of polling a dead process until the timeout and
    // then being fooled by some *other* server answering on the same port.
    if (proc.exitCode !== null || proc.signalCode !== null) {
      throw new Error(
        `genroc on port ${port} exited before becoming ready (code=${proc.exitCode}, signal=${proc.signalCode})`,
      );
    }
    try {
      // /healthz, not /openapi.json: it is the readiness endpoint, it is root-mounted
      // (actionDef.Root), and so it does not move when the API namespace does.
      const r = await fetch(`http://localhost:${port}/healthz`);
      await r.body?.cancel();
      if (r.ok) return;
    } catch {}
    await new Promise((r) => setTimeout(r, 100));
  }
  throw new Error(`genroc on port ${port} did not become ready within ${timeoutMs}ms`);
}

// stopProc sends SIGTERM and resolves once the process has actually exited (so the
// OS has released its listening port). Callers that reuse the port on the next line
// MUST await this — a fixed sleep races the graceful shutdown on a slow host.
function stopProc(proc: ChildProcess): Promise<void> {
  return new Promise<void>((resolve) => {
    if (proc.exitCode !== null || proc.signalCode !== null) return resolve();
    proc.once("exit", () => resolve());
    proc.kill("SIGTERM");
  });
}

export interface GenrocProcess {
  client: ReturnType<typeof createClientTyped>;
  port: number;
  baseUrl: string; // http://localhost:<port>, the root — the API lives under /api
  stop: () => Promise<void>; // SIGTERM — resolves once the process has fully exited
  crash: () => void; // SIGKILL — simulate a hard crash, lease stays in DB
  // The SQLite file this server was started on, so a test can assert on columns the API
  // does not expose (task_epoch, parent_task_epoch). Empty when running against Postgres.
  dbPath: string;
}

// Starts a private genroc for one test file — on a free port, on its own SQLite file, from the
// binary this run already built — and returns once /healthz answers. A test reaches for this
// only when the shared server cannot serve it: manual ticks, a crash to survive, config baked
// at start. Everything else uses `client` against the shared one.
export async function startGenroc(o: StartOptions = {}): Promise<GenrocProcess> {
  const port = o.port ?? (await freePort());
  const db = o.pg ? "" : o.db ?? tmpPath("genroc", ".db");
  await assertPortFree(port);
  const proc = spawnProc(o.bin ?? (await getBin()), port, { ...o, db });
  await waitUntilReady(port, proc);
  return {
    client: createClientTyped({ baseUrl: `http://localhost:${port}/api` }),
    port,
    baseUrl: `http://localhost:${port}`,
    stop: () => stopProc(proc),
    crash: () => proc.kill("SIGKILL"),
    dbPath: db,
  };
}

// ── Supervised worker (auto-restart on the overwhelm exit) ────────────────────

export interface WorkerOpts {
  pgDSN: string;
  pollMs: number;
  maxConcurrent: number;
  leaseDurationMs?: number;
  leaseRenewMs?: number;
  immediateRetries?: boolean;
  pgMaxOpenConns?: number;
}

export interface SupervisedWorker {
  restarts: () => number; // times the process exited and was brought back
  crash: () => void; // SIGKILL — the supervisor notices the exit and respawns
  stop: () => Promise<void>;
}

function workerArgs(port: number, o: WorkerOpts): string[] {
  return [
    "--pg", o.pgDSN,
    "--http", `:${port}`,
    "--log", "error",
    "--poll", String(o.pollMs),
    "--max-concurrent", String(o.maxConcurrent),
    ...(o.leaseDurationMs !== undefined ? ["--lease-duration", `${o.leaseDurationMs}ms`] : []),
    ...(o.leaseRenewMs !== undefined ? ["--lease-renew-interval", `${o.leaseRenewMs}ms`] : []),
    ...(o.immediateRetries ? ["--immediate-retries"] : []),
    ...(o.pgMaxOpenConns !== undefined
      ? ["--pg-max-open-conns", String(o.pgMaxOpenConns)]
      : process.env.GENROC_PG_MAX_OPEN_CONNS
        ? ["--pg-max-open-conns", process.env.GENROC_PG_MAX_OPEN_CONNS]
        : []),
    ...(process.env.GENROC_PG_COMMIT_DELAY
      ? ["--pg-commit-delay", process.env.GENROC_PG_COMMIT_DELAY]
      : []),
  ];
}

// startSupervisedWorker runs one genroc worker process and restarts it whenever it
// exits — exactly what a process supervisor (systemd, k8s) does for a worker fleet.
// Nothing inside the engine exits on its own anymore (lease pressure is repaired by
// the gate or refused per-write by the fence — see specs/lease-fencing.md), so
// restarts() counts only real deaths: a crash() here, an OOM kill in production. The
// supervisor brings the worker back with a fresh pid, its abandoned leases expire,
// and the restarted process reclaims them.
export async function startSupervisedWorker(o: WorkerOpts): Promise<SupervisedWorker> {
  const bin = await getBin();
  const port = await freePort();
  let stopped = false;
  let restarts = 0;
  let proc: ChildProcess = spawn(bin, workerArgs(port, o), { stdio: "ignore" });
  const onExit = () => {
    if (stopped) return;
    restarts++;
    // Brief pause so the OS frees the port before the supervisor relaunches.
    setTimeout(() => {
      if (stopped) return;
      proc = spawn(bin, workerArgs(port, o), { stdio: "ignore" });
      proc.on("exit", onExit);
    }, 100);
  };
  proc.on("exit", onExit);
  await waitUntilReady(port, proc);
  return {
    restarts: () => restarts,
    crash: () => proc.kill("SIGKILL"),
    stop: () =>
      new Promise<void>((resolve) => {
        stopped = true;
        if (proc.exitCode !== null || proc.signalCode !== null) return resolve();
        proc.once("exit", () => resolve());
        proc.kill("SIGTERM");
      }),
  };
}

// ── Global shared server for vitest's globalSetup ─────────────────────────────

let sharedServer: GenrocProcess | null = null;

async function ping(): Promise<boolean> {
  try {
    const r = await fetch(`${BASE_URL}/healthz`);
    await r.body?.cancel();
    return r.ok;
  } catch {
    return false;
  }
}

// The shared server keeps its ONE configured port (GENROC_PORT, per project): the module-level
// `client` every ordinary test file imports is built from it at import time. Private servers
// take free ports instead — see startGenroc.
export async function setup(project: TestProject) {
  if (await ping()) {
    // Something already answers there. Reusing it is the way a run silently tests stale code
    // — a server left over from an earlier run skips the rebuild below — so it is opt-in.
    if (process.env.GENROC_REUSE_SERVER) return;
    throw new Error(
      `a genroc is already serving on ${BASE_URL} — a stale one from an earlier run would make ` +
        `this run test old code. Kill it, or set GENROC_REUSE_SERVER=1 to test against it on purpose.`,
    );
  }
  console.log("\nBuilding test server…");
  const bin = await buildGenrocBinary();
  project.provide("genrocBin", bin);
  sharedServer = await startGenroc({ bin, port: PORT as number });
}

export async function teardown() {
  await sharedServer?.stop();
}

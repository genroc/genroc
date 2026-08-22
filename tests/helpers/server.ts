import { spawnSync, spawn, type ChildProcess } from "child_process";
import { randomUUID } from "crypto";
import { createServer } from "net";
import { join } from "path";
import { tmpdir } from "os";
import { BASE_URL, PORT } from "./constants.ts";
import { createClientTyped } from "./client.ts";

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

function spawnProc(
  bin: string,
  port: number,
  db: string,
  pgDSN?: string,
  pollMs?: number,
  maxConcurrent?: number,
  immediateRetries?: boolean,
): ChildProcess {
  const dbArgs = pgDSN ? ["--pg", pgDSN] : ["--db", db];
  const pollArgs = pollMs !== undefined ? ["--poll", String(pollMs)] : [];
  const concArgs = maxConcurrent !== undefined ? ["--max-concurrent", String(maxConcurrent)] : [];
  const retryArgs = immediateRetries ? ["--immediate-retries"] : [];
  // Optional lease overrides via env (used by the benchmark to tune the lease).
  const leaseArgs = [
    ...(process.env.GENROC_LEASE_DURATION ? ["--lease-duration", process.env.GENROC_LEASE_DURATION] : []),
    ...(process.env.GENROC_LEASE_RENEW_INTERVAL ? ["--lease-renew-interval", process.env.GENROC_LEASE_RENEW_INTERVAL] : []),
  ];
  // Optional pool sizing via env (used by the stress test to keep a fleet within max_connections).
  const poolArgs = process.env.GENROC_PG_MAX_OPEN_CONNS
    ? ["--pg-max-open-conns", process.env.GENROC_PG_MAX_OPEN_CONNS]
    : [];
  // Optional SQLite durability via env (used by the benchmark to compare engines at
  // matched durability, e.g. GENROC_SQLITE_SYNCHRONOUS=FULL). Ignored for Postgres.
  const syncArgs = [
    ...(process.env.GENROC_SQLITE_SYNCHRONOUS
      ? ["--sqlite-synchronous", process.env.GENROC_SQLITE_SYNCHRONOUS]
      : []),
    // On macOS a plain fsync(2) does not flush the drive cache, so an unset flag here
    // makes synchronous=FULL benchmark ~20x faster than it would on real storage.
    ...(process.env.GENROC_SQLITE_FULLFSYNC ? ["--sqlite-fullfsync"] : []),
  ];
  return spawn(bin, [...dbArgs, "--http", `:${port}`, "--log", "error", ...pollArgs, ...concArgs, ...retryArgs, ...leaseArgs, ...poolArgs, ...syncArgs], {
    stdio: "ignore",
    // Fixed config fixtures for the config e2e test. The test's process names are
    // random, so we use the global tier (GENROC_GLOBAL_<NAME> → config.<NAME>).
    env: {
      ...process.env,
      GENROC_GLOBAL_E2E_URL: "https://config.example.test",
      GENROC_GLOBAL_E2E_PORT: "8080",
      GENROC_GLOBAL_E2E_TOKEN: "supersecret-token-value",
      // Config-sourced URL for secret_log_test's "secret config value in the URL"
      // case. A config value is baked in here at server start (a random port-0 can't
      // be known yet), so each file needing one gets its OWN fixed port — Vitest runs
      // test files in parallel, and two files sharing a port would collide.
      GENROC_GLOBAL_SERVER_URL: "http://localhost:14100",
      // Dedicated fixed port for endpoint_template_test (avoids the 14100 clash).
      GENROC_GLOBAL_ENDPOINT_URL: "http://localhost:14101",
      // A secret config value for the API-redaction test.
      GENROC_GLOBAL_API_KEY: "supersecret-api-key",
      // Retry policy driven from the environment (retry_policy_test). The schema default
      // is 0, so a run that retries at all proves the env value reached the curve.
      GENROC_GLOBAL_E2E_RETRY_ATTEMPTS: "2",
    },
  });
}

// Refuse to start on a port something else already holds. The exit check below is not
// enough on its own: a readiness probe answered by the *other* server can win the race
// against noticing our own process died binding, and the caller then drives someone else's
// engine — which surfaces as an assertion about behaviour, never as a port error. Vitest
// runs files in parallel, so this is reachable whenever two files pick the same number.
async function assertPortFree(port: number): Promise<void> {
  const probe = createServer();
  await new Promise<void>((resolve, reject) => {
    probe.once("error", (err: NodeJS.ErrnoException) =>
      reject(
        err.code === "EADDRINUSE"
          ? new Error(
              `port ${port} is already in use — another test file is serving on it; ` +
                `give this one a port of its own`,
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
      const r = await fetch(`http://localhost:${port}/openapi.json`);
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
  stop: () => Promise<void>; // SIGTERM — resolves once the process has fully exited
  crash: () => void; // SIGKILL — simulate a hard crash, lease stays in DB
  // The SQLite file this server was started on, so a test can assert on columns the API
  // does not expose (task_epoch, parent_task_epoch). Empty when running against Postgres.
  dbPath: string;
}

export async function startGenroc(
  bin: string,
  port: number,
  db: string,
  pgDSN?: string,
  pollMs?: number,
  maxConcurrent?: number,
  immediateRetries?: boolean,
): Promise<GenrocProcess> {
  await assertPortFree(port);
  const proc = spawnProc(bin, port, db, pgDSN, pollMs, maxConcurrent, immediateRetries);
  await waitUntilReady(port, proc);
  return {
    client: createClientTyped({ baseUrl: `http://localhost:${port}` }),
    stop: () => stopProc(proc),
    crash: () => proc.kill("SIGKILL"),
    dbPath: pgDSN ? "" : db,
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
  ];
}

// startSupervisedWorker runs one genroc worker process and restarts it whenever it
// exits — exactly what a process supervisor (systemd, k8s) does for a worker fleet.
// Nothing inside the engine exits on its own anymore (lease pressure is repaired by
// the gate or refused per-write by the fence — see specs/lease-fencing.md), so
// restarts() counts only real deaths: a crash() here, an OOM kill in production. The
// supervisor brings the worker back with a fresh pid, its abandoned leases expire,
// and the restarted process reclaims them.
export async function startSupervisedWorker(
  bin: string,
  port: number,
  o: WorkerOpts,
): Promise<SupervisedWorker> {
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
    const r = await fetch(`${BASE_URL}/openapi.json`);
    await r.body?.cancel();
    return r.ok;
  } catch {
    return false;
  }
}

export async function setup() {
  if (await ping()) return;
  console.log("\nBuilding test server…");
  const bin = await buildGenrocBinary();
  const db = tmpPath("genroc", ".db");
  sharedServer = await startGenroc(bin, PORT as number, db);
}

export async function teardown() {
  await sharedServer?.stop();
}

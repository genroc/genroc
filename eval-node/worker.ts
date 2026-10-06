#!/usr/bin/env node
// The queue worker: claims parked `external` script tasks, evaluates each in its own realm, and
// answers. The only genroc-facing half (README.md; specs/external-task-queue.md).

import { readFileSync } from "node:fs";
import { Cancelled, evaluate, type EvalRequest, type FailureKind } from "./eval.ts";

const SERVER = (process.env.GENROC_SERVER ?? "http://localhost:8448").replace(/\/$/, "");
const WORKER_ID = process.env.WORKER_ID ?? `evaluator-${process.pid}`;
// Needs only the `worker` permission (specs/api-auth.md §5): this credential sits on the machine
// you trust least. A header, because Node's fetch refuses credentials in a URL. The file form
// keeps it out of `docker inspect` and child environments; the inline variable wins.
const TOKEN =
  process.env.GENROC_TOKEN ??
  (process.env.GENROC_TOKEN_FILE ? readFileSync(process.env.GENROC_TOKEN_FILE, "utf8").trim() : "");
const authHeaders: Record<string, string> = TOKEN ? { authorization: `Bearer ${TOKEN}` } : {};
// The worker claims only what it can run, so a backlog is a queue rather than threads fighting
// over a core.
const CONCURRENCY = Number(process.env.CONCURRENCY ?? 4);
const POLL_MS = Number(process.env.POLL_MS ?? 250);
// The visibility timeout. Short, and renewed while work is in flight: a worker that dies
// should return its task quickly rather than holding it for the whole budget.
const LEASE_MS = Number(process.env.LEASE_MS ?? 30_000);
const RENEW_MS = Math.max(1_000, Math.floor(LEASE_MS / 3));
// The answer has to land before the task's deadline fires, or a script that overran surfaces as
// external.timeout ("nobody answered") rather than as its own `timeout`.
const ANSWER_MARGIN_MS = 250;
const PROCESS_FILTER = process.env.PROCESS ?? "";
const TASK_FILTER = process.env.TASK ?? "";

type ObjectEntry = { path: (string | number)[]; ref: string; size: number };

type QueueTask = {
  token: string;
  process: string;
  task: string;
  external_input: unknown;
  objects?: ObjectEntry[];
  raises?: Record<string, unknown>;
  deadline_in_ms?: number;
};

// Large values arrive as refs; a bundle is one object per definition version, fetched once.
// Refs are content hashes, so the cache never invalidates.
const objectCache = new Map<string, unknown>();

async function fetchObject(ref: string): Promise<unknown> {
  const cached = objectCache.get(ref);
  if (cached !== undefined) return cached;
  // Left to throw: a task whose input cannot be fetched must not run against a missing value.
  // The caller releases the claim.
  const res = await fetch(`${SERVER}/api/objects/${encodeURIComponent(ref)}`, { headers: authHeaders });
  if (!res.ok) throw new Error(`fetch object ${ref}: HTTP ${res.status}`);
  const { data } = (await res.json()) as { data: string };
  let value: unknown;
  try {
    value = JSON.parse(data);
  } catch {
    value = data;
  }
  objectCache.set(ref, value);
  return value;
}

/** Put each listed value back where its path says it belongs. Paths are arrays of keys, so this
 *  needs no parser: the whole reason they are not JSON Pointer strings. */
async function resolveObjects(job: QueueTask): Promise<unknown> {
  let input: any = job.external_input;
  for (const e of job.objects ?? []) {
    const value = await fetchObject(e.ref);
    if (e.path.length === 0) {
      input = value;
      continue;
    }
    // The path is rooted at the entry and starts with "external_input", the value being rebuilt.
    const rest = e.path[0] === "external_input" ? e.path.slice(1) : e.path;
    if (rest.length === 0) {
      input = value;
      continue;
    }
    let cur: any = input;
    for (let i = 0; i < rest.length - 1; i++) cur = cur?.[rest[i]!];
    if (cur) cur[rest[rest.length - 1]!] = value;
  }
  return input;
}

async function call(path: string, body: unknown): Promise<{ ok: boolean; status: number; data: any }> {
  // A network error is a REPLY (status 0, "never reached the server"), not a throw: a worker
  // outlives restarts and deploys, and an unhandled rejection would kill it.
  let res: Response;
  try {
    res = await fetch(SERVER + path, {
      method: "POST",
      headers: { "content-type": "application/json", ...authHeaders },
      body: JSON.stringify(body),
    });
  } catch (err) {
    return { ok: false, status: 0, data: { error: `${SERVER} unreachable: ${(err as Error).message}` } };
  }
  const text = await res.text();
  let data: any = null;
  try {
    data = text ? JSON.parse(text) : null;
  } catch {
    data = { error: text };
  }
  return { ok: res.ok, status: res.status, data };
}

/** A task input that is not an EvalRequest is the definition's fault, reported as compile_error:
 *  the nearest permanent kind, since no retry fixes the definition. */
function asEvalRequest(input: unknown, timeoutMs: number | undefined): EvalRequest | string {
  if (typeof input !== "object" || input === null) return "the task input is not an object";
  const r = input as Record<string, unknown>;
  if (typeof r.code !== "string") return "the task input has no `code` string";
  return { code: r.code, input: r.input, timeout_ms: timeoutMs };
}

/** genroc cannot call us, so a cancellation arrives on our own heartbeat and reaches the
 *  evaluation through the controller. */
type Running = { job: QueueTask; abort: AbortController };

const inFlight = new Map<string, Running>();
let running = true;

// Announce reachability TRANSITIONS only: polling several times a second would otherwise emit
// thousands of lines during a restart.
let serverReachable = true;

async function claim(n: number): Promise<QueueTask[]> {
  const { ok, status, data } = await call("/api/external-tasks/claim", {
    worker_id: WORKER_ID,
    limit: n,
    lease_ms: LEASE_MS,
    ...(PROCESS_FILTER ? { process: PROCESS_FILTER } : {}),
    ...(TASK_FILTER ? { task: TASK_FILTER } : {}),
  });
  if (!ok) {
    // A credential problem is not transient, and polling through it looks like a healthy idle
    // worker. Exit, so a supervisor restarts it visibly.
    if (status === 401 || status === 403) {
      console.error(
        `claim rejected (${status}): ${JSON.stringify(data)}\n` +
          `The server requires authentication. Set GENROC_TOKEN to a token with the 'worker' ` +
          `permission — mint one with: genctl token create --perms worker --label evaluator -q`,
      );
      process.exit(1);
    }
    // status 0 (see call): a restart or blip the next poll clears, so it is reported once.
    if (status === 0) {
      if (serverReachable) {
        serverReachable = false;
        console.error(
          `genroc at ${SERVER} is unreachable — ${(data as { error?: string })?.error ?? ""}. ` +
            `Still polling every ${POLL_MS}ms; work resumes when it comes back.`,
        );
      }
      return [];
    }
    console.error(`claim failed: ${JSON.stringify(data)}`);
    return [];
  }
  if (!serverReachable) {
    serverReachable = true;
    console.error(`genroc at ${SERVER} is reachable again — resuming.`);
  }
  return (data?.items ?? []) as QueueTask[];
}

async function release(token: string): Promise<void> {
  const { ok, data } = await call("/api/external-tasks/release", { token });
  if (!ok) console.error(`release failed: ${JSON.stringify(data)}`);
}

/** A refused outcome is not retried with another: the worker does not satisfy the declared
 *  contract. Released, so an operator sees the task waiting rather than gone. */
async function answer(token: string, outcome: Record<string, unknown>): Promise<void> {
  const { ok, data } = await call("/api/external-tasks/resolve", { token, ...outcome });
  if (ok) return;
  console.error(`genroc refused the outcome for ${token}: ${JSON.stringify(data)}`);
  await release(token);
}

async function run(job: QueueTask, signal: AbortSignal): Promise<void> {
  // The task's own timeout is the script's budget. Taken on arrival, because fetching the
  // objects below spends the same deadline.
  const due = job.deadline_in_ms === undefined ? undefined : performance.now() + job.deadline_in_ms;
  let resolved: unknown;
  try {
    resolved = await resolveObjects(job);
  } catch (err) {
    // The values are there or they are not; this is the runner failing to read them, not the
    // script failing, so hand the task back for someone else rather than reporting an outcome.
    console.error(`resolving objects for ${job.token}: ${err instanceof Error ? err.message : String(err)}`);
    await release(job.token);
    return;
  }
  let timeoutMs: number | undefined;
  if (due !== undefined) {
    timeoutMs = Math.floor(due - performance.now() - ANSWER_MARGIN_MS);
    if (timeoutMs <= 0) {
      // Too late to run anything: the deadline fires as external.timeout, which is the truth.
      await release(job.token);
      return;
    }
  }
  const req = asEvalRequest(resolved, timeoutMs);
  if (typeof req === "string") {
    await answer(job.token, {
      error: { code: "compile_error", message: req, data: { name: "BadTaskInput" } },
    });
    return;
  }

  let result;
  try {
    result = await evaluate(req, signal);
  } catch (err) {
    // Cancelled is not a fault. Still released: the row is terminal so nothing re-claims it,
    // but the release stops it waiting out a lease nobody serves.
    if (err instanceof Cancelled) {
      console.log(`cancelled: ${job.token}`);
      await release(job.token);
      return;
    }
    // The RUNNER faulted, the one retryable class. Releasing is how a queue spells "retryable",
    // and it reaches another worker without spending the definition's on_error budget.
    console.error(`evaluator fault on ${job.token}: ${err instanceof Error ? err.message : String(err)}`);
    await release(job.token);
    return;
  }

  if (result.ok) {
    // `body` is JSON text produced inside the realm; an empty body is a script that returned
    // nothing, which genroc reads as null.
    await answer(job.token, { result: result.body === "" ? null : JSON.parse(result.body) });
    return;
  }
  const f = result.failure;
  await answer(job.token, {
    error: {
      // The failure KIND is the code, so an on_error rule branches on what went wrong without
      // reading a payload. Every kind is permanent; see eval.ts.
      code: f.kind satisfies FailureKind,
      message: f.message,
      data: { name: f.name, ...(f.stack ? { stack: f.stack } : {}) },
    },
  });
}

async function renewLoop(): Promise<void> {
  while (running) {
    await new Promise((r) => setTimeout(r, RENEW_MS));
    const tokens = [...inFlight.keys()];
    if (!tokens.length) continue;
    const { ok, data } = await call("/api/external-tasks/renew", {
      worker_id: WORKER_ID,
      tokens,
      lease_ms: LEASE_MS,
    });
    if (!ok) continue;

    // Abort first rather than let a stopped script burn a core; the release rides run()'s
    // catch, so nothing is released twice.
    for (const token of (data?.cancelled ?? []) as string[]) {
      inFlight.get(token)?.abort.abort();
    }
    // Lost: another worker holds it now and our answer will be refused; logged as the sign that
    // LEASE_MS is too short. NOT released: that would bump the epoch out from under the holder.
    const lost = (data?.lost ?? []) as string[];
    if (lost.length) {
      console.error(`lost ${lost.length}/${tokens.length} claims; a lease lapsed under load`);
    }
  }
}

async function pollLoop(): Promise<void> {
  while (running) {
    const free = CONCURRENCY - inFlight.size;
    const jobs = free > 0 ? await claim(free) : [];
    for (const job of jobs) {
      const abort = new AbortController();
      inFlight.set(job.token, { job, abort });
      void run(job, abort.signal).finally(() => inFlight.delete(job.token));
    }
    // Only idle when there was nothing to take: a full queue should be drained at the speed the
    // realms allow, not at the poll interval.
    if (jobs.length === 0) await new Promise((r) => setTimeout(r, POLL_MS));
  }
}

async function shutdown(signal: string): Promise<void> {
  if (!running) return;
  running = false;
  const tokens = [...inFlight.keys()];
  console.log(`${signal}: releasing ${tokens.length} claim(s)`);
  // Hand work back rather than letting it sit out its lease. The evaluations still running are
  // abandoned, which is exactly what the release says: nobody answered.
  await Promise.all(tokens.map(release));
  process.exit(0);
}

process.on("SIGINT", () => void shutdown("SIGINT"));
process.on("SIGTERM", () => void shutdown("SIGTERM"));

console.log(
  `evaluator worker ${WORKER_ID} polling ${SERVER} (concurrency=${CONCURRENCY}, lease=${LEASE_MS}ms)`,
);
void renewLoop();
void pollLoop();

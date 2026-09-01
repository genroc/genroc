// The queue worker: claims parked `external` script tasks from genroc, evaluates each in its
// own realm, and answers. This is the whole genroc-facing half — eval.ts and realm.ts know
// nothing about the queue, which is what keeps the containment strategy swappable.
//
// See README.md for the contract, and specs/external-task-queue.md for the queue itself.

import { evaluate, type EvalRequest, type FailureKind } from "./eval.ts";

const SERVER = (process.env.GENROC_SERVER ?? "http://localhost:8448").replace(/\/$/, "");
const WORKER_ID = process.env.WORKER_ID ?? `evaluator-${process.pid}`;
// The credential, when the server runs with --auth token. A worker needs exactly the `worker`
// permission — the four queue verbs plus GET /api/objects — so mint it scoped rather than
// handing a worker an admin token: this is the credential most likely to sit on a machine you
// trust least. specs/api-auth.md §5.
//
// Sent as a header rather than in the URL because Node's fetch REFUSES a URL carrying
// credentials ("Request cannot be constructed from a URL that includes credentials"), so the
// basic-auth-in-the-URL trick that works for genctl is not available here.
const TOKEN = process.env.GENROC_TOKEN ?? "";
const authHeaders: Record<string, string> = TOKEN ? { authorization: `Bearer ${TOKEN}` } : {};
// Concurrency is the worker's to set, and that is the point of pulling: under the old fetch
// shape genroc decided how many scripts ran at once (--max-concurrent, default 200) and the
// evaluator accepted every one of them. Here it claims what it can run and no more, so a
// backlog is a queue rather than 200 threads fighting over a core.
const CONCURRENCY = Number(process.env.CONCURRENCY ?? 4);
const POLL_MS = Number(process.env.POLL_MS ?? 250);
// The visibility timeout. Short, and renewed while work is in flight: a worker that dies
// should return its task quickly rather than holding it for the whole budget.
const LEASE_MS = Number(process.env.LEASE_MS ?? 30_000);
const RENEW_MS = Math.max(1_000, Math.floor(LEASE_MS / 3));
const PROCESS_FILTER = process.env.PROCESS ?? "";
const TASK_FILTER = process.env.TASK ?? "";

type ObjectEntry = { path: (string | number)[]; ref: string; size: number };

type QueueTask = {
  token: string;
  process: string;
  task_id: string;
  input: unknown;
  objects?: ObjectEntry[];
  raises?: Record<string, unknown>;
};

// Values too large to ship inline are listed rather than carried, and a bundle is exactly that:
// one object shared by every instance of a definition version, fetched once instead of copied
// into each task. A ref is a content hash, so it is immutable — the cache never invalidates.
const objectCache = new Map<string, unknown>();

async function fetchObject(ref: string): Promise<unknown> {
  const cached = objectCache.get(ref);
  if (cached !== undefined) return cached;
  // Left to throw on purpose: this runs while a task is IN FLIGHT, and a task whose input
  // cannot be fetched must fail rather than silently run against a missing value. The caller
  // releases the claim, so the task returns to the queue.
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
  let input: any = job.input;
  for (const e of job.objects ?? []) {
    const value = await fetchObject(e.ref);
    if (e.path.length === 0) {
      input = value;
      continue;
    }
    // The path is rooted at the entry and starts with "input", which is the value being rebuilt.
    const rest = e.path[0] === "input" ? e.path.slice(1) : e.path;
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
  // A network error is a REPLY, not a throw. A worker outlives the server it polls — a
  // restart, a rolling deploy, a container coming up before genroc is listening — and an
  // unhandled rejection here kills it for a condition the next poll would clear. Status 0
  // says "never reached the server", which is distinct from anything genroc answers.
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

/** The task input IS an EvalRequest: `code` required, `input` and `timeout_ms` optional. A task
 *  whose input is not that shape is the definition's fault, not the script's, and is reported
 *  as a compile_error — the nearest permanent kind, since no retry can fix the definition. */
function asEvalRequest(input: unknown): EvalRequest | string {
  if (typeof input !== "object" || input === null) return "the task input is not an object";
  const r = input as Record<string, unknown>;
  if (typeof r.code !== "string") return "the task input has no `code` string";
  return {
    code: r.code,
    input: r.input,
    timeout_ms: typeof r.timeout_ms === "number" ? r.timeout_ms : undefined,
  };
}

const inFlight = new Map<string, QueueTask>();
let running = true;

// Whether the last claim reached genroc. A worker polls several times a second, so an
// unreachable server would otherwise emit a line per poll — thousands during a restart, which
// buries the one line that mattered. Announce the TRANSITIONS instead: going away, and coming
// back. Silence in between is the report that nothing changed.
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
    // A credential problem is not transient, and polling through it looks like a healthy
    // worker that never picks anything up — the worst shape for an operator to debug. Exit
    // instead, so a supervisor restarts it and the failure is visible where it happened.
    if (status === 401 || status === 403) {
      console.error(
        `claim rejected (${status}): ${JSON.stringify(data)}\n` +
          `The server requires authentication. Set GENROC_TOKEN to a token with the 'worker' ` +
          `permission — mint one with: genctl token create --perms worker --label evaluator -q`,
      );
      process.exit(1);
    }
    // status 0 is "never reached the server" (see call): a restart, a rolling deploy, a
    // network blip. Not an error to act on — the next poll clears it — so it is reported once
    // and then waited out.
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

/** answer submits the outcome. A refusal is NOT retried with a different one: the definition
 *  declared a contract this worker does not satisfy (an undeclared code, a payload that does
 *  not fit `raises`), and guessing again would only pick a second wrong answer. Release it, so
 *  the task returns to the queue and an operator sees it waiting rather than silently gone. */
async function answer(token: string, outcome: Record<string, unknown>): Promise<void> {
  const { ok, data } = await call("/api/external-tasks/resolve", { token, ...outcome });
  if (ok) return;
  console.error(`genroc refused the outcome for ${token}: ${JSON.stringify(data)}`);
  await release(token);
}

async function run(job: QueueTask): Promise<void> {
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
  const req = asEvalRequest(resolved);
  if (typeof req === "string") {
    await answer(job.token, {
      error: { code: "compile_error", message: req, data: { name: "BadTaskInput" } },
    });
    return;
  }

  let result;
  try {
    result = await evaluate(req);
  } catch (err) {
    // The RUNNER faulted, not the script — the one class where a retry can help. There is no
    // error code for it on purpose: releasing the claim is how a queue spells "retryable", and
    // it puts the task in front of a different worker instead of burning the definition's
    // on_error budget on this one's bad day.
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
    // A short count means a claim lapsed and was taken over. Nothing to do about it — the work
    // continues and its answer will be refused — but say so, because it is the signal that
    // LEASE_MS is too short for what these scripts actually take.
    if (ok && data?.renewed < tokens.length) {
      console.error(`renewed ${data.renewed}/${tokens.length} claims; a lease lapsed under load`);
    }
  }
}

async function pollLoop(): Promise<void> {
  while (running) {
    const free = CONCURRENCY - inFlight.size;
    const jobs = free > 0 ? await claim(free) : [];
    for (const job of jobs) {
      inFlight.set(job.token, job);
      void run(job).finally(() => inFlight.delete(job.token));
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

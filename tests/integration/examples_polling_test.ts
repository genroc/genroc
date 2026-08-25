import { createServer } from "http";
import type { AddressInfo } from "net";
import { readFileSync } from "node:fs";
import { load as loadYaml } from "js-yaml";
import { expect, test } from "vitest";
import { client, waitForInstance } from "../helpers/client.ts";

// The definitions under test are the real example files in examples/polling-task/, loaded
// and applied verbatim — so this doubles as an executable check that the shipped example
// works end to end. The poller returns the job's payload as `unknown` plus a typed
// `attempts`, and the parent narrows the payload with a result_schema; running out of
// attempts is RAISED as `poll_timeout` and caught on the child task. (Vitest's bundler
// can't `import` a .yaml file, so we read + parse the source instead.)
const EXAMPLES = new URL("../../examples/polling-task/", import.meta.url);
function loadDef(file: string): any {
  return loadYaml(readFileSync(new URL(file, EXAMPLES), "utf8"));
}
const poller = loadDef("poller.genroc.yaml");
const parent = loadDef("parent.genroc.yaml");

// startJobService stands in for the remote server the poller talks to. It signals
// progress with the HTTP STATUS, not a body field — which is what lets the poller treat
// both response bodies as opaque.
//   POST /jobs   -> 200 { job_id }  starts a job (the poller never reads this body)
//   POST /status -> 202 {}          still running, for the first `pendingPolls` checks
//                -> 200 result      done
// Jobs are keyed by the caller-supplied `ref`, since the poller cannot carry a
// server-assigned id from one request to the other. Every request must carry
// `expectedAuth` or it's rejected 401 — so a completed run proves the auth header the
// parent set reached the service on each call.
async function startJobService(
  pendingPolls: number,
  result: Record<string, unknown>,
  expectedAuth: string,
) {
  let startCount = 0;
  let statusRequests = 0;
  const pollsByJob = new Map<string, number>();
  const authSeen = new Set<string>();

  const server = createServer((req, res) => {
    const chunks: Buffer[] = [];
    req.on("data", (c) => chunks.push(c as Buffer));
    req.on("end", () => {
      const body = chunks.length ? JSON.parse(Buffer.concat(chunks).toString()) : {};
      const send = (code: number, obj: unknown) => {
        res.writeHead(code, { "Content-Type": "application/json" });
        res.end(JSON.stringify(obj));
      };

      const auth = req.headers["authorization"];
      if (typeof auth === "string") authSeen.add(auth);
      if (auth !== expectedAuth) return send(401, { error: "unauthorized" });

      if (req.url === "/jobs") {
        startCount++;
        const ref = body.ref as string;
        pollsByJob.set(ref, 0);
        send(200, { job_id: `job-${startCount}` });
      } else if (req.url === "/status") {
        statusRequests++;
        const ref = body.ref as string;
        const seen = (pollsByJob.get(ref) ?? 0) + 1;
        pollsByJob.set(ref, seen);
        if (seen <= pendingPolls) send(202, {});
        else send(200, result);
      } else {
        send(404, {});
      }
    });
  });

  await new Promise<void>((r) => server.listen(0, r));
  return {
    port: (server.address() as AddressInfo).port,
    startCount: () => startCount,
    statusRequests: () => statusRequests,
    authHeaders: () => [...authSeen],
    stop: () => new Promise<void>((r) => server.close(() => r())),
  };
}

const AUTH_TOKEN = "s3cr3t-token";
let refCounter = 0;

// Apply the example definitions exactly as shipped — child before parent so the parent's
// child reference resolves at registration.
async function applyExample() {
  for (const def of [poller, parent]) {
    const { error } = await client.PUT("/definitions", { body: def as never });
    expect(error).toBeUndefined();
  }
}

async function startExample(port: number, extra: Record<string, unknown> = {}): Promise<string> {
  const { data, error } = await client.POST("/instances", {
    body: {
      process: parent.name,
      input: {
        url: `http://localhost:${port}`,
        ref: `run-${++refCounter}`,
        headers: { Authorization: `Bearer ${AUTH_TOKEN}` },
        ...extra,
      },
    },
  });
  expect(error).toBeUndefined();
  return data!.id;
}

// The parent records its spawned child under context._children.<taskId>; a single `child`
// task stores the bare child id there (not a keyed map), so here it's _children.run. Poll
// until it appears and return the child instance id.
async function waitForChildId(parentId: string, timeoutMs = 10_000): Promise<string> {
  const deadline = Date.now() + timeoutMs;
  while (Date.now() < deadline) {
    const { data } = await client.GET("/instances/{id}", { params: { path: { id: parentId } } });
    const childId = (data?.context as any)?._children?.run;
    if (typeof childId === "string") return childId;
    await new Promise((r) => setTimeout(r, 50));
  }
  throw new Error(`child of ${parentId} was not spawned within ${timeoutMs}ms`);
}

async function outputsOf(id: string): Promise<Record<string, any>> {
  const { data } = await client.GET("/instances/{id}", { params: { path: { id } } });
  return ((data?.context as any)?.outputs ?? {}) as Record<string, any>;
}

test("examples/polling-task: the poller returns the job's answer to the parent", async () => {
  const pendingPolls = 2; // two "pending" replies, then "done" on the third check
  // The payload carries a field the parent's result_schema does not declare, proving the
  // poller really is agnostic: it forwards whatever the job produced, and narrowing at the
  // parent strips the surplus rather than rejecting it.
  const mock = await startJobService(pendingPolls, { answer: 42, debug: "ignored" }, `Bearer ${AUTH_TOKEN}`);

  try {
    await applyExample();
    const id = await startExample(mock.port, { poll_interval_ms: 50 });

    expect(await waitForInstance(id, 20_000)).toBe("completed");

    // The payload travelled up opaque and came out typed: `answer` is readable only
    // because the parent narrowed it, `attempts` was typed by the child all along, and
    // `debug` was dropped by the narrowing conform. The error path was never taken.
    const outputs = await outputsOf(id);
    expect(outputs.run).toEqual({ answer: 42, attempts: pendingPolls + 1 });
    expect(outputs.report).toBeUndefined();

    // Started once, polled until done (pendingPolls "pending" + 1 "done").
    expect(mock.startCount()).toBe(1);
    expect(mock.statusRequests()).toBe(pendingPolls + 1);

    // Every request carried exactly the auth header the parent threaded down.
    expect(mock.authHeaders()).toEqual([`Bearer ${AUTH_TOKEN}`]);
  } finally {
    await mock.stop();
  }
});

// The trade-off `unknown` makes: because the poller never inspects the payload, a
// malformed one is not caught where it is produced (the child's fetch accepts it — the
// slot is unknown) but where it is consumed, when the parent's result_schema conforms the
// collected child output. Later, and outside the child's own on_error scope.
test("examples/polling-task: a payload that fails the parent's narrowing is caught at the parent", async () => {
  const pendingPolls = 1;
  // `answer` is a string; the parent's result_schema declares it a number.
  const mock = await startJobService(pendingPolls, { answer: "forty-two" }, `Bearer ${AUTH_TOKEN}`);

  try {
    await applyExample();
    const id = await startExample(mock.port, { poll_interval_ms: 50 });

    // The CHILD completes — it polled successfully and carried the payload out untouched.
    const childId = await waitForChildId(id);
    expect(await waitForInstance(childId, 20_000)).toBe("completed");

    // The PARENT fails, on the narrowing conform at collect. `poll_timeout` is the only
    // code its on_error catches, so a type violation is not swallowed.
    expect(await waitForInstance(id, 20_000)).toBe("failed");
    const { data } = await client.GET("/instances/{id}", { params: { path: { id } } });
    expect(data?.error_message ?? "").toMatch(/output validation/i);
  } finally {
    await mock.stop();
  }
});

test("examples/polling-task: exhausting max_attempts raises `poll_timeout`, which the parent catches", async () => {
  // The job never reports done; the caller caps it at two attempts.
  const mock = await startJobService(Number.MAX_SAFE_INTEGER, { answer: 42 }, `Bearer ${AUTH_TOKEN}`);

  try {
    await applyExample();
    const id = await startExample(mock.port, { poll_interval_ms: 50, max_attempts: 2 });

    expect(await waitForInstance(id, 20_000)).toBe("completed");

    // The child gave up and raised `poll_timeout`; the parent caught it.
    const outputs = await outputsOf(id);
    expect(outputs.report).toEqual({
      outcome: "poll_timeout",
      detail: "gave up after max_attempts polls",
    });

    // Exactly max_attempts status checks before it gave up.
    expect(mock.startCount()).toBe(1);
    expect(mock.statusRequests()).toBe(2);
  } finally {
    await mock.stop();
  }
});

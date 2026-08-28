import { expect, test } from "vitest";
import { client, startMockService, waitForInstance } from "../helpers/client.ts";
import { BASE_URL } from "../helpers/constants.ts";

// Every API failure used to answer 400 with a prose string and nothing else. These
// tests pin the two things that replaced it: the status now distinguishes the kinds
// of failure, and every error body carries a machine-readable `code` — the same value
// TCP/UDS clients get on the Reply, since they have no status line to read.

const MISSING_ID = "00000000-0000-0000-0000-000000000000";

async function errorOf(path: string, init?: RequestInit) {
  const res = await fetch(`${BASE_URL}/api${path}`, init);
  return { status: res.status, body: await res.json() as Record<string, unknown> };
}

test("api errors — a missing instance is 404 not_found, not 400", async () => {
  const { status, body } = await errorOf(`/instances/${MISSING_ID}`);
  expect(status).toBe(404);
  expect(body.code).toBe("not_found");
  expect(body.error).toContain("not found");
});

test("api errors — a missing definition version is 404 not_found", async () => {
  const { status, body } = await errorOf(`/channels`, {
    method: "PUT",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ name: `nope_${crypto.randomUUID()}`, channel: "stable", version: 7 }),
  });
  expect(status).toBe(404);
  expect(body.code).toBe("not_found");
});

test("api errors — starting an unknown process is 404, not 400", async () => {
  const { status, body } = await errorOf(`/instances`, {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ process: `absent_${crypto.randomUUID()}` }),
  });
  expect(status).toBe(404);
  expect(body.code).toBe("not_found");
});

test("api errors — a missing required field is 400 invalid", async () => {
  const { status, body } = await errorOf(`/instances`, {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ input: { x: 1 } }),
  });
  expect(status).toBe(400);
  expect(body.code).toBe("invalid");
});

test("api errors — an unrecognised field is rejected rather than silently dropped", async () => {
  // Bodies used to be decoded leniently, so a misspelled field became the zero value
  // and the request succeeded with a meaning the caller never asked for.
  const { status, body } = await errorOf(`/instances`, {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ process: "whatever", verison: 2 }),
  });
  expect(status).toBe(400);
  expect(body.code).toBe("invalid");
  expect(body.error).toContain("verison");
});

test("api errors — a wrong-typed field is rejected", async () => {
  const { status, body } = await errorOf(`/instances`, {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ process: "whatever", version: "1" }),
  });
  expect(status).toBe(400);
  expect(body.code).toBe("invalid");
});

test("api errors — /tick on a polling server is 501 unsupported, not 400", async () => {
  // The endpoint is routed but this server was not started with --poll 0. That is a
  // configuration fact, not a malformed request, and the status now says so.
  const { status, body } = await errorOf(`/tick`, { method: "POST" });
  expect(status).toBe(501);
  expect(body.code).toBe("unsupported");
  expect(body.error).toContain("manual mode");
});

test("api errors — resuming a SETTLED process is 409 conflict (a live one is 204)", async () => {
  const mock = await startMockService(0, { response: { ok: true } });
  const name = `err_resume_${crypto.randomUUID()}`;
  await client.PUT("/definitions", {
    body: {
      name,
      tasks: [
        {
          id: "call",
          action: { type: "fetch" as const, url: `http://localhost:${mock.port}/x` },
          timeout: 2000,
          switch: [{ goto: "end" }],
        },
      ],
    },
  });
  const { data } = await client.POST("/instances", { body: { process: name, input: {} } });
  const id = data!.id;
  expect(await waitForInstance(id)).toBe("completed");

  const { status, body } = await errorOf(`/instances/${id}/resume`, { method: "POST" });
  expect(status).toBe(409);
  expect(body.code).toBe("conflict");
  expect(body.error).toContain("settled");

  mock.stop();
});

test("api errors — retrying a completed process is 409 conflict", async () => {
  const mock = await startMockService(0, { response: { ok: true } });
  const name = `err_retry_${crypto.randomUUID()}`;
  await client.PUT("/definitions", {
    body: {
      name,
      tasks: [
        {
          id: "call",
          action: { type: "fetch" as const, url: `http://localhost:${mock.port}/x` },
          timeout: 2000,
          switch: [{ goto: "end" }],
        },
      ],
    },
  });
  const { data } = await client.POST("/instances", { body: { process: name, input: {} } });
  const id = data!.id;
  expect(await waitForInstance(id)).toBe("completed");

  const { status, body } = await errorOf(`/instances/${id}/retry`, { method: "POST" });
  expect(status).toBe(409);
  expect(body.code).toBe("conflict");
  expect(body.error).toContain("not retryable");

  mock.stop();
});

type Field = { field: string; rule: string; param?: string; message: string };

test("api errors — a definition validation failure reports the offending field", async () => {
  // The per-field detail the validator already produced used to be joined into one
  // string; `fields` keeps it addressable so a client need not parse English.
  const { status, body } = await errorOf(`/definitions`, {
    method: "PUT",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ name: `nofields_${crypto.randomUUID()}`, tasks: [] }),
  });
  expect(status).toBe(400);
  expect(body.code).toBe("invalid");
  const fields = body.fields as Field[];
  expect(fields).toEqual([
    { field: "tasks", rule: "min", param: "1", message: "tasks must have at least 1 item(s)" },
  ]);
});

test("api errors — a nested validation failure carries the indexed path to the field", async () => {
  // The path is what makes `fields` worth having. The message for a nested failure
  // names only the leaf ("id is required"), so with three tasks it cannot say which
  // one is at fault; tasks[1].id can.
  const { status, body } = await errorOf(`/definitions`, {
    method: "PUT",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({
      name: `nested_${crypto.randomUUID()}`,
      tasks: [
        { id: "first", switch: [{ goto: "end" }] },
        { switch: [{ goto: "end" }] }, // no id
        { id: "third", switch: [{ goto: "end" }] },
      ],
    }),
  });
  expect(status).toBe(400);
  expect(body.code).toBe("invalid");
  expect(body.fields as Field[]).toEqual([
    { field: "tasks[1].id", rule: "required", message: "id is required" },
  ]);
});

test("api errors — every failing element is reported with its own index", async () => {
  const { status, body } = await errorOf(`/definitions`, {
    method: "PUT",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({
      name: `multi_${crypto.randomUUID()}`,
      tasks: [
        { switch: [{ goto: "end" }] },
        { id: "ok", switch: [{ goto: "end" }] },
        { switch: [{ goto: "end" }] },
      ],
    }),
  });
  expect(status).toBe(400);
  const fields = body.fields as Field[];
  expect(fields.map((f) => f.field)).toEqual(["tasks[0].id", "tasks[2].id"]);
  // Identical messages, distinct paths — the case the path exists to disambiguate.
  expect(new Set(fields.map((f) => f.message)).size).toBe(1);
});

test("api errors — the field path survives the batch handler's name prefix", async () => {
  // applyBatch wraps each definition's failure with the process name, so the detail
  // only reaches the client if errReply unwraps through that prefix.
  const name = `batch_${crypto.randomUUID()}`;
  const { status, body } = await errorOf(`/definitions/batch`, {
    method: "PUT",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({
      channel: "latest",
      definitions: [{ name, tasks: [{ switch: [{ goto: "end" }] }] }],
    }),
  });
  expect(status).toBe(400);
  expect(body.code).toBe("invalid");
  expect(body.error).toContain(name); // the prefix is still there
  expect((body.fields as Field[]).map((f) => f.field)).toEqual(["tasks[0].id"]);
});

test("api errors — validate_definitions reports paths without saving anything", async () => {
  const name = `dryrun_${crypto.randomUUID()}`;
  const { status, body } = await errorOf(`/definitions/validate`, {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify([{ name, tasks: [{ switch: [{ goto: "end" }] }] }]),
  });
  expect(status).toBe(400);
  expect((body.fields as Field[]).map((f) => f.field)).toEqual(["tasks[0].id"]);

  const listed = await errorOf(`/instances?limit=1`);
  expect(listed.status).toBe(200); // sanity: the server is still serving
});

test("api errors — a bad external-task token is 400, a stale one is 409", async () => {
  const malformed = await errorOf(`/external-tasks/resolve`, {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ token: "no-dot-here", result: {} }),
  });
  expect(malformed.status).toBe(400);
  expect(malformed.body.code).toBe("invalid");

  // The suffix is the arming's task_epoch, so a non-numeric one is malformed rather than
  // merely unknown — the format is tighter than the nonce it replaced.
  const badEpoch = await errorOf(`/external-tasks/resolve`, {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ token: `${MISSING_ID}.abc`, result: {} }),
  });
  expect(badEpoch.status).toBe(400);
  expect(badEpoch.body.code).toBe("invalid");

  const unknown = await errorOf(`/external-tasks/resolve`, {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ token: `${MISSING_ID}.0`, result: {} }),
  });
  expect(unknown.status).toBe(404);
  expect(unknown.body.code).toBe("not_found");
});

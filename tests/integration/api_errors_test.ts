import { expect, test } from "vitest";
import { client, startMockService, waitForInstance } from "../helpers/client.ts";
import { BASE_URL } from "../helpers/constants.ts";

// Every API failure used to answer 400 with a prose string and nothing else. These
// tests pin the two things that replaced it: the status now distinguishes the kinds
// of failure, and every error body carries a machine-readable `code` — the same value
// TCP/UDS clients get on the Reply, since they have no status line to read.

const MISSING_ID = "00000000-0000-0000-0000-000000000000";

async function errorOf(path: string, init?: RequestInit) {
  const res = await fetch(`${BASE_URL}${path}`, init);
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

test("api errors — resuming a process that is not paused is 409 conflict", async () => {
  const mock = await startMockService(0, { response: { ok: true } });
  const name = `err_resume_${crypto.randomUUID()}`;
  await client.PUT("/definitions", {
    body: {
      name,
      tasks: [
        {
          id: "call",
          action: { type: "fetch" as const, url: `http://localhost:${mock.port}/x` },
          timeout_ms: 2000,
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
  expect(body.error).toContain("not paused");

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
          timeout_ms: 2000,
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
  const fields = body.fields as Array<{ field: string; rule: string; message: string }>;
  expect(Array.isArray(fields)).toBe(true);
  expect(fields.length).toBeGreaterThan(0);
  expect(fields.some((f) => f.field === "tasks")).toBe(true);
  expect(fields.some((f) => f.rule === "min")).toBe(true);
});

test("api errors — a bad external-task token is 400, a stale one is 409", async () => {
  const malformed = await errorOf(`/external-tasks/resolve`, {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ token: "no-dot-here", result: {} }),
  });
  expect(malformed.status).toBe(400);
  expect(malformed.body.code).toBe("invalid");

  const unknown = await errorOf(`/external-tasks/resolve`, {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ token: `${MISSING_ID}.abc`, result: {} }),
  });
  expect(unknown.status).toBe(404);
  expect(unknown.body.code).toBe("not_found");
});

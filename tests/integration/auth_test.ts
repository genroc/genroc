import { afterAll, beforeAll, expect, test } from "vitest";
import { buildGenrocBinary, startGenroc, tmpPath, type GenrocProcess } from "../helpers/server.ts";

// specs/api-auth.md §3, §5. The permission split is only real if it is observed over HTTP —
// a Go unit test can assert `authorize` returns 403, but not that the gate is actually in
// front of every route on every transport.
//
// This server runs with --auth token, unlike the shared one, so the bootstrap credential is
// supplied rather than read out of a log.

const PORT = 8954;
const ADMIN = "genroc_sk_" + "a".repeat(43);
const dbPath = tmpPath("genroc_auth", ".db");
let server: GenrocProcess | undefined;
let base = "";

async function req(path: string, token: string | null, init: RequestInit = {}) {
  const headers: Record<string, string> = { "content-type": "application/json" };
  if (token) headers.authorization = `Bearer ${token}`;
  const res = await fetch(`${base}${path}`, { ...init, headers });
  const text = await res.text();
  return { status: res.status, body: text ? JSON.parse(text) : null };
}

/** Mints a token with the given permissions, using the bootstrap admin. */
async function mint(perms: string[]): Promise<string> {
  const { status, body } = await req("/api/tokens", ADMIN, {
    method: "POST",
    body: JSON.stringify({ label: perms.join("-"), perms }),
  });
  expect(status, `mint ${perms}: ${JSON.stringify(body)}`).toBe(200);
  return body.token as string;
}

beforeAll(async () => {
  const bin = await buildGenrocBinary();
  process.env.GENROC_TEST_AUTH = "token";
  process.env.GENROC_TEST_BOOTSTRAP_TOKEN = ADMIN;
  server = await startGenroc(bin, PORT, dbPath);
  base = `http://localhost:${PORT}`;
}, 120_000);

afterAll(async () => {
  delete process.env.GENROC_TEST_AUTH;
  delete process.env.GENROC_TEST_BOOTSTRAP_TOKEN;
  await server?.stop();
});

test("auth — no credential is 401, a bad one is 401, and neither says which", async () => {
  const none = await req("/api/definitions", null);
  expect(none.status).toBe(401);
  expect(none.body.code).toBe("unauthenticated");

  const bad = await req("/api/definitions", "genroc_sk_nosuchtoken");
  expect(bad.status).toBe(401);
  // A revoked token and one that never existed must be indistinguishable, or the response
  // tells an attacker which of their guesses named a real credential.
  expect(bad.body.code).toBe("unauthenticated");
});

test("auth — the probe answers without a credential, because a supervisor holds none", async () => {
  const res = await fetch(`${base}/healthz`);
  expect(res.status).toBe(200);
});

test("auth — a worker token reaches the inbound zone and nothing else", async () => {
  const worker = await mint(["worker"]);

  const claim = await req("/api/external-tasks/claim", worker, {
    method: "POST",
    body: JSON.stringify({ worker_id: "w1", limit: 1 }),
  });
  expect(claim.status, "a worker must be able to claim").toBe(200);

  // The control plane is the blast radius of a leaked worker credential, so this is the
  // assertion that matters most in the file.
  for (const path of ["/api/definitions", "/api/instances", "/api/channels?name=x", "/api/tokens"]) {
    const { status, body } = await req(path, worker);
    expect(status, `worker reached ${path}`).toBe(403);
    expect(body.code).toBe("forbidden");
  }
});

test("auth — read cannot write, and the 403 names the permission needed", async () => {
  const reader = await mint(["read"]);
  expect((await req("/api/definitions", reader)).status).toBe(200);

  const { status, body } = await req("/api/definitions", reader, {
    method: "PUT",
    body: JSON.stringify({ name: "nope", tasks: [{ id: "a", switch: [{ goto: "end" }] }] }),
  });
  expect(status).toBe(403);
  // Naming the permission is what turns a 403 into an actionable message rather than a guess.
  expect(body.error).toContain("deploy");
});

test("auth — deploy can apply and run, but not mint credentials", async () => {
  const deploy = await mint(["deploy", "operate", "read"]);
  const name = `auth_ok_${crypto.randomUUID().slice(0, 8)}`;

  const applied = await req("/api/definitions", deploy, {
    method: "PUT",
    body: JSON.stringify({ name, tasks: [{ id: "a", switch: [{ goto: "end" }] }] }),
  });
  expect(applied.status, JSON.stringify(applied.body)).toBe(200);

  const started = await req("/api/instances", deploy, {
    method: "POST",
    body: JSON.stringify({ process: name }),
  });
  expect(started.status).toBe(200);

  // Minting is granting access, so it stays admin however much else a token can do.
  expect((await req("/api/tokens", deploy, { method: "POST", body: JSON.stringify({ perms: ["read"] }) })).status).toBe(403);
});

test("auth — revocation takes effect on the next request", async () => {
  const doomed = await mint(["read"]);
  expect((await req("/api/definitions", doomed)).status).toBe(200);

  const listed = await req("/api/tokens", ADMIN);
  const id = (listed.body.items as { id: string; label: string }[]).find((t) => t.label === "read")!.id;
  expect((await req(`/api/tokens/${id}`, ADMIN, { method: "DELETE" })).status).toBe(200);

  expect(
    (await req("/api/definitions", doomed)).status,
    "a revoked token still worked — revocation is the reason the token is opaque rather than signed",
  ).toBe(401);
});

test("auth — an unknown permission is refused at mint, not discovered from a later 403", async () => {
  const { status, body } = await req("/api/tokens", ADMIN, {
    method: "POST",
    body: JSON.stringify({ label: "typo", perms: ["deployy"] }),
  });
  expect(status).toBe(400);
  expect(body.error).toContain("deployy");
});

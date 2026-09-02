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
async function mint(perms: string[], label = perms.join("-")): Promise<string> {
  const { status, body } = await req("/api/tokens", ADMIN, {
    method: "POST",
    body: JSON.stringify({ label, perms }),
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

// ── attribution ──────────────────────────────────────────────────────────────
// specs/api-auth.md §7. The actor is `source:subject`, so a reader can never mistake an
// identity a proxy asserted for one genroc authenticated. These run here rather than in Go
// because the value has to survive the whole path — principal, handler, column, response.

test("attribution — a deployed version records who deployed it", async () => {
  const deploy = await mint(["deploy"], "release-bot");
  const name = `attrib_${crypto.randomUUID().slice(0, 8)}`;

  expect(
    (await req("/api/definitions", deploy, {
      method: "PUT",
      body: JSON.stringify({ name, tasks: [{ id: "a", switch: [{ goto: "end" }] }] }),
    })).status,
  ).toBe(200);

  const listed = await req("/api/definitions?limit=100", ADMIN);
  const row = (listed.body.items as { name: string; actor?: string }[]).find((d) => d.name === name);
  expect(
    row?.actor,
    "a version was deployed with no actor recorded — 'who deployed v7?' is what §7 exists to answer",
  ).toBe("token:release-bot");
});

test("attribution — the source is in the actor, so an asserted identity cannot pass as an authenticated one", async () => {
  const deploy = await mint(["deploy"], "ada@example.com");
  const name = `attrib_src_${crypto.randomUUID().slice(0, 8)}`;
  await req("/api/definitions", deploy, {
    method: "PUT",
    body: JSON.stringify({ name, tasks: [{ id: "a", switch: [{ goto: "end" }] }] }),
  });

  const listed = await req("/api/definitions?limit=100", ADMIN);
  const row = (listed.body.items as { name: string; actor?: string }[]).find((d) => d.name === name);
  // The subject alone would be indistinguishable from a proxy-asserted `ada@example.com`,
  // which is the whole reason the source is carried in the same string.
  expect(row?.actor).toBe("token:ada@example.com");
  expect(row?.actor?.startsWith("token:")).toBe(true);
});

test("attribution — an operator verb is recorded on the instance's trail, and the engine's own rows are not", async () => {
  const op = await mint(["deploy", "operate", "read"], "oncall-kim");
  const name = `attrib_pause_${crypto.randomUUID().slice(0, 8)}`;

  expect(
    (await req("/api/definitions", op, {
      method: "PUT",
      body: JSON.stringify({
        name,
        tasks: [{ id: "wait", action: { type: "delay", for: "1h" }, switch: [{ goto: "end" }] }],
      }),
    })).status,
  ).toBe(200);

  const started = await req("/api/instances", op, {
    method: "POST",
    body: JSON.stringify({ process: name }),
  });
  expect(started.status, JSON.stringify(started.body)).toBe(200);
  const id = started.body.id as string;

  expect((await req(`/api/instances/${id}/pause`, op, { method: "POST" })).status).toBeLessThan(300);

  const logs = await req(`/api/instances/${id}/logs?limit=100`, op);
  const rows = logs.body.items as { event: string; actor?: string }[];

  const paused = rows.find((l) => l.event.startsWith("inst_paus"));
  expect(paused, `no pause event in ${JSON.stringify(rows.map((l) => l.event))}`).toBeDefined();
  expect(
    paused!.actor,
    "a pause landed with no actor — an audit trail that cannot say who paused a run is the gap §7 names",
  ).toBe("token:oncall-kim");

  // The engine advances on its own behalf. Crediting the operator who started the run would
  // attribute rows nobody asked for, which is worse than leaving them blank.
  const engineRow = rows.find((l) => l.event === "work_started" || l.event === "task_completed");
  if (engineRow) {
    expect(
      engineRow.actor ?? "",
      `the engine's ${engineRow.event} claimed an actor; only operator-initiated events carry one`,
    ).toBe("");
  }
});

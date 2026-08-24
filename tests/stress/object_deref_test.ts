import { spawnSync } from "child_process";
import { createServer } from "http";
import type { AddressInfo } from "net";
import { afterAll, beforeAll, expect, test } from "vitest";
import { buildGenrocBinary, startGenroc, tmpPath, type GenrocProcess } from "../helpers/server.ts";

// Deterministic object-store GC test (SQLite, single server, no chaos).
//
// This test used to assert the OPPOSITE: that releasing a context slot deleted its object in
// that same write, so a replaced value never lingered. specs/object-store.md §Collection gave
// that up deliberately — reading now hands out references and fetching them is a second call,
// so deleting at release means a client 404s on a reference the server gave it moments earlier.
// A release leaves the object unclaimed; the sweep marks it and collects past the window.
//
// So what it covers now is the RELEASE half of the lifecycle: every released object stays
// carried (by its release mark) rather than vanishing, and every claim resolves. The store
// growing one object per round is expected here, not a leak — the window is the bound, and
// bounding it is what --object-grace is for.
//
// A single task loops with a REST action — so each round persists and reclaims — and recomputes
// a large output whose content changes every round (the input blob plus a monotonic counter from
// the mock).

const PORT = 8951;
const BLOB = "B".repeat(12 * 1024); // over the 8 KiB externalization threshold
const ROUNDS = 8;

let bin = "";
const dbPath = tmpPath("genroc_obj_deref", ".db");
let server: GenrocProcess | undefined;

const sleep = (ms: number) => new Promise((r) => setTimeout(r, ms));

// A mock whose /gen returns a monotonic counter and flips `done` after ROUNDS calls, so
// the loop terminates deterministically. Driving the loop from the action result keeps
// the test independent of self.previous resolution.
function startCountingMock(rounds: number) {
  let calls = 0;
  const server = createServer((req, res) => {
    req.on("data", () => {});
    req.on("end", () => {
      calls++;
      res.writeHead(200, { "Content-Type": "application/json" });
      res.end(JSON.stringify({ i: calls, done: calls >= rounds }));
    });
  });
  return {
    listen: () =>
      new Promise<number>((r) =>
        server.listen(0, () => r((server.address() as AddressInfo).port)),
      ),
    calls: () => calls,
    stop: () => new Promise<void>((r) => server.close(() => r())),
  };
}

async function waitDown(timeoutMs = 5000) {
  const deadline = Date.now() + timeoutMs;
  while (Date.now() < deadline) {
    try {
      const r = await fetch(`http://localhost:${PORT}/openapi.json`);
      await r.body?.cancel();
    } catch {
      return; // connection refused → the process is gone
    }
    await sleep(50);
  }
  throw new Error("server did not shut down in time");
}

beforeAll(async () => {
  bin = await buildGenrocBinary();
});

afterAll(() => {
  server?.stop();
});

test("a released context object is carried by its release mark, and every claim still resolves", async () => {
  const mock = startCountingMock(ROUNDS);
  const mockPort = await mock.listen();
  server = await startGenroc(bin, PORT, dbPath, undefined, 50 /* poll */, 8 /* max-concurrent */);
  const client = server.client;

  try {
    const name = `obj_deref_${Date.now()}`;
    const { error: defErr } = await client.PUT("/definitions", {
      body: {
        name,
        input_schema: {
          type: "object",
          properties: { blob: { type: "string" } },
          required: ["blob"],
        },
        tasks: [
          {
            id: "gen",
            action: {
              type: "fetch",
              url: `http://localhost:${mockPort}/gen`,
              responses: { 200: {
                type: "object",
                properties: { i: { type: "integer" }, done: { type: "boolean" } },
                required: ["i", "done"],
              } },
            },
            // A large output that differs every round (distinct hash), so each round
            // dereferences the previous round's object. Not logged ⇒ deleted at once.
            output: { blob: "${ input.blob }-${ self.result.i }" },
            switch: [
              { case: "self.result.done == true", goto: "end" },
              { goto: "$gen" },
            ],
          },
        ],
      } as never,
    });
    expect(defErr, `register failed: ${JSON.stringify(defErr)}`).toBeUndefined();

    const { data: started, error: startErr } = await client.POST("/instances", {
      body: { process: name, input: { blob: BLOB } } as never,
    });
    expect(startErr, `start failed: ${JSON.stringify(startErr)}`).toBeUndefined();
    const id = started!.id;

    const deadline = Date.now() + 15_000;
    let status = "";
    while (Date.now() < deadline) {
      const { data } = await client.GET("/instances/{id}", { params: { path: { id } } });
      status = data?.status ?? "";
      if (status === "completed" || status === "failed" || status === "cancelled") break;
      await sleep(50);
    }
    expect(status).toBe("completed");
    expect(mock.calls()).toBe(ROUNDS); // one action call per round

    // Quiesce, then stop the server so the DB file has no concurrent writer.
    await sleep(300);
    server.stop();
    server = undefined;
    await waitDown();

    // Read the store directly through the sqlite3 CLI (-json): there is no SQLite driver among
    // the test dependencies. Every object, and every claim on it — content is global now, so
    // this cannot be scoped to one instance the way the old per-instance table was.
    const sql = <T>(q: string): T[] => {
      const r = spawnSync("sqlite3", ["-json", dbPath, q], {
        encoding: "utf8",
        maxBuffer: 64 * 1024 * 1024,
      });
      expect(r.status, `sqlite3 failed: ${r.stderr}`).toBe(0);
      return (r.stdout.trim() ? JSON.parse(r.stdout.trim()) : []) as T[];
    };
    const objs = sql<{ hash: string; releasedAt: number | null }>(
      "SELECT hash, released_at AS releasedAt FROM objects",
    );
    const refs = sql<{ hash: string; ownerKind: string; ownerId: string }>(
      "SELECT hash, owner_kind AS ownerKind, owner_id AS ownerId FROM object_refs",
    );
    void id;

    // 1. Nothing is OVERDUE. An unclaimed object is not a leak: the sweep marks it when it
    //    notices, and collects it a window later. Unmarked simply means the janitor (once a
    //    minute) has not visited since the release — this run quiesces for 300ms, so most
    //    releases are still pending, which is the ±minute the mark design accepts.
    //
    //    What would be a leak is an object whose window has long since passed and which is still
    //    here, so that is what this asserts.
    const claimed = new Set(refs.map((r) => r.hash));
    const overdue = objs.filter(
      (o) => !claimed.has(o.hash) && o.releasedAt !== null && Date.now() - o.releasedAt > 10 * 60_000,
    );
    expect(
      overdue.length,
      `${overdue.length} object(s) unclaimed and marked long past their window — the collector is not collecting`,
    ).toBe(0);

    // 2. And nothing dangles: every claim resolves to content that is still there.
    const haveContent = new Set(objs.map((o) => o.hash));
    const dangling = refs.filter((r) => !haveContent.has(r.hash));
    expect(
      dangling.length,
      `${dangling.length} claim(s) point at content that is gone — the release path deleted something someone still held`,
    ).toBe(0);

    // 3. Only a live context slot claims anything: the latest output. Every earlier round's
    //    output has handed its instance claim back and survives unclaimed — content is retained
    //    by the store, not by the releaser, which is the behaviour this file exists to assert.
    const instanceClaims = refs.filter((r) => r.ownerKind === "instance");
    const released = objs.filter((o) => !claimed.has(o.hash));
    expect(
      released.length,
      `expected earlier rounds' outputs to survive their release after ${ROUNDS} rounds`,
    ).toBeGreaterThan(0);
    expect(
      instanceClaims.length,
      `expected few live instance claims (input + latest output), got ${instanceClaims.length}`,
    ).toBeLessThanOrEqual(3);
  } finally {
    await mock.stop();
  }
});

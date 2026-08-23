import { spawnSync } from "child_process";
import { createServer } from "http";
import type { AddressInfo } from "net";
import { afterAll, beforeAll, expect, test } from "vitest";
import { buildGenrocBinary, startGenroc, tmpPath, type GenrocProcess } from "../helpers/server.ts";

// Deterministic object-store GC test (SQLite, single server, no chaos).
//
// Covers the IMMEDIATE half of the object lifecycle: when a context value-slot stops
// referencing an object and no log needs it, the row is deleted in that same write —
// not left for the retention sweep (DeleteDereferencedObject, queries.sql). The sweep
// itself and crash-orphan handling are deliberately out of scope.
//
// A single task loops with a REST action — so each round persists and reclaims — and
// recomputes a large output whose content changes every round (the input blob plus a
// monotonic counter from the mock). Each new output externalizes into process_objects
// and dereferences the previous round's object; task outputs are never logged, so the
// dereferenced row must be gone at once. The proof: however many rounds run, the store
// holds only the input plus the latest output — it never grows one row per round.

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

test("a dereferenced, unlogged context object is deleted immediately (not left for the sweep)", async () => {
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

    // Read process_objects directly through the sqlite3 CLI (-json): there is no SQLite
    // driver among the test dependencies, and this needs no schema knowledge.
    const r = spawnSync(
      "sqlite3",
      [
        "-json",
        dbPath,
        `SELECT pinned, log_until AS logUntil FROM process_objects WHERE instance_id = '${id}'`,
      ],
      { encoding: "utf8", maxBuffer: 64 * 1024 * 1024 },
    );
    expect(r.status, `sqlite3 failed: ${r.stderr}`).toBe(0);
    const objs = (r.stdout.trim() ? JSON.parse(r.stdout.trim()) : []) as {
      pinned: number;
      logUntil: number | null;
    }[];

    // The invariant: every surviving object must be kept alive by a context pin OR a log
    // horizon (alive iff pinned OR log_until set). An unpinned, unlogged row is exactly a
    // dereferenced object that should have been deleted in its write but wasn't — it would
    // otherwise linger until the 60s sweep (which never runs in this short test).
    const leaked = objs.filter((o) => o.pinned === 0 && o.logUntil === null);
    expect(
      leaked.length,
      `${leaked.length} unpinned, unlogged object(s) survived after ${ROUNDS} rounds — a dereferenced object was not deleted immediately`,
    ).toBe(0);

    // And the store does not accumulate: only the input (its context object, plus possibly
    // a byte-identical log object) and the latest output remain — never one row per round.
    expect(
      objs.length,
      `expected <=3 objects (input + latest output), got ${objs.length} after ${ROUNDS} rounds`,
    ).toBeLessThanOrEqual(3);
  } finally {
    await mock.stop();
  }
});

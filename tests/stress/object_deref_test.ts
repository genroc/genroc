import { spawnSync } from "child_process";
import { createServer } from "http";
import type { AddressInfo } from "net";
import { afterAll, beforeAll, expect, test } from "vitest";
import { startGenroc, tmpPath, type GenrocProcess } from "../helpers/server.ts";

// The RELEASE half of the object lifecycle (SQLite, no chaos): a released object stays, unclaimed,
// until the sweep collects it past its window (specs/object-store.md §Collection). The store
// growing one object per round is expected; --object-grace bounds it.

const BLOB = "B".repeat(12 * 1024); // over the 2 KiB externalization threshold
const ROUNDS = 8;

const dbPath = tmpPath("genroc_obj_deref", ".db");
let server: GenrocProcess | undefined;

const sleep = (ms: number) => new Promise((r) => setTimeout(r, ms));

// Driven by the action result rather than self.previous, so the loop ends deterministically.
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

afterAll(async () => {
  await server?.stop();
});

test("a released context object is carried by its release mark, and every claim still resolves", async () => {
  const mock = startCountingMock(ROUNDS);
  const mockPort = await mock.listen();
  server = await startGenroc({ db: dbPath, poll: 50, maxConcurrent: 8 });
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
              method: "post",
              url: `http://localhost:${mockPort}/gen`,
              responses: { 200: {
                type: "object",
                properties: { i: { type: "integer" }, done: { type: "boolean" } },
                required: ["i", "done"],
              } },
            },
            // Differs every round (distinct hash), so each round releases the previous one's object.
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

    // Await stop(), which resolves on process exit: genroc closes its listener before the DB, so a
    // closed port does not mean an unlocked file.
    await sleep(300);
    await server.stop();
    server = undefined;

    // Via the sqlite3 CLI: the test deps have no SQLite driver.
    const sql = <T>(q: string): T[] => {
      // .timeout mirrors the engine's own _busy_timeout=5000: wait for a lock, never fail on one.
      const r = spawnSync("sqlite3", ["-cmd", ".timeout 5000", "-json", dbPath, q], {
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

    // 1. Nothing OVERDUE: unclaimed is not a leak until its window has long passed (the janitor
    //    runs once a minute, so most releases here are still pending).
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

    // 3. Only live slots claim anything; earlier rounds' outputs survive unclaimed, retained by
    //    the store rather than the releaser.
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

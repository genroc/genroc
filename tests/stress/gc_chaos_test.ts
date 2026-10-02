import { spawnSync } from "child_process";
import { createServer } from "http";
import type { AddressInfo } from "net";
import { afterAll, beforeAll, expect, test } from "vitest";
import { startGenroc, tmpPath, type GenrocProcess, freePort } from "../helpers/server.ts";
import { createClientTyped, listAllInstances } from "../helpers/client.ts";

// An object is legitimate iff some claim holds it (a live slot, a log, a grace window). Chaos, then
// the raw tables are read. SQLite only: the check reads the DB file, and one crashing process
// avoids multi-writer contention (multi_worker_test.ts is the Postgres fleet).

const ROOT_COUNT = 8;
const CHAOS_MS = 6_000;
const SETTLE_MS = 60_000;

// Over the 2 KiB externalization threshold, so every slot holding one lands in the object store.
const BLOB = "B".repeat(12 * 1024);
const PAD = "P".repeat(12 * 1024);

const sleep = (ms: number) => new Promise((r) => setTimeout(r, ms));
const pick = <T,>(xs: T[]): T => xs[Math.floor(Math.random() * xs.length)];
// Not `paused`/`pausing`: a pause is not an outcome.
const isTerminal = (s?: string) => s === "completed" || s === "failed";

// ── flaky mock backing the `gen` action ───────────────────────────────────────
// Chaos mode 500s and finishes at random; settle mode always succeeds with done, so loops end.
function startGenMock() {
  let calls = 0;
  let failRate = 0;
  let settle = false;
  const server = createServer((req, res) => {
    req.on("data", () => {});
    req.on("end", () => {
      calls++;
      req.socket.on("error", () => {});
      res.on("error", () => {});
      if (!settle && Math.random() < failRate) {
        res.writeHead(500);
        res.end("boom");
        return;
      }
      const done = settle ? true : Math.random() < 0.4;
      res.writeHead(200, { "Content-Type": "application/json" });
      res.end(JSON.stringify({ i: calls, done, pad: PAD }));
    });
  });
  server.on("clientError", () => {});
  return {
    listen: () =>
      new Promise<number>((r) =>
        server.listen(0, () => r((server.address() as AddressInfo).port)),
      ),
    setFailRate: (n: number) => {
      failRate = n;
    },
    enterSettle: () => {
      settle = true;
    },
    calls: () => calls,
    stop: () => new Promise<void>((r) => server.close(() => r())),
  };
}

// One port for the file's lifetime: the server is crashed and respawned in place, and `api`
// has to keep pointing at it across the respawn.
let port = 0;
const dbPath = tmpPath("genroc_gc_chaos", ".db");
let api: ReturnType<typeof createClientTyped>;
let server: GenrocProcess | undefined;
let mock: ReturnType<typeof startGenMock>;
let mockPort = 0;

// A short lease, so a post-crash reclaim lands within seconds; the env knobs are set only across
// the spawn so no other stress file inherits them.
async function spawn(): Promise<GenrocProcess> {
  const prev = {
    d: process.env.GENROC_LEASE_DURATION,
    r: process.env.GENROC_LEASE_RENEW_INTERVAL,
  };
  process.env.GENROC_LEASE_DURATION = "2s";
  process.env.GENROC_LEASE_RENEW_INTERVAL = "500ms";
  try {
    return await startGenroc({ port, db: dbPath, poll: 100, maxConcurrent: 32, immediateRetries: true });
  } finally {
    const restore = (k: "GENROC_LEASE_DURATION" | "GENROC_LEASE_RENEW_INTERVAL", v?: string) =>
      v === undefined ? delete process.env[k] : (process.env[k] = v);
    restore("GENROC_LEASE_DURATION", prev.d);
    restore("GENROC_LEASE_RENEW_INTERVAL", prev.r);
  }
}

beforeAll(async () => {
  port = await freePort();
  mock = startGenMock();
  mockPort = await mock.listen();
  server = await spawn();
  api = createClientTyped({ baseUrl: `${server.baseUrl}/api` });
}, 60_000);

afterAll(async () => {
  await server?.stop();
  await mock?.stop();
});

test(
  "every object stays claimed, and every claim resolves, through crash/error/pause/retry chaos",
  async () => {
    const suffix = crypto.randomUUID();
    const leaf = `gc_leaf_${suffix}`;
    const root = `gc_root_${suffix}`;
    const isMine = (p?: string) => p === leaf || p === root;

    // The LEAF externalizes three ways: input.blob (instance + log claim on one object), gen's
    // self.result (churned to log-only each loop), and scratch's output (unlogged, deleted each
    // loop). It returns the big blob in its OUTPUT.
    const { error: leafErr } = await api.PUT("/definitions", {
      body: {
        name: leaf,
        input_schema: {
          type: "object",
          properties: { blob: { type: "string" } },
          required: ["blob"],
        },
        tasks: [
          {
            id: "gen",
            action: {
              type: "fetch" as const,
              method: "post",
              url: `http://localhost:${mockPort}/gen`,
              responses: { 200: {
                type: "object",
                properties: {
                  i: { type: "number" },
                  done: { type: "boolean" },
                  pad: { type: "string" },
                },
                required: ["i", "done"],
              } },
            },
            on_error: [{ retry: 1 }],
            output: "$: self.result",
            switch: [
              { case: "outputs.gen.done == true", goto: "end" },
              { goto: "$scratch" },
            ],
          },
          {
            id: "scratch",
            output: "${ input.blob }-${ outputs.gen.i }",
            switch: [{ goto: "$gen" }],
          },
        ],
        output: { echo: "$: input.blob", rounds: "$: outputs.gen.i" },
        // eslint-disable-next-line @typescript-eslint/no-explicit-any
      } as any,
    });
    expect(leafErr).toBeUndefined();

    // The ROOT collects the leaf's big output, so the value round-trips parent → child → parent.
    const { error: rootErr } = await api.PUT("/definitions", {
      body: {
        name: root,
        input_schema: {
          type: "object",
          properties: { blob: { type: "string" } },
          required: ["blob"],
        },
        tasks: [
          {
            id: "call",
            action: {
              type: "child_map" as const,
              children: {
                out: {
                  name: leaf,
                  input: { blob: "$: input.blob" },
                  result_schema: {
                    type: "object",
                    properties: {
                      echo: { type: "string" },
                      rounds: { type: "number" },
                    },
                    required: ["echo"],
                  },
                },
              },
            },
            output: "$: self.result.out",
            switch: [{ goto: "end" }],
          },
        ],
        output: { echo: "$: outputs.call.echo" },
        // eslint-disable-next-line @typescript-eslint/no-explicit-any
      } as any,
    });
    expect(rootErr).toBeUndefined();

    // Start the roots, then turn the mock flaky for the chaos window.
    const rootIds: string[] = [];
    for (let i = 0; i < ROOT_COUNT; i++) {
      const { data, error } = await api.POST("/instances", {
        body: { process: root, input: { blob: BLOB } },
      });
      expect(error).toBeUndefined();
      rootIds.push(data!.id);
    }
    mock.setFailRate(0.35);

    // Chaos: randomly crash+restart the server, pause+resume a root, or force-retry one.
    let crashes = 0;
    const chaosDeadline = Date.now() + CHAOS_MS;
    const chaosMid = Date.now() + CHAOS_MS / 2;
    while (Date.now() < chaosDeadline) {
      const roll = Math.random();
      // At least one crash by the halfway mark, so every run has a real SIGKILL+restart.
      const forceCrash = crashes === 0 && Date.now() > chaosMid;
      try {
        if (roll < 0.12 || forceCrash) {
          server!.crash();
          crashes++;
          await sleep(200); // let the OS reap the pid / free the port
          server = await spawn();
        } else if (roll < 0.5) {
          // The beat between pause and resume races the worker; a resume lost to a crash is
          // picked up by the settle sweep.
          const id = pick(rootIds);
          await api.POST("/instances/{id}/pause", { params: { path: { id } } });
          await sleep(60);
          await api.POST("/instances/{id}/resume", { params: { path: { id } } });
        } else {
          // Retry takes only a `failed` root; an error otherwise is part of the contention.
          await api.POST("/instances/{id}/retry", {
            params: { path: { id: pick(rootIds) }, query: { force: true } },
          });
        }
      } catch {
        // API calls during the crash window fail — expected.
      }
      await sleep(180);
    }
    expect(crashes, "the server actually crashed during chaos").toBeGreaterThan(0);

    // Make sure a server is up (a crash may have landed on the last iteration).
    try {
      const r = await fetch(`${server!.baseUrl}/public/openapi.json`);
      await r.body?.cancel();
      if (!r.ok) throw new Error("not ok");
    } catch {
      server = await spawn();
    }

    // A root left paused (a crash swallowed its resume) would never advance, so unpause all first.
    for (const id of rootIds) {
      await api
        .POST("/instances/{id}/resume", { params: { path: { id } } })
        .catch(() => {});
    }

    // Settle: keep resuming paused roots and force-retrying failed ones until all are green.
    mock.enterSettle();
    let settled = false;
    const settleDeadline = Date.now() + SETTLE_MS;
    while (Date.now() < settleDeadline) {
      let insts;
      try {
        insts = (await listAllInstances(api)).filter((i) => isMine(i.process));
      } catch {
        await sleep(200);
        continue;
      }
      // Via roots only: the verbs are root-scoped and cascade. A `pausing` root is re-swept until
      // its in-flight task lands it in `paused` and the resume takes.
      const byId = new Map(insts.map((i) => [i.id, i]));
      for (const id of rootIds) {
        const r = byId.get(id);
        if (r && (r.status === "paused" || r.status === "pausing")) {
          await api
            .POST("/instances/{id}/resume", { params: { path: { id } } })
            .catch(() => {});
        } else if (r && r.status === "failed") {
          await api
            .POST("/instances/{id}/retry", {
              params: { path: { id }, query: { force: true } },
            })
            .catch(() => {});
        }
      }
      if (insts.length > 0 && insts.every((i) => i.status === "completed")) {
        settled = true;
        break;
      }
      await sleep(250);
    }
    expect(settled, "all instances reached completed after settling").toBe(true);

    // Await stop() before touching the file: it resolves on process exit, whereas the HTTP
    // listener closes before the DB does.
    await sleep(500);
    await server!.stop();
    server = undefined;

    // ── Verify the GC invariant against the raw tables ──────────────────────────
    // Via the sqlite3 CLI: the test deps have no SQLite driver.
    const sqlJson = <T,>(query: string): T[] => {
      // .timeout mirrors the engine's own _busy_timeout=5000: wait for a lock, never fail on one.
      const r = spawnSync("sqlite3", ["-cmd", ".timeout 5000", "-json", dbPath, query], {
        encoding: "utf8",
        maxBuffer: 256 * 1024 * 1024,
      });
      if (r.status !== 0) throw new Error(`sqlite3 failed: ${r.stderr}`);
      const out = (r.stdout ?? "").trim();
      return out ? (JSON.parse(out) as T[]) : [];
    };

    // One object per distinct content, one claim row per owner. specs/object-store.md.
    const objs = sqlJson<{ hash: string; releasedAt: number | null }>(
      "SELECT hash, released_at AS releasedAt FROM objects",
    );
    const refs = sqlJson<{
      hash: string;
      ownerKind: string;
      ownerId: string;
    }>(
      "SELECT hash, owner_kind AS ownerKind, owner_id AS ownerId FROM object_refs",
    );
    // Each owner declares its references in its `objects` column: read that, not the encoder's layout.
    const insts = sqlJson<{ id: string; objects: string }>(
      "SELECT id, objects FROM process_instances",
    );
    const logs = sqlJson<{ id: string; instanceId: string; objects: string }>(
      "SELECT id, instance_id AS instanceId, objects FROM process_logs",
    );

    const key = (ownerId: string, ref: string) => `${ownerId}|${ref}`;
    const declared = (ownerId: string, raw: string, into: Set<string>) => {
      if (!raw) return;
      let list: { ref?: string }[];
      try {
        list = JSON.parse(raw) as { ref?: string }[];
      } catch {
        return; // a malformed column surfaces in another assertion
      }
      for (const r of list) if (typeof r.ref === "string") into.add(key(ownerId, r.ref));
    };

    // Context references, by the instance that declares them.
    const contextRefs = new Set<string>();
    for (const i of insts) declared(i.id, i.objects, contextRefs);

    // Log references, by the ROW that declares them -- a log claim's owner is the row.
    const logRefs = new Set<string>();
    for (const l of logs) declared(l.id, l.objects, logRefs);

    // A claim is (kind, owner, hash), so the checks below can say WHO holds a row.
    const claims = new Set(refs.map((r) => `${r.ownerKind}|${r.ownerId}|${r.hash}`));
    const claimsByHash = new Map<string, number>();
    for (const r of refs) claimsByHash.set(r.hash, (claimsByHash.get(r.hash) ?? 0) + 1);

    // The chaos must have actually externalized objects, else the test proves nothing.
    expect(objs.length, "chaos produced externalized objects").toBeGreaterThan(0);
    expect(contextRefs.size, "live contexts reference objects").toBeGreaterThan(0);

    const byKind = (k: string) => refs.filter((r) => r.ownerKind === k).length;
    const sharedObjects = [...claimsByHash.values()].filter((n) => n > 1).length;
    console.log(
      `[gc_chaos] crashes=${crashes} instances=${insts.length} logs=${logs.length} ` +
        `objects=${objs.length} claims=${refs.length} ` +
        `(instance=${byKind("instance")}, log=${byKind("log")}, grace=${byKind("grace")}, ` +
        `shared=${sharedObjects}) mockCalls=${mock.calls()}`,
    );


    // 1. Every live context reference resolves to content claimed by THAT instance.
    for (const k of contextRefs) {
      const [instanceId, hash] = k.split("|");
      expect(objs.some((o) => o.hash === hash), `context ref ${k} has no content`).toBe(true);
      expect(
        claims.has(`instance|${instanceId}|${hash}`),
        `context ref ${k} is not claimed by its own instance`,
      ).toBe(true);
    }

    // 2. Every log reference resolves to content held by a log claim of that ROW.
    for (const k of logRefs) {
      const [logId, hash] = k.split("|");
      expect(objs.some((o) => o.hash === hash), `log ref ${k} has no content`).toBe(true);
      expect(claims.has(`log|${logId}|${hash}`), `log ref ${k} has no log claim`).toBe(true);
    }


    // 3. No OVERDUE content: a recent release is legitimately unclaimed until the sweep's window
    //    passes. A crash-orphaned LOG claim is likewise a pending release, so this checks claims,
    //    not surviving log rows.
    for (const o of objs) {
      const overdue = o.releasedAt !== null && Date.now() - o.releasedAt > 10 * 60_000;
      expect(
        (claimsByHash.get(o.hash) ?? 0) > 0 || !overdue,
        `object ${o.hash} is unclaimed and long past its window — the collector is not collecting`,
      ).toBe(true);
    }

    // 4. No dangling claims: a claim on content that is gone is the failure the store's
    //    ON CONFLICT DO UPDATE exists to prevent, and the one a crash could otherwise leave.
    const haveContent = new Set(objs.map((o) => o.hash));
    for (const r of refs) {
      expect(
        haveContent.has(r.hash),
        `${r.ownerKind} claim by ${r.ownerId} points at content that is gone (${r.hash})`,
      ).toBe(true);
    }

    // 5. An INSTANCE claim needs a live slot; log and grace claims outlive their slots by design.
    for (const r of refs) {
      if (r.ownerKind !== "instance") continue;
      expect(
        contextRefs.has(key(r.ownerId, r.hash)),
        `instance ${r.ownerId} claims ${r.hash} but no live context slot references it`,
      ).toBe(true);
    }
  },
  120_000,
);

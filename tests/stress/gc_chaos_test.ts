import { spawnSync } from "child_process";
import { createServer } from "http";
import type { AddressInfo } from "net";
import { afterAll, beforeAll, expect, test } from "vitest";
import {
  buildGenrocBinary,
  startGenroc,
  tmpPath,
  type GenrocProcess,
} from "../helpers/server.ts";
import { createClientTyped, listAllInstances } from "../helpers/client.ts";

// GC-under-chaos (SQLite, one server crashed/restarted at random). An object is legitimate iff
// some claim holds it -- a live context slot, a log, or a grace window; this
// hammers that bookkeeping -- big blobs round-tripping parent->child->parent, flaky 500s,
// random SIGKILLs, pauses/resumes/force-retries -- then reads the raw tables and asserts
// every row is reachable and every reference resolves. SQLite-only: the check reads the
// DB file, and one crashed process avoids multi-writer contention (multi_worker_test.ts
// is the Postgres fleet shape).

const ROOT_COUNT = 8;
const CHAOS_MS = 6_000;
const SETTLE_MS = 60_000;
const PORT = 8950;
const BASE_URL = `http://localhost:${PORT}`;

// Both comfortably over the 8 KiB externalization threshold so every slot that holds
// one lands in the object store.
const BLOB = "B".repeat(12 * 1024);
const PAD = "P".repeat(12 * 1024);

const sleep = (ms: number) => new Promise((r) => setTimeout(r, ms));
const pick = <T,>(xs: T[]): T => xs[Math.floor(Math.random() * xs.length)];
// `paused`/`pausing` are deliberately absent: a pause is not an outcome, it only
// says the tree is not being advanced, so it never counts as settled.
const isTerminal = (s?: string) => s === "completed" || s === "failed";

// ── flaky mock backing the `gen` action ───────────────────────────────────────
// Returns a large result (pad) plus a monotonic counter `i` and a `done` flag. In
// chaos mode it randomly 500s and randomly finishes; in settle mode it always
// succeeds and reports done, so every loop terminates and instances can be driven
// green.
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

let bin = "";
const dbPath = tmpPath("genroc_gc_chaos", ".db");
const api = createClientTyped({ baseUrl: BASE_URL });
let server: GenrocProcess | undefined;
let mock: ReturnType<typeof startGenMock>;
let mockPort = 0;

// Spawn a SQLite-backed server on the fixed port with a short lease so a reclaim
// after a crash happens within a couple of seconds. The lease is passed through the
// env knobs spawnProc already reads, set only across the spawn so no other stress
// file inherits them.
async function spawn(): Promise<GenrocProcess> {
  const prev = {
    d: process.env.GENROC_LEASE_DURATION,
    r: process.env.GENROC_LEASE_RENEW_INTERVAL,
  };
  process.env.GENROC_LEASE_DURATION = "2s";
  process.env.GENROC_LEASE_RENEW_INTERVAL = "500ms";
  try {
    return await startGenroc(
      bin,
      PORT,
      dbPath,
      undefined,
      100 /* poll */,
      32 /* max-concurrent */,
      true /* immediate retries */,
    );
  } finally {
    const restore = (k: "GENROC_LEASE_DURATION" | "GENROC_LEASE_RENEW_INTERVAL", v?: string) =>
      v === undefined ? delete process.env[k] : (process.env[k] = v);
    restore("GENROC_LEASE_DURATION", prev.d);
    restore("GENROC_LEASE_RENEW_INTERVAL", prev.r);
  }
}

beforeAll(async () => {
  bin = await buildGenrocBinary();
  mock = startGenMock();
  mockPort = await mock.listen();
  server = await spawn();
}, 60_000);

afterAll(async () => {
  server?.stop();
  await mock?.stop();
});

test(
  "every object stays claimed, and every claim resolves, through crash/error/pause/retry chaos",
  async () => {
    const suffix = crypto.randomUUID();
    const leaf = `gc_leaf_${suffix}`;
    const root = `gc_root_${suffix}`;
    const isMine = (p?: string) => p === leaf || p === root;

    // The LEAF is a looping worker that externalizes values three ways:
    //   • input.blob              — large input (instance claim + a log claim on inst_created's row → one shared object)
    //   • gen → self.result       — large action result (instance claim + per-row log claims → churned to log-only each loop)
    //   • scratch → blob + i      — large task output, NOT logged (pure context; deleted outright on each loop)
    // gen loops back through scratch until the mock reports done, then the leaf returns
    // the big blob in its OUTPUT.
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

    // The ROOT spawns the leaf with the big blob and collects the leaf's (big) output
    // back into its own context — so the value round-trips parent → child → parent and
    // lands in a parent-owned object, on top of the child-owned ones.
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
      // Random crashes, but guarantee at least one by the halfway mark so the run
      // always exercises a real SIGKILL+restart.
      const forceCrash = crashes === 0 && Date.now() > chaosMid;
      try {
        if (roll < 0.12 || forceCrash) {
          server!.crash();
          crashes++;
          await sleep(200); // let the OS reap the pid / free the port
          server = await spawn();
        } else if (roll < 0.5) {
          // Pause is not an outcome and never settles by itself, so every pause is
          // paired with a resume; the short beat in between is what races the worker
          // (pausing → paused → running while a task is mid-flight). A resume lost to
          // a crash landing in the gap is picked up by the settle sweep below.
          const id = pick(rootIds);
          await api.POST("/instances/{id}/pause", { params: { path: { id } } });
          await sleep(60);
          await api.POST("/instances/{id}/resume", { params: { path: { id } } });
        } else {
          // Retry only takes a `failed` root now; on anything else the server replies
          // with an error, which is fine — it is part of the contention.
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
      const r = await fetch(`${BASE_URL}/openapi.json`);
      await r.body?.cancel();
      if (!r.ok) throw new Error("not ok");
    } catch {
      server = await spawn();
    }

    // Resume sweep: a root the chaos left paused (or pausing, if a crash swallowed the
    // resume half of a pair) would never advance again, so nothing below could ever
    // settle. Unpause every root before the settle loop even starts.
    for (const id of rootIds) {
      await api
        .POST("/instances/{id}/resume", { params: { path: { id } } })
        .catch(() => {});
    }

    // Settle: mock always succeeds + reports done so every loop terminates; keep
    // resuming paused roots and force-retrying failed ones until the whole fleet is
    // terminal & green.
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
      // Recover via the roots only (pause/resume/retry is root-scoped and cascades to
      // the subtree); a failed leaf is brought back by retrying its root, a paused one
      // by resuming it. A `pausing` root is re-swept every iteration until the in-flight
      // task's write lands it in `paused` and the resume takes.
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

    // Quiesce, then wait for the server process to EXIT before touching the file. AWAITING
    // stop() is what makes that true -- it resolves on real process exit. Polling the HTTP port
    // until it refuses connections is weaker and was the bug here: the listener closes before
    // the DB does.
    await sleep(500);
    await server!.stop();
    server = undefined;

    // ── Verify the GC invariant against the raw tables ──────────────────────────
    // Read the DB via the sqlite3 CLI (-json): there is no SQLite driver among the test
    // dependencies. The selected columns are all small — an externalized
    // slot stores only its {refs} envelope, never the content — so the JSON stays tiny.
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

    // Content and claims are separate tables now: one object per distinct content, and a row
    // per owner holding it. specs/object-store.md.
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
    // Every owner lists what it references in its own `objects` column, so this reads the
    // declaration instead of reconstructing it. It used to parse input_data, output_data,
    // outputs_data.items and process_logs.data, which meant the check carried a second copy of
    // the encoder's layout and could drift from it silently.
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

    // A claim is (kind, owner, hash). The old shape could only say that SOMEONE pinned a row;
    // this can say who, so the checks below are stricter than the ones they replace.
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


    // 1. Every live context reference resolves to content, held by a claim belonging to THAT
    //    instance. The old shape could only check that the row was pinned by someone.
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


    // 3. No OVERDUE content. Unclaimed is not a leak: the sweep marks an object when it notices
    //    nothing holds it and collects a window later, and the janitor runs once a minute — so a
    //    recent release is legitimately unclaimed and unmarked. A leak is content whose window
    //    passed long ago and which is still here.
    //
    //    A crash-orphaned LOG claim (its row lost to a SIGKILL before the write landed) is
    //    likewise a pending release, not a leak: the sweep drops it once it sees the owner is
    //    gone, which is why this checks the claim rather than a surviving log row.
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

    // 5. No leaked claims: an INSTANCE claim must be backed by a live context slot. Log and
    //    grace claims are exempt by construction — a log claim outlives its slot on purpose, and
    //    a grace claim exists precisely because no slot references it any more.
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

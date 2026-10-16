import { afterAll, beforeAll, describe, expect, test } from "vitest";
import { createServer, type Server } from "node:http";
import { startGenroc, type GenrocProcess } from "../helpers/server.ts";

// Real workers against the drain (specs/pause-resume.md §2-§4): root → 3 mids → 3 leaves each, every
// leaf two calls to an endpoint that fails at random, while the test pauses, resumes, cancels and
// retries. The root reports its whole tree, so a root that reads paused, failed, cancelled or
// completed must have nothing busy below it in the SAME snapshot — and every tree must converge
// once the failures stop. SQLite: one server, so a page of the list is one SELECT.

const DSN = process.env.POSTGRES_DSN;
const ROOTS = 6; // 13 rows each: one page of the list, which is what makes a snapshot atomic
const CHAOS_MS = 4_000;
const SETTLE_MS = 60_000;
const BUSY = new Set(["running", "pausing", "failing", "cancelling"]);
const sleep = (ms: number) => new Promise((r) => setTimeout(r, ms));

type Row = { id: string; status: string; parent_id?: string };
type Client = GenrocProcess["client"];

// A 500 at failRate, after a random delay so calls overlap the verbs.
function chaosEndpoint() {
  const state = { failRate: 0.15 };
  const server: Server = createServer((req, res) => {
    req.resume();
    setTimeout(() => {
      res.writeHead(Math.random() < state.failRate ? 500 : 200, { "Content-Type": "application/json" });
      res.end('{"ok":true}');
    }, Math.random() * 40);
  });
  return new Promise<{ state: typeof state; port: number; stop: () => Promise<void> }>((resolve) =>
    server.listen(0, () =>
      resolve({
        state,
        port: (server.address() as { port: number }).port,
        stop: () => new Promise((r) => server.close(() => r())),
      }),
    ),
  );
}

async function defineTree(api: Client, port: number) {
  const uid = crypto.randomUUID().slice(0, 8);
  const call = (id: string) => ({
    id,
    action: { type: "fetch", method: "post", url: `http://localhost:${port}/c`, timeout: 5_000 },
    switch: [{ goto: id === "first" ? "next" : "end" }],
  });
  const defs = [
    { name: `chaos_leaf_${uid}`, input_schema: { type: "object", properties: { n: { type: "integer" } } }, tasks: [call("first"), call("second")] },
    {
      name: `chaos_mid_${uid}`,
      input_schema: { type: "object", properties: { items: { type: "array", items: { type: "object", properties: { n: { type: "integer" } } } } }, required: ["items"] },
      tasks: [{ id: "fan", action: { type: "child_list", name: `chaos_leaf_${uid}`, over: "$: input.items" }, switch: [{ goto: "end" }] }],
    },
    {
      name: `chaos_root_${uid}`,
      input_schema: { type: "object", properties: { groups: { type: "array", items: { type: "object", properties: { items: { type: "array", items: { type: "object", properties: { n: { type: "integer" } } } } }, required: ["items"] } } }, required: ["groups"] },
      tasks: [{ id: "fan", action: { type: "child_list", name: `chaos_mid_${uid}`, over: "$: input.groups" }, switch: [{ goto: "end" }] }],
    },
  ];
  const { error } = await api.PUT("/definitions", { body: { definitions: defs as never } });
  if (error) throw new Error(`define failed: ${JSON.stringify(error)}`);
  return `chaos_root_${uid}`;
}

// Every row created since `since`, with whether it came from a single page (one SELECT).
async function snapshot(api: Client, since: number): Promise<{ rows: Row[]; atomic: boolean }> {
  const rows: Row[] = [];
  let after: string | undefined;
  let pages = 0;
  for (;;) {
    const { data, error } = await api.GET("/instances", {
      params: { query: { children: true, created_after: since, limit: 100, after } as never },
    });
    if (error) throw new Error(`list failed: ${JSON.stringify(error)}`);
    rows.push(...((data?.items ?? []) as Row[]));
    pages++;
    after = data?.page.after || undefined;
    if (!after) return { rows, atomic: pages === 1 };
  }
}

// What a root's status promises about its tree, checked against one snapshot.
function violations(rows: Row[], roots: string[]): string[] {
  const byId = new Map(rows.map((r) => [r.id, r]));
  const rootOf = (r: Row): string => {
    let cur = r;
    while (cur.parent_id) cur = byId.get(cur.parent_id) ?? { id: "?", status: "?" };
    return cur.id;
  };
  const out: string[] = [];
  for (const r of rows) {
    const root = byId.get(rootOf(r));
    if (!root || !roots.includes(root.id) || r.id === root.id) continue;
    // A draining root may still have work in flight; a stopped or settled one may not.
    if (!BUSY.has(root.status) && BUSY.has(r.status)) {
      out.push(`${r.id} is ${r.status} under a ${root.status} root ${root.id}`);
    }
    if (root.status === "completed" && r.status !== "completed") {
      out.push(`${r.id} is ${r.status} under a completed root`);
    }
    if (r.status === "failed" && root.status !== "failed" && root.status !== "failing" && root.status !== "cancelled" && root.status !== "cancelling") {
      out.push(`${r.id} failed but its root reads ${root.status}`);
    }
  }
  return out;
}

const backends = [
  { name: "sqlite", workers: 1, pg: undefined as string | undefined },
  ...(DSN ? [{ name: "postgres", workers: 3, pg: DSN }] : []),
];

describe.each(backends)("pause, cancel and failures racing real workers — $name", ({ workers: n, pg }) => {
  let workers: GenrocProcess[] = [];
  let endpoint: Awaited<ReturnType<typeof chaosEndpoint>>;

  beforeAll(async () => {
    endpoint = await chaosEndpoint();
    if (pg) process.env.GENROC_PG_MAX_OPEN_CONNS = "8";
    // Sequential: the first process runs migrations before any other opens the DB.
    for (let i = 0; i < n; i++) {
      workers.push(await startGenroc({ pg, poll: 5, maxConcurrent: 8, immediateRetries: true }));
    }
  }, 60_000);

  afterAll(async () => {
    await Promise.all(workers.map((w) => w.stop()));
    workers = [];
    await endpoint?.stop();
  });

  test(
    "every root agrees with its tree throughout, and every tree converges once failures stop",
    async () => {
      const api = workers[0].client;
      const since = Date.now() - 1;
      const process = await defineTree(api, endpoint.port);
      const groups = [0, 1, 2].map((g) => ({ items: [0, 1, 2].map((i) => ({ n: g * 3 + i })) }));
      const roots: string[] = [];
      for (let i = 0; i < ROOTS; i++) {
        const { data, error } = await api.POST("/instances", { body: { process, input: { groups } } as never });
        if (error) throw new Error(`start failed: ${JSON.stringify(error)}`);
        roots.push(data!.id);
      }

      const bad: string[] = [];
      let checked = 0;
      const verbs = { pause: 0, resume: 0, cancel: 0, retry: 0 };
      const deadline = Date.now() + CHAOS_MS;
      while (Date.now() < deadline) {
        const snap = await snapshot(api, since);
        if (snap.atomic) {
          checked++;
          bad.push(...violations(snap.rows, roots));
        }
        const id = roots[Math.floor(Math.random() * roots.length)];
        const status = snap.rows.find((r) => r.id === id)?.status;
        const roll = Math.random();
        // Cancel rarely: it is final, and the run needs live trees to race.
        const verb = status === "failed" && roll < 0.5 ? "retry" : roll < 0.45 ? "pause" : roll < 0.9 ? "resume" : roll < 0.93 ? "cancel" : undefined;
        if (verb) {
          await workers[Math.floor(Math.random() * workers.length)].client.POST(
            `/instances/{id}/${verb}` as "/instances/{id}/pause",
            { params: { path: { id } } },
          );
          verbs[verb]++;
        }
        await sleep(15);
      }
      expect(bad, bad.slice(0, 10).join("\n")).toEqual([]);
      expect(checked, "no atomic snapshot was taken, so nothing was checked").toBeGreaterThan(10);

      // Stop the failures and recover every tree: resume what is paused, retry what failed.
      endpoint.state.failRate = 0;
      const settleBy = Date.now() + SETTLE_MS;
      let snap = await snapshot(api, since);
      for (;;) {
        const live = roots.filter((id) => {
          const s = snap.rows.find((r) => r.id === id)?.status;
          return s !== "completed" && s !== "cancelled";
        });
        if (live.length === 0) break;
        if (Date.now() > settleBy) throw new Error(`never converged: ${live.map((id) => `${id}=${snap.rows.find((r) => r.id === id)?.status}`).join(", ")}`);
        for (const id of live) {
          const s = snap.rows.find((r) => r.id === id)?.status;
          if (s === "paused") await api.POST("/instances/{id}/resume", { params: { path: { id } } });
          if (s === "failed") await api.POST("/instances/{id}/retry", { params: { path: { id } } });
        }
        await sleep(50);
        snap = await snapshot(api, since);
      }
      const final = violations(snap.rows, roots);
      expect(final, final.slice(0, 10).join("\n")).toEqual([]);
      const leftBusy = snap.rows.filter((r) => BUSY.has(r.status));
      expect(leftBusy.map((r) => `${r.id}=${r.status}`), "a row is still busy in a settled run").toEqual([]);
      console.log(`${checked} snapshots checked; verbs ${JSON.stringify(verbs)}`);
    },
    120_000,
  );
});

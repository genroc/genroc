import { spawnSync } from "child_process";
import { createServer } from "http";
import type { AddressInfo } from "net";
import { afterAll, beforeAll, expect, test } from "vitest";
import { startGenroc, type GenrocProcess } from "../helpers/server.ts";
import { listAllInstances } from "../helpers/client.ts";

// Audit logs are best-effort only across a crash. This server is never killed, so every row it
// minted must land: log seq counts 1, 2, 3… per server, and a hole is a lost row. gc_chaos_test.ts
// is the crashing counterpart, where loss is allowed.

const ROOT_COUNT = 12;
const CHAOS_MS = 4_000;
const SETTLE_MS = 60_000;

const sleep = (ms: number) => new Promise((r) => setTimeout(r, ms));
const pick = <T,>(xs: T[]): T => xs[Math.floor(Math.random() * xs.length)];

// Small bodies on purpose: a payload over 2 KiB is written synchronously, skipping the buffer
// this test is about.
function startFlakyMock() {
  let failRate = 0.3;
  const server = createServer((req, res) => {
    req.on("data", () => {});
    req.on("end", () => {
      res.on("error", () => {});
      if (Math.random() < failRate) {
        res.writeHead(500);
        res.end("boom");
        return;
      }
      res.writeHead(200, { "Content-Type": "application/json" });
      res.end(JSON.stringify({ done: failRate === 0 || Math.random() < 0.3 }));
    });
  });
  return {
    listen: () =>
      new Promise<number>((r) => server.listen(0, () => r((server.address() as AddressInfo).port))),
    settle: () => {
      failRate = 0;
    },
    stop: () => new Promise<void>((r) => server.close(() => r())),
  };
}

let server: GenrocProcess | undefined;
let mock: ReturnType<typeof startFlakyMock>;
let mockPort = 0;

beforeAll(async () => {
  mock = startFlakyMock();
  mockPort = await mock.listen();
  server = await startGenroc({ poll: 20, maxConcurrent: 32, immediateRetries: true });
}, 60_000);

afterAll(async () => {
  await server?.stop();
  await mock?.stop();
});

test(
  "a server that is never killed loses no audit log row through error/pause/retry chaos",
  async () => {
    const api = server!.client;
    const suffix = crypto.randomUUID();
    const leaf = `log_leaf_${suffix}`;
    const root = `log_root_${suffix}`;

    const { error: leafErr } = await api.PUT("/definitions", {
      body: {
        name: leaf,
        tasks: [
          {
            id: "poll",
            action: {
              type: "fetch" as const,
              method: "post",
              url: `http://localhost:${mockPort}/poll`,
              responses: {
                200: { type: "object", properties: { done: { type: "boolean" } }, required: ["done"] },
              },
            },
            on_error: [{ retry: 1 }],
            output: "$: self.result",
            switch: [{ case: "outputs.poll.done == true", goto: "end" }, { goto: "$poll" }],
          },
        ],
        // eslint-disable-next-line @typescript-eslint/no-explicit-any
      } as any,
    });
    expect(leafErr).toBeUndefined();

    const { error: rootErr } = await api.PUT("/definitions", {
      body: {
        name: root,
        tasks: [
          {
            id: "fan",
            action: {
              type: "child_map" as const,
              children: { a: { name: leaf, input: {} }, b: { name: leaf, input: {} } },
            },
            switch: [{ goto: "end" }],
          },
        ],
        // eslint-disable-next-line @typescript-eslint/no-explicit-any
      } as any,
    });
    expect(rootErr).toBeUndefined();

    const rootIds: string[] = [];
    for (let i = 0; i < ROOT_COUNT; i++) {
      const { data, error } = await api.POST("/instances", { body: { process: root, input: {} } });
      expect(error).toBeUndefined();
      rootIds.push(data!.id);
    }

    // Errors here (pausing a finished root, retrying a running one) are contention; ignored.
    const deadline = Date.now() + CHAOS_MS;
    while (Date.now() < deadline) {
      const id = pick(rootIds);
      if (Math.random() < 0.6) {
        await api.POST("/instances/{id}/pause", { params: { path: { id } } }).catch(() => {});
        await sleep(30);
        await api.POST("/instances/{id}/resume", { params: { path: { id } } }).catch(() => {});
      } else {
        await api
          .POST("/instances/{id}/retry", { params: { path: { id }, query: { force: true } } })
          .catch(() => {});
      }
      await sleep(50);
    }

    mock.settle();
    let settled = false;
    const settleDeadline = Date.now() + SETTLE_MS;
    while (Date.now() < settleDeadline) {
      const insts = (await listAllInstances(api)).filter((i) => i.process === leaf || i.process === root);
      const byId = new Map(insts.map((i) => [i.id, i]));
      for (const id of rootIds) {
        const s = byId.get(id)?.status;
        if (s === "paused" || s === "pausing") {
          await api.POST("/instances/{id}/resume", { params: { path: { id } } }).catch(() => {});
        } else if (s === "failed") {
          await api
            .POST("/instances/{id}/retry", { params: { path: { id }, query: { force: true } } })
            .catch(() => {});
        }
      }
      if (insts.length > 0 && insts.every((i) => i.status === "completed")) {
        settled = true;
        break;
      }
      await sleep(200);
    }
    expect(settled, "all instances reached completed after settling").toBe(true);

    // A graceful stop flushes the buffer; the file is read only once the process has exited.
    const dbPath = server!.dbPath;
    await server!.stop();
    server = undefined;

    const r = spawnSync("sqlite3", ["-cmd", ".timeout 5000", "-json", dbPath,
      "SELECT seq, objects FROM process_logs"], { encoding: "utf8", maxBuffer: 64 * 1024 * 1024 });
    if (r.status !== 0) throw new Error(`sqlite3 failed: ${r.stderr}`);
    const rows = JSON.parse(r.stdout.trim() || "[]") as { seq: number; objects: string }[];

    const seqs = new Set(rows.map((x) => x.seq));
    const lost: number[] = [];
    for (let seq = 1; seq <= Math.max(0, ...seqs); seq++) if (!seqs.has(seq)) lost.push(seq);
    const buffered = rows.filter((x) => !x.objects).length;
    console.log(`[log_loss] rows=${rows.length} buffered=${buffered} lost=${lost.length}`);

    expect(buffered, "most rows went through the buffer, or the test proves nothing about it")
      .toBeGreaterThan(rows.length / 2);
    expect(lost, "log rows (by seq) missing from a server that was never killed").toEqual([]);
  },
  120_000,
);

/** A failure pausing a sibling that is parked on an external task, with a worker holding the claim:
 *  root → {ext: external wait, bad: a delay, then a call that fails}. The pause keeps the wait and
 *  the claim, delivery is not advancement, and the retry consumes the answer that arrived meanwhile. */
import { createServer } from "node:http";
import { expect, test, beforeAll, afterAll } from "vitest";
import { useTickEnv } from "./helpers.ts";

const ctx = useTickEnv();

// Answers 500 until `healthy` is set, so a retry can succeed where the first attempt failed; /ok
// always succeeds, for a fixed version to call.
const endpoint = { healthy: false, port: 0, stop: async () => {} };

beforeAll(async () => {
  const server = createServer((req, res) => {
    req.resume();
    res.writeHead(endpoint.healthy || req.url === "/ok" ? 200 : 500, { "Content-Type": "application/json" });
    res.end('{"ok":true}');
  });
  await new Promise<void>((r) => server.listen(0, () => r()));
  endpoint.port = (server.address() as { port: number }).port;
  endpoint.stop = () => new Promise((r) => server.close(() => r()));
});

afterAll(() => endpoint.stop());

async function tree() {
  const uid = crypto.randomUUID().slice(0, 8);
  const ext = `fx_ext_${uid}`;
  await defineExt(ext, "$: outputs.approval");
  const bad = `fx_bad_${uid}`;
  await defineBad(bad, "/x");
  const root = `fx_root_${uid}`;
  const defineRoot = () =>
    ctx.env.define(root, [
      {
        id: "run",
        action: { type: "child_map" as const, children: { ext: { name: ext }, bad: { name: bad } } },
        switch: [{ goto: "end" }],
      },
    ]);
  await defineRoot();

  const id = await ctx.env.start(root);
  await ctx.env.tick(); // root spawns both
  const kids = await ctx.env.childrenOf(id, "run");
  await ctx.env.tick(); // one sibling: ext arms, or bad parks on its delay
  await ctx.env.tick(); // the other
  expect(await ctx.env.statuses({ ext: kids.ext, bad: kids.bad })).toEqual({
    ext: "running external",
    bad: "running",
  });
  return { root: id, ext, bad, ext_id: kids.ext, bad_id: kids.bad, defineRoot };
}

function defineExt(name: string, output: unknown) {
  return ctx.env.define(name, [
    {
      id: "approval",
      action: {
        type: "external",
        result_schema: { type: "object", properties: { approved: { type: "boolean" } }, required: ["approved"] },
      },
      output: "$: self.result",
      switch: "end",
    },
  ], { output });
}

// The delay lets ext arm first whichever sibling a tick claims first.
function defineBad(name: string, path: string) {
  return ctx.env.define(name, [
    { id: "wait", action: { type: "delay", for: "1s" }, switch: "next" },
    {
      id: "call",
      action: { type: "fetch" as const, method: "post", url: `http://localhost:${endpoint.port}${path}`, timeout: 5_000 },
      switch: [{ goto: "end" }],
    },
  ]);
}

// Past bad's delay, then until it settles: the delay's wake and the failing call are two claims.
async function failBad() {
  await ctx.env.client.POST("/tick", { body: { advance_ms: 2_000 } });
  await ctx.env.tickUntilIdle();
}

test("a failure pauses a claimed external wait, which still takes its answer; retry consumes it", async () => {
  endpoint.healthy = false;
  const t = await tree();

  const { data: claim } = await ctx.env.client.POST("/external-tasks/claim", {
    body: { worker_id: "w1", process: t.ext, task: "approval", limit: 1 },
  });
  // eslint-disable-next-line @typescript-eslint/no-explicit-any
  const [job] = ((claim as any)?.items ?? []) as { token: string }[];
  expect(job, "the parked wait must be offered before the failure").toBeDefined();

  // bad's delay elapses, then its call fails. ext is parked, not held by an engine worker, so it
  // pauses outright and the root has nothing busy below it.
  await failBad();
  expect(await ctx.env.statuses({ root: t.root, ext: t.ext_id, bad: t.bad_id })).toEqual({
    root: "failed",
    ext: "paused external",
    bad: "failed",
  });

  // The worker's heartbeat is not told to stop: a pause is not a cancel, and its work may resume.
  const { data: renew } = await ctx.env.client.POST("/external-tasks/renew", {
    body: { worker_id: "w1", tokens: [job.token], lease_ms: 30_000 },
  });
  // eslint-disable-next-line @typescript-eslint/no-explicit-any
  expect((renew as any)?.cancelled ?? []).toEqual([]);

  // Delivery is not advancement: the answer is taken and waits, unclaimable, for a retry.
  const { error: resolveErr } = await ctx.env.client.POST("/external-tasks/resolve", {
    body: { token: job.token, result: { approved: true } } as never,
  });
  expect(resolveErr, `a paused wait must take its answer: ${JSON.stringify(resolveErr)}`).toBeUndefined();
  expect(await ctx.env.status(t.ext_id)).toMatch(/^paused/);
  expect(await ctx.env.tick()).toBe(0);

  // Retry revives bad and resumes ext; ext consumes the buffered answer, bad now succeeds, and the
  // root collects both.
  endpoint.healthy = true;
  await ctx.env.retry(t.root);
  await ctx.env.tickUntilIdle();
  expect(await ctx.env.statuses({ root: t.root, ext: t.ext_id, bad: t.bad_id })).toEqual({
    root: "completed",
    ext: "completed",
    bad: "completed",
  });
  const { data: detail } = await ctx.env.client.GET("/instances/{id}/detail", { params: { path: { id: t.ext_id } } });
  expect(detail?.output, "ext must finish on the answer delivered while it was paused").toEqual({ approved: true });
});

test("a cancel leaves a failed tree's paused wait open for its answer", async () => {
  endpoint.healthy = false;
  const t = await tree();
  const { data: claim } = await ctx.env.client.POST("/external-tasks/claim", {
    body: { worker_id: "w2", process: t.ext, task: "approval", limit: 1 },
  });
  // eslint-disable-next-line @typescript-eslint/no-explicit-any
  const [job] = ((claim as any)?.items ?? []) as { token: string }[];

  await failBad();
  expect(await ctx.env.status(t.root)).toBe("failed");

  // A failed tree is concluded, so a cancel leaves it — and the paused wait — for a retry.
  expect(await ctx.env.cancel(t.root)).toBe("unchanged");
  const { error } = await ctx.env.client.POST("/external-tasks/resolve", {
    body: { token: job.token, result: { approved: true } } as never,
  });
  expect(error, "the wait is paused, not cancelled, so it still takes its answer").toBeUndefined();
});

// The repair an operator reaches for: publish a fixed child, re-apply the root to pin it, upgrade
// the failed tree, retry. The failed child moves to the fix and the paused wait moves with the
// tree, so the retry runs the fixed version and resumes the wait on its new one. The endpoint
// stays broken throughout: only the fix can succeed.
test("fix, upgrade, retry: the failed child runs the fixed version and the paused wait resumes", async () => {
  endpoint.healthy = false;
  const t = await tree();
  await failBad();
  expect(await ctx.env.statuses({ root: t.root, ext: t.ext_id, bad: t.bad_id })).toEqual({
    root: "failed",
    ext: "paused external",
    bad: "failed",
  });

  await defineBad(t.bad, "/ok");
  await defineExt(t.ext, { approved: "$: outputs.approval.approved", version: 2 });
  await t.defineRoot(); // pins both v2s: a parent keeps child versions baked in until re-applied
  const { data: up, error: upErr } = await ctx.env.client.POST("/instances/{id}/upgrade", {
    params: { path: { id: t.root } },
    body: { to_version: 2 } as never,
  });
  expect(upErr, JSON.stringify(upErr)).toBeUndefined();
  expect(up!.upgraded, JSON.stringify(up)).toBe(true);
  const moved = (id: string) => up!.moves?.find((m) => m.id === id);
  expect(moved(t.bad_id), JSON.stringify(up)).toMatchObject({ from_version: 1, to_version: 2, status: "failed" });
  expect(moved(t.ext_id), JSON.stringify(up)).toMatchObject({ from_version: 1, to_version: 2, status: "paused" });
  expect(await ctx.env.status(t.ext_id), "the upgrade must not disturb the paused wait").toBe("paused external");

  await ctx.env.retry(t.root);
  await ctx.env.tickUntilIdle();
  expect(await ctx.env.statuses({ root: t.root, ext: t.ext_id, bad: t.bad_id })).toEqual({
    root: "running children",
    ext: "running external",
    bad: "completed", // only v2's /ok can succeed
  });

  const { data: parked } = await ctx.env.client.GET("/instances/{id}/detail", { params: { path: { id: t.ext_id } } });
  const { error } = await ctx.env.client.POST("/external-tasks/resolve", {
    body: { token: `${t.ext_id}.${parked!.task_epoch}`, result: { approved: true } } as never,
  });
  expect(error, JSON.stringify(error)).toBeUndefined();
  await ctx.env.tickUntilIdle();
  expect(await ctx.env.status(t.root)).toBe("completed");
  const { data: done } = await ctx.env.client.GET("/instances/{id}/detail", { params: { path: { id: t.ext_id } } });
  expect(done?.output, "ext must have finished on the version it was moved to").toEqual({ approved: true, version: 2 });
});

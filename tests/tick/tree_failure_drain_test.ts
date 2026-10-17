/** A failure while a sibling's task is in flight: root → {a: fails at once, b: held mid-call}.
 *  Two instances per tick, so one tick runs both and a fails while b's worker still holds it. The
 *  failure pauses the rest of the tree, a held row only drains, and the root reports the whole tree:
 *  it reaches 'failed' only once nothing below it is busy. */
import { expect, test, beforeAll, afterAll } from "vitest";
import { startMockService } from "../helpers/client.ts";
import { useTickEnv } from "./helpers.ts";

const ctx = useTickEnv({ maxConcurrent: 2 });

let failPort: number;
let stopFail: () => Promise<void>;

beforeAll(async () => {
  const fail = await startMockService(0, { statusCode: 500 });
  failPort = fail.port;
  stopFail = fail.stop;
}, 60_000);

afterAll(() => stopFail?.());

// b's first task hangs until released; `after` is the task it would run next.
async function heldTree(opts: { bHasNextTask: boolean }) {
  const uid = crypto.randomUUID().slice(0, 8);
  const held = await startMockService(0, { response: { ok: true }, firstRequestDelayMs: Infinity });
  const next = await startMockService(0, { response: { ok: true } });
  const fetchTo = (id: string, port: number, then: string) => ({
    id,
    action: { type: "fetch" as const, method: "post", url: `http://localhost:${port}/action`, timeout: 30_000 },
    switch: [{ goto: then }],
  });

  await ctx.env.define(`a_${uid}`, [fetchTo("work", failPort, "end")]);
  await ctx.env.define(
    `b_${uid}`,
    opts.bHasNextTask
      ? [fetchTo("held", held.port, "next"), fetchTo("after", next.port, "end")]
      : [fetchTo("held", held.port, "end")],
  );
  await ctx.env.define(`root_${uid}`, [
    {
      id: "run",
      action: { type: "child_map" as const, children: { a: { name: `a_${uid}` }, b: { name: `b_${uid}` } } },
      switch: [{ goto: "end" }],
    },
  ]);

  const root = await ctx.env.start(`root_${uid}`);
  await ctx.env.tick(); // root spawns a and b
  const { a, b } = await ctx.env.childrenOf(root, "run");

  // One tick claims both: a 500s straight away while b's request is held open.
  const inFlight = ctx.env.tick();
  await held.firstRequestReceived;
  await waitFor(async () => (await ctx.env.status(a)) === "failed");

  const stop = async () => {
    await held.stop();
    await next.stop();
  };
  return { root, a, b, held, next, inFlight, stop };
}

async function waitFor(cond: () => Promise<boolean>, timeoutMs = 5_000) {
  const deadline = Date.now() + timeoutMs;
  while (!(await cond())) {
    if (Date.now() > deadline) throw new Error("waitFor: condition not met");
    await new Promise((r) => setTimeout(r, 20));
  }
}

test("the root fails only once the in-flight sibling lands, and the sibling's next task never runs", async () => {
  const t = await heldTree({ bHasNextTask: true });
  try {
    // b is held, so the failure can only ask it to stop, and the root drains above it.
    expect(await ctx.env.statuses({ root: t.root, a: t.a, b: t.b })).toEqual({
      root: "failing children",
      a: "failed",
      b: "pausing",
    });
    const { error } = await ctx.env.client.POST("/instances/{id}/retry", { params: { path: { id: t.root } } });
    expect(JSON.stringify(error), "a draining root is a transition, so a retry must wait for it").toContain(
      "wait until it is failed",
    );

    // b's task finishes; its own write keeps the pause, so the root is still draining.
    t.held.release();
    await t.inFlight;
    expect(await ctx.env.statuses({ root: t.root, b: t.b })).toEqual({ root: "failing children", b: "pausing" });

    // The next claim settles b, and that settle climbs: the root fails in the same write.
    expect(await ctx.env.tick()).toBe(1);
    expect(await ctx.env.statuses({ root: t.root, a: t.a, b: t.b })).toEqual({
      root: "failed",
      a: "failed",
      b: "paused",
    });
    expect(t.next.requestCount(), "a doomed sibling must not start its next task").toBe(0);
    expect(await ctx.env.tick()).toBe(0);
  } finally {
    t.held.release();
    await t.inFlight;
    await ctx.env.tickUntilIdle();
    await t.stop();
  }
});

test("a sibling whose in-flight task concludes it settles the failure itself", async () => {
  const t = await heldTree({ bHasNextTask: false });
  try {
    expect(await ctx.env.status(t.root)).toBe("failing children");

    // b's only task completes: a real outcome wins over the pause, and its terminal write is the
    // settle, so no extra claim is needed.
    t.held.release();
    await t.inFlight;
    expect(await ctx.env.statuses({ root: t.root, a: t.a, b: t.b })).toEqual({
      root: "failed",
      a: "failed",
      b: "completed",
    });
  } finally {
    t.held.release();
    await t.inFlight;
    await ctx.env.tickUntilIdle();
    await t.stop();
  }
});

test("a draining failure refuses a cancel until it settles", async () => {
  const t = await heldTree({ bHasNextTask: true });
  try {
    // 'failing' is a transition: a cancel landing mid-way would race the settle that ends it. It
    // would add nothing anyway — nothing new starts in a failing tree.
    const { error } = await ctx.env.client.POST("/instances/{id}/cancel", { params: { path: { id: t.root } } });
    expect(JSON.stringify(error)).toContain("wait until it is failed");
    expect(await ctx.env.statuses({ root: t.root, a: t.a, b: t.b })).toEqual({
      root: "failing children",
      a: "failed",
      b: "pausing",
    });

    t.held.release();
    await t.inFlight;
    expect(await ctx.env.tick()).toBe(1); // b's settle, which climbs to the root
    expect(await ctx.env.statuses({ root: t.root, a: t.a, b: t.b })).toEqual({
      root: "failed",
      a: "failed",
      b: "paused",
    });
    expect(t.next.requestCount(), "a doomed sibling must not start its next task").toBe(0);

    // Settled, and concluded: a cancel now has nothing to stop, and the tree waits for a retry.
    expect(await ctx.env.cancel(t.root)).toBe("unchanged");
    expect(await ctx.env.status(t.root)).toBe("failed");
  } finally {
    t.held.release();
    await t.inFlight;
    await ctx.env.tickUntilIdle();
    await t.stop();
  }
});

// The settle-up writes the failure's landing on each ancestor, as info: it is the moment the tree
// became retryable, and the only line that says so.
test("the root's settle to failed is on its trail", async () => {
  const t = await heldTree({ bHasNextTask: false });
  try {
    t.held.release();
    await t.inFlight;
    expect(await ctx.env.status(t.root)).toBe("failed");
    let hit: { level?: string } | undefined;
    for (let i = 0; i < 40 && !hit; i++) {
      const { data } = await ctx.env.client.GET("/instances/{id}/logs", {
        params: { path: { id: t.root }, query: { limit: 100, flat: true } },
      });
      hit = (data?.items ?? []).find((l) => l.event === "inst_settled");
      if (!hit) await new Promise((r) => setTimeout(r, 50));
    }
    expect(hit, "inst_settled on the root").toBeDefined();
    expect(hit!.level).toBe("info");
  } finally {
    t.held.release();
    await t.inFlight;
    await ctx.env.tickUntilIdle();
    await t.stop();
  }
});

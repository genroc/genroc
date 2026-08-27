import { expect, test, beforeAll } from "vitest";
import { join } from "path";
import { tmpdir } from "os";
import { buildGenrocBinary, startGenroc, type GenrocProcess } from "../helpers/server.ts";
import { client, startMockService, waitForInstance, tick, childrenOfTask, listAllInstances } from "../helpers/client.ts";

const TICK_PORT = 20017;
// Its own constant, not TICK_PORT + n: the offsets landed on 20018 and 20019, which are
// tick/logs_test.ts and tick/delay_test.ts. Both run *manual-tick* servers, and this file's
// guard test asserts the opposite mode — so when the lifetimes overlapped, startGenroc's
// readiness probe was answered by the neighbour's server before it noticed its own process
// had failed to bind, and /tick was accepted instead of refused.
const PUMP_GUARD_PORT = 20050;

let genrocBin: string;
beforeAll(async () => {
  genrocBin = await buildGenrocBinary();
}, 60_000);

async function getStatus(genroc: GenrocProcess, id: string) {
  const { data, error } = await genroc.client.GET("/instances/{id}", {
    params: { path: { id } },
  });
  if (error) throw new Error(`get_instance failed: ${JSON.stringify(error)}`);
  return data!;
}

// failed → retry → completes, without re-executing the task that already succeeded.
test("retry failed instance — resumes from the failed task", async () => {
  const name = `retry_failed_${crypto.randomUUID()}`;
  const step1Mock = await startMockService(0, { response: { ok: true } });
  let step2Mock = await startMockService(0, { statusCode: 500 });
  const step2Port = step2Mock.port;

  try {
    await client.PUT("/definitions", {
      body: {
        name,
        tasks: [
          {
            id: "step1",
            action: { type: "fetch" as const, url: `http://localhost:${step1Mock.port}/action` },
            timeout: 2000,
            switch: [{ goto: "next" }],
          },
          {
            id: "step2",
            action: { type: "fetch" as const, url: `http://localhost:${step2Port}/action` },
            timeout: 2000,
            switch: [{ goto: "end" }],
          },
        ],
      },
    });

    const { data: startData } = await client.POST("/instances", { body: { process: name } });
    const id = startData!.id;
    expect(await waitForInstance(id, 15_000)).toBe("failed");
    expect(step1Mock.requestCount()).toBe(1);

    // Fix the failing service: restart the mock on the same port with a 200.
    await step2Mock.stop();
    step2Mock = await startMockService(step2Port, { response: { done: true } });

    const { error: retryErr } = await client.POST("/instances/{id}/retry", {
      params: { path: { id } },
    });
    expect(retryErr).toBeUndefined();

    expect(await waitForInstance(id, 15_000)).toBe("completed");
    // step1 was never re-executed — the retry resumed at step2.
    expect(step1Mock.requestCount()).toBe(1);
  } finally {
    await step1Mock.stop();
    await step2Mock.stop();
  }
}, 30_000);

// retry is for failures only: a paused process has not failed and is owed no extra
// attempt, so the endpoint refuses it and points at resume instead.
test("retry on a paused instance — rejected, pointing at resume", async () => {
  const name = `retry_paused_${crypto.randomUUID()}`;
  const db = join(tmpdir(), `genroc_retry_paused_${Date.now()}.db`);
  const genroc = await startGenroc(genrocBin, TICK_PORT, db, undefined, 0);

  const step1Mock = await startMockService(0, { response: { ok: true } });
  const step2Mock = await startMockService(0, { response: { done: true } });

  try {
    await genroc.client.PUT("/definitions", {
      body: {
        name,
        tasks: [
          {
            id: "step1",
            action: { type: "fetch" as const, url: `http://localhost:${step1Mock.port}/action` },
            timeout: 2000,
            switch: [{ goto: "next" }],
          },
          {
            id: "step2",
            action: { type: "fetch" as const, url: `http://localhost:${step2Mock.port}/action` },
            timeout: 2000,
            switch: [{ goto: "end" }],
          },
        ],
      },
    });

    const { data: startData } = await genroc.client.POST("/instances", { body: { process: name } });
    const id = startData!.id;

    // Tick 1 — step1 executes; the pause lands between tasks.
    await tick(genroc.client);
    await genroc.client.POST("/instances/{id}/pause", { params: { path: { id } } });
    expect((await getStatus(genroc, id)).status).toBe("paused");

    const { error: retryErr } = await genroc.client.POST("/instances/{id}/retry", {
      params: { path: { id } },
    });
    expect(retryErr).toBeDefined();
    expect(JSON.stringify(retryErr)).toContain("paused, not failed");
    expect((await getStatus(genroc, id)).status).toBe("paused");

    // Resume is the operation that actually applies, and it changes nothing else:
    // step2 runs next, step1 is not repeated.
    await genroc.client.POST("/instances/{id}/resume", { params: { path: { id } } });
    await tick(genroc.client);
    expect((await getStatus(genroc, id)).status).toBe("completed");
    expect(step1Mock.requestCount()).toBe(1);
    expect(step2Mock.requestCount()).toBe(1);
  } finally {
    genroc.stop();
    await step1Mock.stop();
    await step2Mock.stop();
  }
}, 30_000);

// only_once → plain retry rejected, force retry succeeds.
test("retry only_once task — rejected without force, allowed with force", async () => {
  const name = `retry_only_once_${crypto.randomUUID()}`;
  let chargeMock = await startMockService(0, { statusCode: 500 });
  const chargePort = chargeMock.port;

  try {
    await client.PUT("/definitions", {
      body: {
        name,
        tasks: [
          {
            id: "charge",
            only_once: true,
            action: { type: "fetch" as const, url: `http://localhost:${chargePort}/action` },
            timeout: 2000,
            switch: [{ goto: "end" }],
          },
        ],
      },
    });

    const { data: startData } = await client.POST("/instances", { body: { process: name } });
    const id = startData!.id;
    expect(await waitForInstance(id, 15_000)).toBe("failed");

    // Plain retry is rejected: the pending task is only_once.
    const { error: plainErr } = await client.POST("/instances/{id}/retry", {
      params: { path: { id } },
    });
    expect(plainErr).toBeDefined();
    expect(JSON.stringify(plainErr)).toContain("only_once");

    // Fix the service, then force the retry.
    await chargeMock.stop();
    chargeMock = await startMockService(chargePort, { response: { ok: true } });

    const { error: forceErr } = await client.POST("/instances/{id}/retry", {
      params: { path: { id }, query: { force: true } },
    });
    expect(forceErr).toBeUndefined();
    expect(await waitForInstance(id, 15_000)).toBe("completed");
  } finally {
    await chargeMock.stop();
  }
}, 30_000);

// retry/pause on a child instance → rejected with the root's id.
test("retry and pause on non-root instance — rejected naming the root", async () => {
  const id = crypto.randomUUID();
  const leafName = `nonroot_leaf_${id}`;
  const rootName = `nonroot_root_${id}`;
  const failMock = await startMockService(0, { statusCode: 500 });

  try {
    await client.PUT("/definitions", {
      body: {
        name: leafName,
        tasks: [
          {
            id: "work",
            action: { type: "fetch" as const, url: `http://localhost:${failMock.port}/action` },
            timeout: 2000,
            switch: [{ goto: "end" }],
          },
        ],
      },
    });
    await client.PUT("/definitions", {
      body: {
        name: rootName,
        tasks: [
          {
            id: "spawn",
            action: { type: "child_map" as const, children: { out: { name: leafName } } },
            switch: [{ goto: "end" }],
          },
        ],
      },
    });

    const { data: startData } = await client.POST("/instances", { body: { process: rootName } });
    const rootId = startData!.id;
    expect(await waitForInstance(rootId, 15_000)).toBe("failed");

    // A child_map's placeholder is keyed by the entry name.
    const childId = ((await childrenOfTask(rootId, "spawn")) as Record<string, string>)?.out;
    expect(childId).toBeTruthy();

    const { error: retryErr } = await client.POST("/instances/{id}/retry", {
      params: { path: { id: childId } },
    });
    expect(retryErr).toBeDefined();
    expect(JSON.stringify(retryErr)).toContain(rootId);

    const { error: pauseErr } = await client.POST("/instances/{id}/pause", {
      params: { path: { id: childId } },
    });
    expect(pauseErr).toBeDefined();
    expect(JSON.stringify(pauseErr)).toContain(rootId);
  } finally {
    await failMock.stop();
  }
}, 30_000);

// parallel children, one failed → root retry re-runs only the failed child.
test("retry with parallel children — only the failed child re-runs", async () => {
  const id = crypto.randomUUID();
  const goodName = `par_good_${id}`;
  const badName = `par_bad_${id}`;
  const rootName = `par_root_${id}`;

  const goodMock = await startMockService(0, { response: { ok: true } });
  let badMock = await startMockService(0, { statusCode: 500 });
  const badPort = badMock.port;

  try {
    await client.PUT("/definitions", {
      body: {
        name: goodName,
        tasks: [
          {
            id: "work",
            action: { type: "fetch" as const, url: `http://localhost:${goodMock.port}/action` },
            timeout: 2000,
            switch: [{ goto: "end" }],
          },
        ],
        output: { slot: "good" },
      },
    });
    await client.PUT("/definitions", {
      body: {
        name: badName,
        tasks: [
          {
            id: "work",
            action: { type: "fetch" as const, url: `http://localhost:${badPort}/action` },
            timeout: 2000,
            switch: [{ goto: "end" }],
          },
        ],
        output: { slot: "bad" },
      },
    });
    await client.PUT("/definitions", {
      body: {
        name: rootName,
        tasks: [
          {
            id: "fanout",
            action: {
              type: "child_map" as const,
              children: {
                good: { name: goodName, result_schema: {} },
                bad: { name: badName, result_schema: {} },
              },
            },
            output: "$: self.result",
            switch: [{ goto: "end" }],
          },
        ],
        output: { kids: "$: outputs.fanout" },
      },
    });

    const { data: startData } = await client.POST("/instances", { body: { process: rootName } });
    const rootId = startData!.id;
    expect(await waitForInstance(rootId, 15_000)).toBe("failed");
    expect(goodMock.requestCount()).toBe(1);

    // Fix the failing service and retry the root.
    await badMock.stop();
    badMock = await startMockService(badPort, { response: { ok: true } });

    const { error: retryErr } = await client.POST("/instances/{id}/retry", {
      params: { path: { id: rootId } },
    });
    expect(retryErr).toBeUndefined();

    expect(await waitForInstance(rootId, 15_000)).toBe("completed");
    // The completed child was never re-executed.
    expect(goodMock.requestCount()).toBe(1);

    // Asserting the COLLECTED OUTPUT, not just the status: a revived parent whose batch was
    // orphaned still reaches 'completed', merging {} and reporting success with every
    // child's output silently gone. Status alone cannot tell the two apart.
    const { data: detail } = await client.GET("/instances/{id}/detail", {
      params: { path: { id: rootId } },
    });
    expect((detail?.state?.output as any)?.kids).toEqual({
      good: { slot: "good" },
      bad: { slot: "bad" },
    });
  } finally {
    await goodMock.stop();
    await badMock.stop();
  }
}, 30_000);

// /tick is a manual-mode tool: when the continuous pump is running (poll > 0),
// an out-of-band tick would race it, so the endpoint refuses.
test("tick is rejected when the engine runs the continuous pump", async () => {
  const db = join(tmpdir(), `genroc_tick_guard_${Date.now()}.db`);
  // No poll arg → server uses its default poll interval (continuous mode).
  const genroc = await startGenroc(genrocBin, PUMP_GUARD_PORT, db);
  try {
    const { error } = await genroc.client.POST("/tick", { body: { advance_ms: 0 } });
    expect(error).toBeDefined();
    expect(JSON.stringify(error)).toContain("manual mode");
  } finally {
    genroc.stop();
  }
}, 30_000);

// The case §12 was written for: a child concluded with a RAISE its parent has no rule for,
// so the tree failed. The condition is fixed outside, and retry must actually run that child
// again — reviving it would only re-run the switch that decided to raise, against the same
// upstream state. specs/child-error-handling.md §12.
test("retry re-spawns a raised child once its cause is fixed", async () => {
  const uid = crypto.randomUUID().slice(0, 8);
  const childName = `respawn_child_${uid}`;
  const rootName = `respawn_root_${uid}`;

  let mock = await startMockService(0, { statusCode: 503 });
  const port = mock.port;
  try {
    const { error: childErr } = await client.PUT("/definitions", {
      body: {
        name: childName,
        tasks: [
          {
            id: "call",
            action: {
              type: "fetch" as const,
              url: `http://localhost:${port}/action`,
              responses: { "200": {} },
            },
            timeout: 2000,
            on_error: [{ code: ["http.5%"], raise: { code: "svc_down", message: "service is down" } }],
            output: "$: self.result",
            switch: [{ goto: "end" }],
          },
        ],
        output: { reached: true },
      } as never,
    });
    expect(childErr).toBeUndefined();

    const { error: rootErr } = await client.PUT("/definitions", {
      body: {
        name: rootName,
        tasks: [
          {
            id: "run",
            action: { type: "child" as const, name: childName, result_schema: {} },
            output: "$: self.result",
            switch: [{ goto: "end" }],
          },
        ],
        output: { kid: "$: outputs.run" },
      } as never,
    });
    expect(rootErr).toBeUndefined();

    const { data: started } = await client.POST("/instances", { body: { process: rootName } });
    const id = started!.id;
    expect(await waitForInstance(id, 15_000)).toBe("failed");
    const { data: failed } = await client.GET("/instances/{id}", { params: { path: { id } } });
    expect(failed!.error_code, "an unmatched raise fails the parent with the child's code").toBe("svc_down");

    // Fix the cause the child raised about, then retry.
    await mock.stop();
    mock = await startMockService(port, { response: { ok: true } });

    const { error: retryErr } = await client.POST("/instances/{id}/retry", { params: { path: { id } } });
    expect(retryErr).toBeUndefined();

    expect(await waitForInstance(id, 15_000)).toBe("completed");
    expect(mock.requestCount(), "the replacement child really ran; a revived one would not have called out again").toBe(1);
    const { data: detail } = await client.GET("/instances/{id}/detail", { params: { path: { id } } });
    expect((detail?.state?.output as any)?.kid).toEqual({ reached: true });

    // The slot now has two rows, and the placeholder must name the LIVE one. The retired
    // attempt is the older of the two, so a single `child` that took the first row would
    // point an operator at the instance that raised rather than the one that succeeded.
    const live = await listAllInstances();
    const kids = live.filter((i) => i.parent_id === id);
    expect(kids, "both attempts are kept as history").toHaveLength(2);
    const completedKid = kids.find((k) => k.status === "completed");
    expect((detail?.children as any)?.run).toBe(completedKid!.id);
  } finally {
    await mock.stop();
  }
}, 30_000);

// §5.5: a child task retries like any other task. The child raises a code the parent names in
// on_error with a budget; each round re-spawns the raised slot, and when the budget is spent
// the rule routes. Exhaustion is the deterministic half — it proves admission runs, that the
// per-slot counter advances (rather than resetting, which would never terminate), and that
// routing waits for the budget.
test("child task retry — a raised slot is re-spawned until its budget is spent", async () => {
  const uid = crypto.randomUUID().slice(0, 8);
  const childName = `slotretry_child_${uid}`;
  const rootName = `slotretry_root_${uid}`;

  const mock = await startMockService(0, { statusCode: 503 });
  try {
    const { error: childErr } = await client.PUT("/definitions", {
      body: {
        name: childName,
        tasks: [
          {
            id: "call",
            action: {
              type: "fetch" as const,
              url: `http://localhost:${mock.port}/action`,
              responses: { "200": {} },
            },
            timeout: 2000,
            on_error: [{ code: ["http.5%"], raise: { code: "svc_down", message: "down" } }],
            output: "$: self.result",
            switch: [{ goto: "end" }],
          },
        ],
        output: { reached: true },
      } as never,
    });
    expect(childErr).toBeUndefined();

    const { error: rootErr } = await client.PUT("/definitions", {
      body: {
        name: rootName,
        tasks: [
          {
            id: "run",
            action: { type: "child" as const, name: childName, result_schema: {} },
            on_error: [{ code: ["svc_down"], retry: { attempts: 2, delay: 10 }, goto: "$gave_up" }],
            output: "$: self.result",
            switch: [{ goto: "end" }],
          },
          { id: "gave_up", output: { gave_up: true }, switch: [{ goto: "end" }] },
        ],
        output: "$: outputs.gave_up",
      } as never,
    });
    expect(rootErr).toBeUndefined();

    const { data: started } = await client.POST("/instances", { body: { process: rootName } });
    const id = started!.id;

    expect(await waitForInstance(id, 20_000)).toBe("completed");
    const { data: detail } = await client.GET("/instances/{id}/detail", { params: { path: { id } } });
    expect(detail?.state?.output, "the rule routes once the budget is spent").toEqual({ gave_up: true });
    // 1 first attempt + 2 retries. Fewer means admission never fired; more means the counter
    // reset each round, which is the shape that never terminates.
    expect(mock.requestCount(), "the slot ran once per admitted attempt").toBe(3);
  } finally {
    await mock.stop();
  }
}, 40_000);

// The fan-out case per-slot budgets exist for: one slot raises and retries on its own while a
// completed sibling stands. A shared budget could not express this, and a batch-wide re-spawn
// would re-run the sibling. specs/child-error-handling.md §5.5.
test("child task retry — one slot retries while its completed sibling stands", async () => {
  const uid = crypto.randomUUID().slice(0, 8);
  const goodName = `fanretry_good_${uid}`;
  const flakyName = `fanretry_flaky_${uid}`;
  const rootName = `fanretry_root_${uid}`;

  const goodMock = await startMockService(0, { response: { ok: true } });
  const flakyMock = await startMockService(0, { statusCode: 503 });
  try {
    for (const [name, port, raises] of [
      [goodName, goodMock.port, false],
      [flakyName, flakyMock.port, true],
    ] as const) {
      const { error } = await client.PUT("/definitions", {
        body: {
          name,
          tasks: [
            {
              id: "call",
              action: { type: "fetch" as const, url: `http://localhost:${port}/action`, responses: { "200": {} } },
              timeout: 2000,
              ...(raises
                ? { on_error: [{ code: ["http.5%"], raise: { code: "svc_down", message: "down" } }] }
                : {}),
              output: "$: self.result",
              switch: [{ goto: "end" }],
            },
          ],
          output: { from: name },
        } as never,
      });
      expect(error).toBeUndefined();
    }

    const { error: rootErr } = await client.PUT("/definitions", {
      body: {
        name: rootName,
        tasks: [
          {
            id: "fanout",
            action: {
              type: "child_map" as const,
              children: {
                good: { name: goodName, result_schema: {} },
                flaky: { name: flakyName, result_schema: {} },
              },
            },
            on_error: [{ code: ["svc_down"], retry: { attempts: 2, delay: 10 }, goto: "$gave_up" }],
            output: "$: self.result",
            switch: [{ goto: "end" }],
          },
          { id: "gave_up", output: { gave_up: true }, switch: [{ goto: "end" }] },
        ],
        output: "$: outputs.gave_up",
      } as never,
    });
    expect(rootErr).toBeUndefined();

    const { data: started } = await client.POST("/instances", { body: { process: rootName } });
    const id = started!.id;
    expect(await waitForInstance(id, 20_000)).toBe("completed");

    expect(flakyMock.requestCount(), "the raised slot ran once per admitted attempt").toBe(3);
    expect(goodMock.requestCount(), "a completed sibling is never re-run — only the raised slot is replaced").toBe(1);

    // One audit line per re-spawned SLOT, naming which slot and which attempt. A per-round
    // line cannot say either, and a round is not a unit anyone debugs.
    const { data: logs } = await client.GET("/instances/{id}/logs", {
      params: { path: { id }, query: { limit: 100 } },
    });
    const respawns = (logs?.items ?? []).filter((l: any) => String(l.message ?? "").includes("re-spawning"));
    expect(respawns).toHaveLength(2);
    expect(String(respawns[0].message)).toContain('child_key "flaky"');
    expect(respawns.map((l: any) => String(l.message).match(/attempt (\d)\//)?.[1]).sort()).toEqual(["1", "2"]);
  } finally {
    await goodMock.stop();
    await flakyMock.stop();
  }
}, 40_000);

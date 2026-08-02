import { expect, test, beforeAll } from "vitest";
import { join } from "path";
import { tmpdir } from "os";
import { buildGenrocBinary, startGenroc, type GenrocProcess } from "../helpers/server.ts";
import { client, startMockService, waitForInstance, tick } from "../helpers/client.ts";

const TICK_PORT = 20017;

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

    // The spawn placeholder in the root's context (_children) holds the child id,
    // keyed by the child_map entry name.
    const { data: rootData } = await client.GET("/instances/{id}", {
      params: { path: { id: rootId } },
    });
    const childId = (rootData?.context as any)?._children?.spawn?.out as string;
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
                good: { name: goodName },
                bad: { name: badName },
              },
            },
            switch: [{ goto: "end" }],
          },
        ],
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
  const genroc = await startGenroc(genrocBin, TICK_PORT + 2, db);
  try {
    const { error } = await genroc.client.POST("/tick", { body: { advance_ms: 0 } });
    expect(error).toBeDefined();
    expect(JSON.stringify(error)).toContain("manual mode");
  } finally {
    genroc.stop();
  }
}, 30_000);

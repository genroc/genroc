import { expect, test, beforeAll } from "vitest";
import { join } from "path";
import { tmpdir } from "os";
import { buildGenrocBinary, startGenroc, type GenrocProcess } from "../helpers/server.ts";
import { startMockService, tick } from "../helpers/client.ts";

const TICK_PORT = 20013;

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

// Verifies that pausing between two tasks stops execution cleanly, and that
// resuming picks up at exactly the task the pause stopped in front of.
// Uses manual tick mode (-poll 0) so each engine cycle is explicit, making
// every intermediate DB state directly observable.
test("pause between tasks — step2 waits for the resume, then runs exactly once", async () => {
  const processName = `pause_tick_${crypto.randomUUID()}`;
  const db = join(tmpdir(), `genroc_pause_${Date.now()}.db`);
  const genroc = await startGenroc(genrocBin, TICK_PORT, db, undefined, 0);

  const step1Mock = await startMockService(0, { response: { ok: true } });
  const step2Mock = await startMockService(0, { response: { done: true } });

  try {
    await genroc.client.PUT("/definitions", {
      body: {
        name: processName,
        tasks: [
          {
            id: "step1",
            action: {
              type: "fetch" as const,
              url: `http://localhost:${step1Mock.port}/action`,
            },
            timeout: 5_000,
            switch: [{ goto: "next" }],
          },
          {
            id: "step2",
            action: {
              type: "fetch" as const,
              url: `http://localhost:${step2Mock.port}/action`,
            },
            timeout: 5_000,
            switch: [{ goto: "end" }],
          },
        ],
      },
    });

    const { data: startData } = await genroc.client.POST("/instances", {
      body: { process: processName },
    });
    const id = startData!.id;

    // Before any tick: instance exists but no tasks have run yet.
    const s0 = await getStatus(genroc, id);
    expect(s0.status).toBe("running");
    expect(step1Mock.requestCount()).toBe(0);

    // Tick 1 — engine executes step1, then writes updated queue via
    // UpdateInstanceProgress (does not touch status).
    expect(await tick(genroc.client)).toBe(1);
    expect(step1Mock.requestCount()).toBe(1);
    expect(step2Mock.requestCount()).toBe(0);

    const s1 = await getStatus(genroc, id);
    expect(s1.status).toBe("running"); // still running, waiting for next tick

    // Pause between tasks. The instance holds no lease here (tick 1 released it
    // when it persisted step1's result), so there is no in-flight task to wait
    // out and it is suspended outright — no 'pausing' step, no extra tick.
    await genroc.client.POST("/instances/{id}/pause", {
      params: { path: { id } },
    });

    const s2 = await getStatus(genroc, id);
    expect(s2.status).toBe("paused");

    // Tick 2 — a paused instance is not claimable, so nothing runs at all and
    // step2 is left untouched.
    expect(await tick(genroc.client)).toBe(0);
    expect(step2Mock.requestCount()).toBe(0);
    expect((await getStatus(genroc, id)).status).toBe("paused");

    // Resume, and the process continues from step2 — the task the pause stopped
    // in front of, executed once and only now.
    await genroc.client.POST("/instances/{id}/resume", {
      params: { path: { id } },
    });
    expect((await getStatus(genroc, id)).status).toBe("running");

    expect(await tick(genroc.client)).toBe(1);
    expect(step2Mock.requestCount()).toBe(1);
    expect(step1Mock.requestCount()).toBe(1); // completed work never re-run

    const s3 = await getStatus(genroc, id);
    expect(s3.status).toBe("completed");
  } finally {
    genroc.stop();
    await step1Mock.stop();
    await step2Mock.stop();
  }
}, 30_000);

import { expect, test } from "vitest";
import { client, startMockService, waitForInstance } from "../helpers/client.ts";

// The response cap turns an unbounded body (an OOM stranding every lease) into result.too_large.

// Comfortably past the 8 MiB cap in internal/transport.
const OVERSIZED = "y".repeat(9 * 1024 * 1024);

async function getInstance(id: string) {
  const { data, error } = await client.GET("/instances/{id}", { params: { path: { id } } });
  if (error) throw new Error(`get_instance failed: ${JSON.stringify(error)}`);
  return data!;
}

test("oversized response — the instance fails with result.too_large", async () => {
  const mock = await startMockService(0, { response: { blob: OVERSIZED } });
  const name = `too_large_${crypto.randomUUID()}`;
  try {
    await client.PUT("/definitions", {
      body: {
        name,
        tasks: [{
          id: "fetch_blob",
          action: { type: "fetch" as const, method: "post", url: `http://localhost:${mock.port}/blob`, timeout: 10_000 },
          switch: [{ goto: "end" }],
        }],
      },
    });

    const { data: started } = await client.POST("/instances", { body: { process: name } });
    const id = started!.id;

    expect(await waitForInstance(id, 20_000)).toBe("failed");
    expect((await getInstance(id)).error_code).toBe("result.too_large");
  } finally {
    await mock.stop();
  }
}, 40_000);

test("oversized response — on_error catches result.too_large and routes on", async () => {
  // A response did arrive, so this is catchable; as an engine.* failure the goto would never be taken.
  const mock = await startMockService(0, { response: { blob: OVERSIZED } });
  const name = `too_large_caught_${crypto.randomUUID()}`;
  try {
    await client.PUT("/definitions", {
      body: {
        name,
        tasks: [
          {
            id: "fetch_blob",
            action: { type: "fetch" as const, method: "post", url: `http://localhost:${mock.port}/blob`, timeout: 10_000 },
            on_error: [{ code: ["result.too_large"], goto: "$fallback" }],
            switch: [{ goto: "end" }],
          },
          {
            id: "fallback",
            action: { type: "delay" as const, for: "1ms" },
            switch: [{ goto: "end" }],
          },
        ],
      },
    });

    const { data: started } = await client.POST("/instances", { body: { process: name } });
    const id = started!.id;

    expect(await waitForInstance(id, 20_000)).toBe("completed");
    expect((await getInstance(id)).task).toBe("fallback");
  } finally {
    await mock.stop();
  }
}, 40_000);

test("oversized response — a body under the cap is unaffected", async () => {
  const mock = await startMockService(0, { response: { blob: "z".repeat(64 * 1024) } });
  const name = `under_cap_${crypto.randomUUID()}`;
  try {
    await client.PUT("/definitions", {
      body: {
        name,
        tasks: [{
          id: "fetch_blob",
          action: { type: "fetch" as const, method: "post", url: `http://localhost:${mock.port}/blob`, timeout: 10_000 },
          switch: [{ goto: "end" }],
        }],
      },
    });

    const { data: started } = await client.POST("/instances", { body: { process: name } });
    expect(await waitForInstance(started!.id, 20_000)).toBe("completed");
  } finally {
    await mock.stop();
  }
}, 40_000);

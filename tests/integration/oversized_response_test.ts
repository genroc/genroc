import { expect, test } from "vitest";
import { client, startMockService, waitForInstance } from "../helpers/client.ts";

// A fetch used to decode the response body with no size limit. A worker holds leases on
// every instance it claimed, so one endpoint streaming an unbounded body OOMs the process
// and strands all of them until those leases expire. The cap turns that into
// output.too_large — and because a response *did* arrive, it is an ordinary catchable
// call error rather than a terminal engine failure.

// Comfortably past the 8 MiB cap in internal/transport.
const OVERSIZED = "y".repeat(9 * 1024 * 1024);

async function getInstance(id: string) {
  const { data, error } = await client.GET("/instances/{id}", { params: { path: { id } } });
  if (error) throw new Error(`get_instance failed: ${JSON.stringify(error)}`);
  return data!;
}

test("oversized response — the instance fails with output.too_large", async () => {
  const mock = await startMockService(0, { response: { blob: OVERSIZED } });
  const name = `too_large_${crypto.randomUUID()}`;
  try {
    await client.PUT("/definitions", {
      body: {
        name,
        tasks: [{
          id: "fetch_blob",
          action: { type: "fetch" as const, url: `http://localhost:${mock.port}/blob` },
          timeout: 10_000,
          switch: [{ goto: "end" }],
        }],
      },
    });

    const { data: started } = await client.POST("/instances", { body: { process: name } });
    const id = started!.id;

    expect(await waitForInstance(id, 20_000)).toBe("failed");
    expect((await getInstance(id)).error_code).toBe("output.too_large");
  } finally {
    await mock.stop();
  }
}, 40_000);

test("oversized response — on_error catches output.too_large and routes on", async () => {
  // The claim the code makes by not being in the unknowable set: a definition can handle
  // this like any other call error. If it were reported as a terminal engine.* failure
  // instead, the goto below would never be taken and the instance would end failed.
  const mock = await startMockService(0, { response: { blob: OVERSIZED } });
  const name = `too_large_caught_${crypto.randomUUID()}`;
  try {
    await client.PUT("/definitions", {
      body: {
        name,
        tasks: [
          {
            id: "fetch_blob",
            action: { type: "fetch" as const, url: `http://localhost:${mock.port}/blob` },
            timeout: 10_000,
            on_error: [{ code: ["output.too_large"], goto: "$fallback" }],
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
          action: { type: "fetch" as const, url: `http://localhost:${mock.port}/blob` },
          timeout: 10_000,
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

import { expect, test } from "vitest";
import { client, startMockService, waitForInstance, spliceObjects } from "../helpers/client.ts";

// self.previous accumulates in-memory (no action) and across persist + reclaim (an action per
// iteration); both pass 2 KiB, so self.previous must resolve an externalized ref.

// Crosses the 2 KiB threshold within a few iterations while keeping server work modest.
const CHUNK = "0123456789".repeat(100); // 1000 chars

// With actionPort set, each iteration also makes a REST call, forcing a persist + reclaim between
// iterations.
function makeDef(name: string, actionPort?: number) {
  const append: Record<string, unknown> = {
    id: "append",
    output: {
      text: `$: (self.previous.text ?? '') + '${CHUNK}'`,
      i: "$: (self.previous.i ?? 0) + 1",
    },
    switch: [
      // self.output, not outputs.append: inside its own task, outputs.<id> is the PREVIOUS
      // output in every slot, the switch included. Reading it here would loop n+1 times.
      { case: "self.output.i < input.n", goto: "$append" },
      { goto: "end" },
    ],
  };
  if (actionPort !== undefined) {
    append.action = {
      type: "fetch",
      method: "post",
      url: `http://localhost:${actionPort}/step`,
      responses: { 200: { type: "object", properties: { ok: { type: "boolean" } } } },
    };
  }
  return {
    name,
    input_schema: {
      type: "object",
      properties: { n: { type: "integer" } },
      required: ["n"],
    },
    tasks: [append],
    output: {
      text: "$: outputs.append.text",
      count: "$: outputs.append.i",
    },
  };
}

async function register(def: ReturnType<typeof makeDef>) {
  const { error } = await client.PUT("/definitions", { body: def as never });
  if (error) throw new Error(`register failed: ${JSON.stringify(error)}`);
}

async function runAndReadOutput(name: string, n: number, timeoutMs: number) {
  const { data: started, error } = await client.POST("/instances", {
    body: { process: name, input: { n } },
  });
  if (error) throw new Error(`start failed: ${JSON.stringify(error)}`);
  const id = started!.id;
  expect(await waitForInstance(id, timeoutMs)).toBe("completed");
  // The accumulated text is far past the 2 KiB cutoff so the output externalizes — splice it back to read it.
  const { data, error: getErr } = await client.GET("/instances/{id}/detail", { params: { path: { id } } });
  if (getErr) throw new Error(`get failed: ${JSON.stringify(getErr)}`);
  await spliceObjects(data);
  return data!.output as { text: string; count: number };
}

test("self.previous accumulates across an in-memory loop (single advance)", async () => {
  // 20 × 1000 chars ≈ 20 KB (crosses the 2 KiB threshold); under the 1000 inline-task cap,
  // and with no action the whole loop runs in one advance().
  const n = 20;
  const name = `loop_inmem_${crypto.randomUUID()}`;
  await register(makeDef(name));

  const out = await runAndReadOutput(name, n, 10_000);
  expect(out.count).toBe(n);
  expect(out.text).toBe(CHUNK.repeat(n));
  expect(out.text.length).toBe(CHUNK.length * n);
});

test("self.previous accumulates across DB persist+reclaim (action each iteration)", async () => {
  // Past iteration 3 self.previous reloads as an externalized ref; unresolved, it would reset to ""
  // and the loop would never terminate.
  const n = 20;
  const mock = await startMockService(0, { response: { ok: true } });
  try {
    const name = `loop_db_${crypto.randomUUID()}`;
    await register(makeDef(name, mock.port));

    const out = await runAndReadOutput(name, n, 30_000);
    expect(out.count).toBe(n);
    expect(out.text).toBe(CHUNK.repeat(n));
    // One REST call per iteration ⇒ the loop genuinely persisted and reclaimed n times,
    // rather than collapsing into a single in-memory advance.
    expect(mock.requestCount()).toBe(n);
  } finally {
    await mock.stop();
  }
});

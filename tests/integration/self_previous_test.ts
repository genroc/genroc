import { expect, test } from "vitest";
import { client, startMockService, waitForInstance, spliceObjects } from "../helpers/client.ts";

// self.previous is a task's own prior output — the same value as outputs[<this task>]. A
// task that gotos itself and appends to self.previous each iteration accumulates a string.
// We exercise that in the two execution modes the engine distinguishes:
//   1. no action in the loop  → the whole loop runs in one in-memory advance().
//   2. an action each iteration → the engine persists + reclaims between iterations, so
//      self.previous round-trips through the DB every time.
// Both deliberately push the accumulated value past the 8 KiB object-store threshold, so
// outputs[<task>] externalizes and reloads as an *ObjectRef — which self.previous must
// resolve exactly like outputs.<id> does (the regression scenario 2 guards).

// A big chunk so the accumulator crosses the 8 KiB threshold within a handful of
// iterations (each iteration appends CHUNK), keeping the server-side work modest.
const CHUNK = "0123456789".repeat(100); // 1000 chars

// makeDef builds a process whose single "append" task loops on itself, appending CHUNK to
// self.previous.text and counting in self.previous.i, until the count reaches input.n. The
// process projects the accumulated text + final count as its output. With actionPort set,
// each iteration also makes a REST call, which forces a persist+reclaim between iterations.
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
  // The accumulated text is >8 KiB so the output externalizes — splice it back to read it.
  const { data, error: getErr } = await client.GET("/instances/{id}/detail", { params: { path: { id } } });
  if (getErr) throw new Error(`get failed: ${JSON.stringify(getErr)}`);
  await spliceObjects(data);
  return (data!.state as Record<string, { text: string; count: number }>).output;
}

test("self.previous accumulates across an in-memory loop (single advance)", async () => {
  // 20 × 1000 chars ≈ 20 KB (crosses the 8 KiB threshold); under the 1000 inline-task cap,
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
  // text crosses 8 KiB at iteration 9, so the remaining iterations reload self.previous as
  // an externalized ref that must resolve — without the resolve it would silently reset to
  // "" (and the counter with it, so the loop would never even terminate).
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

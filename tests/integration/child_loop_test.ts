import { expect, test } from "vitest";
import { client, waitForInstance } from "../helpers/client.ts";

// A child task re-entered by a loop spawns a NEW batch each pass. Children live under
// (parent_id, spawn_task_id), so without a generation number the second collect saw every
// child ever spawned there — `expected exactly one child, got 2` for a single child, and
// silently merged duplicate slots for the keyed and list shapes. spawn_epoch scopes both
// the collect and the wake decision to the current batch.

async function define(name: string, body: Record<string, unknown>) {
  const { error } = await client.PUT("/definitions", { body: { name, ...body } as never });
  expect(error).toBeUndefined();
}

async function run(name: string, input: unknown) {
  const { data: started } = await client.POST("/instances", { body: { process: name, input } as never });
  const status = await waitForInstance(started!.id, 20_000);
  const { data } = await client.GET("/instances/{id}", { params: { path: { id: started!.id } } });
  return { status, data };
}

/** Echoes its input so a caller can prove WHICH pass's child it collected. */
async function echoChild(): Promise<string> {
  const name = `loop_echo_${crypto.randomUUID()}`;
  await define(name, {
    input_schema: { type: "object", properties: { i: { type: "number" } } },
    tasks: [{ id: "t", output: { seen: "$: input.i ?? 0" }, switch: [{ goto: "end" }] }],
    output: "$: outputs.t",
  });
  return name;
}

test("child in a loop — each pass collects its own child, not every child ever spawned", async () => {
  const child = await echoChild();
  const name = `loop_single_${crypto.randomUUID()}`;
  await define(name, {
    input_schema: { type: "object", properties: { n: { type: "integer" } }, required: ["n"] },
    tasks: [
      {
        id: "tick",
        output: { i: "$: (self.previous.i ?? 0) + 1" },
        switch: [{ goto: "$call" }],
      },
      {
        id: "call",
        action: {
          type: "child",
          name: child,
          // Whole object: navigating outputs.tick.i here does not resolve at a child input.
          input: "$: outputs.tick",
          result_schema: { type: "object", properties: { seen: { type: "number" } }, required: ["seen"] },
        },
        output: { seen: "$: self.result.seen" },
        switch: [{ case: "outputs.tick.i >= input.n", goto: "end" }, { goto: "$tick" }],
      },
    ],
    output: { rounds: "$: outputs.tick.i", last_seen: "$: outputs.call.seen" },
  });

  const { status, data } = await run(name, { n: 3 });
  expect(status, JSON.stringify(data?.error_message)).toBe("completed");
  // last_seen proves the THIRD pass's child was collected — not the first, and not a merge.
  expect(data?.context?.output).toEqual({ rounds: 3, last_seen: 3 });
});

test("child_list in a loop — the collected array is one pass's children, not the accumulation", async () => {
  const child = await echoChild();
  const name = `loop_list_${crypto.randomUUID()}`;
  await define(name, {
    input_schema: {
      type: "object",
      properties: {
        n: { type: "integer" },
        items: { type: "array", items: { type: "object", properties: { i: { type: "integer" } }, required: ["i"] } },
      },
      required: ["n", "items"],
    },
    tasks: [
      {
        id: "tick",
        output: { i: "$: (self.previous.i ?? 0) + 1" },
        switch: [{ goto: "$fan" }],
      },
      {
        id: "fan",
        action: {
          type: "child_list",
          name: child,
          over: "$: input.items",
          result_schema: { type: "object", properties: { seen: { type: "number" } }, required: ["seen"] },
        },
        // Two children per pass. Without scoping this array grows 2, 4, 6…
        output: { got: "$: map(self.result, r => r.seen)" },
        switch: [{ case: "outputs.tick.i >= input.n", goto: "end" }, { goto: "$tick" }],
      },
    ],
    output: { got: "$: outputs.fan.got" },
  });

  const { status, data } = await run(name, { n: 3, items: [{ i: 1 }, { i: 2 }] });
  expect(status, JSON.stringify(data?.error_message)).toBe("completed");
  expect((data?.context?.output as any)?.got).toEqual([1, 2]);
});

import { expect, test } from "vitest";
import { client, waitForInstance } from "../helpers/client.ts";

// The paths that reach a child task WITHOUT going through advance's ordinary switch, plus
// the one batch shape child_loop_test.ts does not cover. Each is a distinct enterTask call
// site (or a distinct collect path), and a site that forgets to move task_epoch re-spawns
// into the batch its predecessor already claimed.
//
// Only the third of these DISCRIMINATES on its own -- measured, by removing the bump and
// watching which fail. child_map merges duplicate keys with no error (buildMapChildOutput
// overwrites, and GetChildrenForTask has no ORDER BY), and a raised batch picks raised[0]
// the same way, so both would pass or fail by luck. Their deterministic assertion is on the
// batch numbers in tests/tick/task_epoch_test.ts. These two earn their place by running on
// BOTH engines -- the tick tests read SQLite directly and are single-engine.

async function define(name: string, body: Record<string, unknown>) {
  const { error } = await client.PUT("/definitions", { body: { name, ...body } as never });
  expect(error).toBeUndefined();
  return name;
}

async function run(name: string, input?: unknown) {
  const { data: started } = await client.POST("/instances", { body: { process: name, input } as never });
  const status = await waitForInstance(started!.id, 20_000);
  const { data } = await client.GET("/instances/{id}/detail", { params: { path: { id: started!.id } } });
  return { id: started!.id, status, data };
}

async function okLeaf() {
  return define(`edge_ok_${crypto.randomUUID()}`, {
    tasks: [{ id: "t", output: { ok: "$: true" }, switch: [{ goto: "end" }] }],
    output: "$: outputs.t",
  });
}

/** Always raises, so the parent leaves the task through the RAISED-batch route. */
async function raisingLeaf() {
  return define(`edge_raise_${crypto.randomUUID()}`, {
    tasks: [
      { id: "t", switch: [{ raise: { code: "always_raises", message: "nope" } }] },
    ],
  });
}

test("child_map in a loop — each pass collects its own keyed batch", async () => {
  const leaf = await okLeaf();
  const name = await define(`edge_map_${crypto.randomUUID()}`, {
    input_schema: { type: "object", properties: { n: { type: "integer" } }, required: ["n"] },
    tasks: [
      { id: "tick", output: { i: "$: (self.previous.i ?? 0) + 1" }, switch: [{ goto: "$fan" }] },
      {
        id: "fan",
        action: {
          type: "child_map",
          children: {
            a: { name: leaf, result_schema: { type: "object", properties: { ok: { type: "boolean" } } } },
            b: { name: leaf, result_schema: { type: "object", properties: { ok: { type: "boolean" } } } },
          },
        },
        // Keyed by child name: an unscoped collect merges DUPLICATE keys across passes.
        output: { keys: "$: self.result" },
        switch: [{ case: "outputs.tick.i >= input.n", goto: "end" }, { goto: "$tick" }],
      },
    ],
    output: { keys: "$: outputs.fan.keys" },
  });

  const { status, data } = await run(name, { n: 3 });
  expect(status, JSON.stringify(data?.error_message)).toBe("completed");
  expect(Object.keys((data?.state?.output as any)?.keys ?? {}).sort()).toEqual(["a", "b"]);
});

test("loop re-entered through a raised child's on_error route", async () => {
  const leaf = await raisingLeaf();
  const name = await define(`edge_raised_route_${crypto.randomUUID()}`, {
    input_schema: { type: "object", properties: { n: { type: "integer" } }, required: ["n"] },
    tasks: [
      {
        id: "call",
        action: { type: "child", name: leaf },
        // The route out of a raised batch is collect.go's own goto, not advance's switch.
        on_error: [{ code: ["always_raises"], goto: "$again" }],
        switch: [{ goto: "end" }],
      },
      {
        id: "again",
        output: { i: "$: (self.previous.i ?? 0) + 1" },
        switch: [{ case: "self.output.i >= input.n", goto: "end" }, { goto: "$call" }],
      },
    ],
    output: { rounds: "$: outputs.again.i" },
  });

  const { status, data } = await run(name, { n: 3 });
  expect(status, JSON.stringify(data?.error_message)).toBe("completed");
  expect((data?.state?.output as any)?.rounds).toBe(3);
});

test("loop re-entered through a call error's on_error route", async () => {
  const leaf = await okLeaf();
  const name = await define(`edge_call_route_${crypto.randomUUID()}`, {
    input_schema: { type: "object", properties: { n: { type: "integer" } }, required: ["n"] },
    tasks: [
      {
        id: "call",
        action: { type: "child", name: leaf },
        switch: [{ goto: "$probe" }],
      },
      {
        id: "probe",
        // Nothing listens on port 1, so this is a pre.* error every time.
        action: { type: "fetch", url: "http://localhost:1/x", method: "GET" },
        timeout: 2000,
        // error.go's goto is the third enterTask site.
        on_error: [{ code: ["pre.%", "http.%"], goto: "$again" }],
        switch: [{ goto: "end" }],
      },
      {
        id: "again",
        output: { i: "$: (self.previous.i ?? 0) + 1" },
        switch: [{ case: "self.output.i >= input.n", goto: "end" }, { goto: "$call" }],
      },
    ],
    output: { rounds: "$: outputs.again.i" },
  });

  const { status, data } = await run(name, { n: 3 });
  expect(status, JSON.stringify(data?.error_message)).toBe("completed");
  expect((data?.state?.output as any)?.rounds).toBe(3);
});

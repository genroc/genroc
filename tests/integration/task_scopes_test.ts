import { expect, test } from "vitest";
import { client, startMockService, waitForInstance } from "../helpers/client.ts";

// The task scope at RUNTIME. Validation pins which names each slot may use
// (internal/validation/validationtest/task_scopes_test.go); these pin what the engine
// actually puts behind them, which is the half a type-checker cannot see.
//
// The rule under test: outputs.<own id> is the PREVIOUS output in every slot of its own task,
// self.previous being the other name for it. The switch is where that is not free — the engine
// has already written the new output by then, so it must shadow the slot.

async function register(def: unknown) {
  const { error } = await client.PUT("/definitions", { body: def as never });
  if (error) throw new Error(`register failed: ${JSON.stringify(error)}`);
}

async function runToOutput(name: string, input: unknown = {}, timeoutMs = 15_000) {
  const { data: started, error } = await client.POST("/instances", {
    body: { process: name, input } as never,
  });
  if (error) throw new Error(`start failed: ${JSON.stringify(error)}`);
  const id = started!.id;
  expect(await waitForInstance(id, timeoutMs)).toBe("completed");
  const { data, error: getErr } = await client.GET("/instances/{id}/detail", { params: { path: { id } } });
  if (getErr) throw new Error(`get failed: ${JSON.stringify(getErr)}`);
  return (data!.state as { output: Record<string, unknown> }).output;
}

test("outputs.<own id> is the previous output in a pre-action slot and in the output map", async () => {
  // The query string is the only slot whose evaluated value leaves the process before the
  // action runs, so it is how a pre-action read is observed at all.
  const mock = await startMockService(0, { response: { ok: true } });
  try {
    const name = `scope_pre_${crypto.randomUUID()}`;
    await register({
      name,
      tasks: [
        {
          id: "t",
          action: {
            type: "fetch",
            url: `http://localhost:${mock.port}/step`,
            query: { seen: "${ outputs.t.i ?? 0 }" },
            responses: { 200: { type: "object", properties: { ok: { type: "boolean" } } } },
          },
          output: {
            i: "$: (self.previous.i ?? 0) + 1",
            // Both names for the previous output, read in the same slot: they must agree.
            own_outputs: "$: outputs.t.i ?? 0",
            own_previous: "$: self.previous.i ?? 0",
          },
          switch: [{ case: "self.output.i < 3", goto: "$t" }, { goto: "end" }],
        },
      ],
      output: {
        i: "$: outputs.t.i",
        own_outputs: "$: outputs.t.own_outputs",
        own_previous: "$: outputs.t.own_previous",
      },
    });

    const out = await runToOutput(name);
    expect(out.i).toBe(3);
    // Third run: the previous output was 2, and both names saw it.
    expect(out.own_outputs).toBe(2);
    expect(out.own_previous).toBe(2);

    // The query the action was built with, run by run: each saw the output of the run before.
    const seen = mock.requestUrls().map((u) => new URL(u, "http://x").searchParams.get("seen"));
    expect(seen).toEqual(["0", "1", "2"]);
  } finally {
    await mock.stop();
  }
});

test("outputs.<own id> in the switch is the previous output, not the one just written", async () => {
  // The switch runs after setTaskOutput, so without the engine's shadow outputs.t would be
  // the value self.output holds and this process would route to "same".
  const name = `scope_switch_${crypto.randomUUID()}`;
  await register({
    name,
    tasks: [
      {
        id: "t",
        action: { type: "delay", for: "10ms" },
        output: { i: "$: (self.previous.i ?? 0) + 1" },
        switch: [
          { case: "self.output.i < 2", goto: "$t" },
          { case: "outputs.t.i == self.output.i", goto: "$same" },
          { goto: "$differs" },
        ],
      },
      { id: "same", output: { verdict: "the new output" }, switch: "end" },
      { id: "differs", output: { verdict: "the previous output" }, switch: "end" },
    ],
    output: { verdict: "$: outputs.same.verdict ?? outputs.differs.verdict" },
  });

  const out = await runToOutput(name);
  expect(out.verdict).toBe("the previous output");
});

test("self.previous drives a child task's input", async () => {
  // A child's input is evaluated before the child is spawned, which is the slot that had no
  // self at all. The child echoes what it was given, so the count can only climb if each run
  // read the run before it.
  const child = `scope_echo_${crypto.randomUUID()}`;
  await register({
    name: child,
    input_schema: { type: "object", properties: { x: { type: "integer" } }, required: ["x"] },
    tasks: [{ id: "e", action: { type: "delay", for: "5ms" }, output: "$: input.x", switch: "end" }],
    output: "$: outputs.e",
  });

  const name = `scope_childinput_${crypto.randomUUID()}`;
  await register({
    name,
    tasks: [
      {
        id: "acc",
        action: {
          type: "child",
          name: child,
          input: { x: "$: (self.previous.n ?? 0) + 1" },
          result_schema: { type: "integer" },
        },
        output: { n: "$: self.result" },
        switch: [{ case: "self.output.n < 3", goto: "$acc" }, { goto: "end" }],
      },
    ],
    output: { n: "$: outputs.acc.n" },
  });

  expect((await runToOutput(name)).n).toBe(3);
});

test("self.previous is readable from an on_error rule", async () => {
  // on_error runs after a failed action: the task has no result, but the output of its last
  // SUCCESSFUL run is untouched, so the rule's case can read it. Two mocks give the task one
  // 200 and then a 500 — the only way to reach a failure with a previous output in hand.
  const ok = await startMockService(0, { response: { ok: true } });
  const bad = await startMockService(0, { response: { bad: true }, statusCode: 500 });
  try {
    const name = `scope_onerror_${crypto.randomUUID()}`;
    await register({
      name,
      tasks: [
        {
          id: "t",
          action: {
            type: "fetch",
            // First run has no previous output and hits the healthy mock; the second, driven
            // by the output the first produced, hits the failing one.
            url: `\${ (self.previous.n ?? 0) < 1 ? 'http://localhost:${ok.port}/x' : 'http://localhost:${bad.port}/x' }`,
            responses: { 200: { type: "object", properties: { ok: { type: "boolean" } } } },
          },
          on_error: [
            // The claim: this case reads the output of the run that succeeded.
            { code: ["http.500"], case: "(self.previous.n ?? 0) == 1", goto: "$caught" },
            { code: ["http.500"], goto: "$uncaught" },
          ],
          output: { n: "$: (self.previous.n ?? 0) + 1" },
          switch: [{ case: "self.output.n < 2", goto: "$t" }, { goto: "end" }],
        },
        { id: "caught", output: { verdict: "on_error saw the previous output" }, switch: "end" },
        { id: "uncaught", output: { verdict: "on_error saw nothing" }, switch: "end" },
      ],
      output: { verdict: "$: outputs.caught.verdict ?? outputs.uncaught.verdict" },
    });

    const out = await runToOutput(name);
    expect(out.verdict).toBe("on_error saw the previous output");
    expect(bad.requestCount()).toBe(1);
  } finally {
    await ok.stop();
    await bad.stop();
  }
});

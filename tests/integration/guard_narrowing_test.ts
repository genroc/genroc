import { expect, test } from "vitest";
import { client, startMockService, waitForInstance } from "../helpers/client.ts";

// A `switch` case's proof travels the edge it selects, so the task it routes to can read what
// was proved without a `?? default` that could never evaluate. End to end: the definition only
// REGISTERS because the proof travels, and the instance then runs the arithmetic the proof
// made legal. specs/guard-narrowing.md.

const NULLABLE_TOTAL = {
  type: "object",
  properties: { total: { type: ["integer", "null"] } },
  required: ["total"],
} as const;

// price exports a nullable `total`; charge reads it as a number, which is only typeable
// because the case that routed there proved it non-null.
function definition(name: string, port: number, guard: string) {
  return {
    name,
    tasks: [
      {
        id: "price",
        action: {
          type: "fetch" as const,
          url: `http://localhost:${port}/price`,
          method: "get",
          responses: { 200: NULLABLE_TOTAL },
          timeout: 2000,
        },
        output: { total: "$: self.result.total" },
        switch: [
          { case: guard, goto: "$charge" },
          { goto: "$skipped" },
        ],
      },
      {
        id: "charge",
        action: {
          type: "fetch" as const,
          url: `http://localhost:${port}/price`,
          method: "post",
          body: { amount: "$: outputs.price.total * 2" },
          timeout: 2000,
        },
        output: { charged: "$: outputs.price.total * 2" },
        switch: "end",
      },
      { id: "skipped", output: { charged: 0 }, switch: "end" },
    ],
    output: { charged: "$: outputs.charge.charged ?? 0" },
  };
}

test("a switch case's proof lets the next task use the value, and the instance runs", async () => {
  const svc = await startMockService(0, { statusCode: 200, response: { total: 21 } });
  const name = `guard_narrow_ok_${crypto.randomUUID()}`;

  const { error: putErr } = await client.PUT("/definitions", {
    body: definition(name, svc.port, "self.output.total != null") as never,
  });
  expect(putErr, `the case proved total is not null: ${JSON.stringify(putErr)}`).toBeUndefined();

  const { data: started } = await client.POST("/instances", { body: { process: name } });
  expect(await waitForInstance(started!.id, 10_000)).toBe("completed");

  const { data } = await client.GET("/instances/{id}", { params: { path: { id: started!.id } } });
  expect((data as { output?: { charged?: number } })?.output?.charged).toBe(42);

  svc.stop();
});

// The mirror, and the reason the feature has to be exact: the same definition with the guard
// pointing the other way reads a value that route proved to be NULL, and must be refused at
// registration rather than failing at runtime with an uncatchable engine.expression.
test("the opposite proof is refused at registration", async () => {
  const svc = await startMockService(0, { statusCode: 200, response: { total: 21 } });
  const name = `guard_narrow_bad_${crypto.randomUUID()}`;

  const { error } = await client.PUT("/definitions", {
    body: definition(name, svc.port, "self.output.total == null") as never,
  });
  expect(error, "that case proves total IS null").toBeDefined();
  expect(JSON.stringify(error)).toContain("non-nullable");

  svc.stop();
});

// The guard-clause shape: handle the bad case, fall through with no `case:`. All of its
// narrowing comes from the NEGATION of the case above it, which the spec weights heaviest.
test("falling past a null check narrows the fall-through, and runs", async () => {
  const svc = await startMockService(0, { statusCode: 200, response: { total: 21 } });
  const name = `guard_narrow_negation_${crypto.randomUUID()}`;

  const def = definition(name, svc.port, "self.output.total != null");
  // Reverse it into the guard-clause form: the null case leaves first, the rest falls through.
  def.tasks[0].switch = [
    { case: "self.output.total == null", goto: "$skipped" },
    { goto: "$charge" },
  ];

  const { error: putErr } = await client.PUT("/definitions", { body: def as never });
  expect(putErr, `reaching the fall-through means the null check failed: ${JSON.stringify(putErr)}`).toBeUndefined();

  const { data: started } = await client.POST("/instances", { body: { process: name } });
  expect(await waitForInstance(started!.id, 10_000)).toBe("completed");

  const { data } = await client.GET("/instances/{id}", { params: { path: { id: started!.id } } });
  expect((data as { output?: { charged?: number } })?.output?.charged).toBe(42);

  svc.stop();
});

// The runtime pairing for the null path: the guard-clause form must still route a real null
// away rather than reaching the arithmetic the narrowing made legal.
test("a null at runtime takes the other edge and the process still completes", async () => {
  const svc = await startMockService(0, { statusCode: 200, response: { total: null } });
  const name = `guard_narrow_nullrun_${crypto.randomUUID()}`;

  const def = definition(name, svc.port, "self.output.total != null");
  def.tasks[0].switch = [
    { case: "self.output.total == null", goto: "$skipped" },
    { goto: "$charge" },
  ];

  await client.PUT("/definitions", { body: def as never });
  const { data: started } = await client.POST("/instances", { body: { process: name } });
  expect(await waitForInstance(started!.id, 10_000)).toBe("completed");

  const { data } = await client.GET("/instances/{id}", { params: { path: { id: started!.id } } });
  expect((data as { output?: { charged?: number } })?.output?.charged).toBe(0);

  svc.stop();
});

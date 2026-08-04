import { expect, test } from "vitest";
import { client, waitForInstance } from "../helpers/client.ts";

const proc = `log_io_${crypto.randomUUID()}`;

async function defineProc() {
  await client.PUT("/definitions", {
    body: {
      name: proc,
      input_schema: {
        type: "object",
        properties: { name: { type: "string" } },
        required: ["name"],
      },
      output: { greeting: "$: input.name" },
      tasks: [{ id: "work", switch: [{ goto: "end" }] }],
    },
  });
}

// The audit trail bookends a process: inst_created carries the process input,
// inst_completed carries the final output (the definition's output projection).
test("logs — inst_created carries input, inst_completed carries output", async () => {
  await defineProc();
  const { data: started } = await client.POST("/instances", {
    body: { process: proc, input: { name: "Sam" } },
  });
  const id = started!.id;
  await waitForInstance(id);

  // order=asc: the endpoint sorts newest-first like every list, and the last assertion
  // here is about which event opens the trail.
  const { data, error } = await client.GET("/instances/{id}/logs", {
    params: { path: { id }, query: { limit: 100, order: "asc" } },
  });
  expect(error).toBeUndefined();
  const items = data!.items ?? [];

  // data is the engine's compact JSON marshalling of the value, stored raw.
  const created = items.find((l) => l.event === "inst_created");
  expect(created).toBeDefined();
  expect(created!.data).toBe('{"name":"Sam"}');

  const completed = items.find((l) => l.event === "inst_completed");
  expect(completed).toBeDefined();
  expect(completed!.data).toBe('{"greeting":"Sam"}');

  // inst_created is the first event in the trail.
  expect(items[0]?.event).toBe("inst_created");
});

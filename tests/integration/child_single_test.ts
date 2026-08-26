import { expect, test } from "vitest";
import { client, waitForInstance } from "../helpers/client.ts";

// The single "child" action runs one named child and exposes its output DIRECTLY as
// self.result — unwrapped, unlike child_map's keyed object. This is the poller-style case:
// a task that delegates to exactly one child and wants its result verbatim.
test("child — result is the child's output unwrapped (not keyed)", async () => {
  const uid = crypto.randomUUID().slice(0, 8);
  const leaf = `single_leaf_${uid}`;
  const parent = `single_parent_${uid}`;

  // Leaf echoes its input number back as { value: n }.
  await client.PUT("/definitions", {
    body: {
      name: leaf,
      input_schema: {
        type: "object",
        properties: { n: { type: "integer" } },
        required: ["n"],
      },
      tasks: [{ id: "done", switch: [{ goto: "end" }] }],
      output: { value: "$: input.n" },
    },
  });

  await client.PUT("/definitions", {
    body: {
      name: parent,
      input_schema: {
        type: "object",
        properties: { x: { type: "integer" } },
        required: ["x"],
      },
      tasks: [
        {
          id: "run",
          action: {
            type: "child" as const,
            name: leaf,
            input: { n: "$: input.x" },
            result_schema: {
              type: "object",
              properties: { value: { type: "integer" } },
              required: ["value"],
            },
          },
          // self.result is the child's output directly — a child_map here would need
          // self.result.<key>, which is exactly the difference being asserted.
          output: "$: self.result",
          switch: [{ goto: "end" }],
        },
      ],
      output: { got: "$: outputs.run" },
    },
  });

  const { data, error } = await client.POST("/instances", {
    body: { process: parent, input: { x: 21 } },
  });
  expect(error).toBeUndefined();

  expect(await waitForInstance(data!.id, 10_000)).toBe("completed");

  const { data: inst } = await client.GET("/instances/{id}/detail", {
    params: { path: { id: data!.id } },
  });
  // Unwrapped: output.got IS the child output { value: 21 }, no intermediate key.
  expect((inst?.state?.output as any)?.got).toEqual({ value: 21 });
});

// The collected output is validated against result_schema, and a child whose output
// cannot satisfy it fails the parent on collect (surfacing the child's process name).
test("child — output validation failure fails the parent and names the child", async () => {
  const uid = crypto.randomUUID().slice(0, 8);
  const leaf = `single_bad_leaf_${uid}`;
  const parent = `single_strict_${uid}`;

  // No declared output, so registration's output-subset check is skipped; at runtime the
  // empty output cannot satisfy required: ["required_field"].
  await client.PUT("/definitions", {
    body: {
      name: leaf,
      tasks: [{ id: "done", switch: [{ goto: "end" }] }],
    },
  });

  await client.PUT("/definitions", {
    body: {
      name: parent,
      tasks: [
        {
          id: "run",
          action: {
            type: "child" as const,
            name: leaf,
            result_schema: {
              type: "object",
              properties: { required_field: { type: "string" } },
              required: ["required_field"],
            },
          },
          switch: [{ goto: "end" }],
        },
      ],
    },
  });

  const { data, error } = await client.POST("/instances", {
    body: { process: parent },
  });
  expect(error).toBeUndefined();

  expect(await waitForInstance(data!.id, 10_000)).toBe("failed");

  const { data: inst } = await client.GET("/instances/{id}/detail", {
    params: { path: { id: data!.id } },
  });
  expect(inst?.error_message).toContain(leaf);
});

// A single child that raises is the parent's to resolve via on_error, exactly like a
// child_map/child_list batch — the parent catches the raised code and completes.
test("child — parent on_error catches the child's raised code", async () => {
  const uid = crypto.randomUUID().slice(0, 8);
  const leaf = `single_raiser_${uid}`;
  const parent = `single_catcher_${uid}`;

  await client.PUT("/definitions", {
    body: {
      name: leaf,
      tasks: [
        {
          id: "boom",
          switch: [{ raise: { code: "card_declined", message: "declined" } }],
        },
      ],
    },
  });

  await client.PUT("/definitions", {
    body: {
      name: parent,
      tasks: [
        {
          id: "run",
          action: { type: "child" as const, name: leaf },
          on_error: [{ code: ["card_declined"], goto: "end" }],
          switch: [{ goto: "end" }],
        },
      ],
    },
  });

  const { data, error } = await client.POST("/instances", {
    body: { process: parent },
  });
  expect(error).toBeUndefined();

  // on_error routes the raised code to end → the parent completes rather than failing.
  expect(await waitForInstance(data!.id, 10_000)).toBe("completed");
});

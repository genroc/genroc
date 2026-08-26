import { expect, test } from "vitest";
import { client, waitForInstance } from "../helpers/client.ts";

// map reshapes an array inside an expression, so a child_list can fan out over a
// shape the caller never sent. Before map, `over` could only pass an input array
// through unchanged, and Shape cannot build an array structurally at all.
test("map — child_list fans out over a reshaped array", async () => {
  const uid = crypto.randomUUID().slice(0, 8);
  const leaf = `map_leaf_${uid}`;
  const parent = `map_parent_${uid}`;

  // The leaf takes {sku, qty}: a different shape from the parent's rows.
  await client.PUT("/definitions", {
    body: {
      name: leaf,
      input_schema: {
        type: "object",
        properties: { sku: { type: "string" }, qty: { type: "integer" } },
        required: ["sku", "qty"],
      },
      tasks: [{ id: "done", switch: [{ goto: "end" }] }],
      output: { line: "${ input.sku }x${ input.qty }" },
    },
  });

  await client.PUT("/definitions", {
    body: {
      name: parent,
      input_schema: {
        type: "object",
        properties: {
          rows: {
            type: "array",
            items: {
              type: "object",
              properties: {
                code: { type: "string" },
                count: { type: "integer" },
              },
              required: ["code", "count"],
            },
          },
        },
        required: ["rows"],
      },
      tasks: [
        {
          id: "spread",
          action: {
            type: "child_list" as const,
            name: leaf,
            // Rename code->sku and count->qty, and derive a value per element.
            over: "$: map(input.rows, r => {sku: r.code, qty: r.count + 1})",
            result_schema: {
              type: "object",
              properties: { line: { type: "string" } },
              required: ["line"],
            },
          },
          switch: [{ goto: "end" }],
          output: { lines: "$: map(self.result, c => c.line)" },
        },
      ],
      output: { lines: "$: outputs.spread.lines" },
    },
  });

  const { data: startData, error: startError } = await client.POST("/instances", {
    body: {
      process: parent,
      input: {
        rows: [
          { code: "AAA", count: 1 },
          { code: "BBB", count: 4 },
        ],
      },
    },
  });
  expect(startError).toBeUndefined();
  const id = startData!.id;

  expect(await waitForInstance(id, 15_000)).toBe("completed");

  const { data } = await client.GET("/instances/{id}/detail", { params: { path: { id } } });
  // count+1 was computed per element, and code was renamed to sku.
  expect((data?.state?.output as any)?.lines).toEqual(["AAAx2", "BBBx5"]);
});

// The registration-time type check is the point of a statically inferred map: a
// body that reads a field the element does not have must be rejected on upload,
// not produce nulls at runtime.
test("map — a bad field in the lambda body is rejected at registration", async () => {
  const uid = crypto.randomUUID().slice(0, 8);
  const res = await client.PUT("/definitions", {
    body: {
      name: `map_badfield_${uid}`,
      input_schema: {
        type: "object",
        properties: {
          rows: {
            type: "array",
            items: {
              type: "object",
              properties: { code: { type: "string" } },
              required: ["code"],
            },
          },
        },
        required: ["rows"],
      },
      tasks: [{ id: "done", switch: [{ goto: "end" }] }],
      output: { bad: "$: map(input.rows, r => r.code + r.nope)" },
    },
  });
  expect(res.response.status).toBe(400);
});

// A nullable source would fail at runtime, so it is a registration error with a
// hint; adding the ?? default makes the same definition register.
test("map — a nullable source is rejected, and ?? [] fixes it", async () => {
  const uid = crypto.randomUUID().slice(0, 8);
  const inputSchema = {
    type: "object",
    properties: {
      rows: {
        type: ["array", "null"],
        items: {
          type: "object",
          properties: { code: { type: "string" } },
          required: ["code"],
        },
      },
    },
    required: ["rows"],
  };

  const bad = await client.PUT("/definitions", {
    body: {
      name: `map_nullsrc_${uid}`,
      input_schema: inputSchema,
      tasks: [{ id: "done", switch: [{ goto: "end" }] }],
      output: { codes: "$: map(input.rows, r => r.code)" },
    },
  });
  expect(bad.response.status).toBe(400);

  const good = await client.PUT("/definitions", {
    body: {
      name: `map_nullsrc_ok_${uid}`,
      input_schema: inputSchema,
      tasks: [{ id: "done", switch: [{ goto: "end" }] }],
      output: { codes: "$: map(input.rows ?? [], r => r.code)" },
    },
  });
  expect(good.response.status).toBe(200);
});

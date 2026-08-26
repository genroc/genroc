import { expect, test } from "vitest";
import { client, listAllInstances, waitForInstance } from "../helpers/client.ts";

// A leaf process that doubles its input number. It delays INVERSELY to n so that
// higher-index children finish first — forcing children to complete out of input
// order, which is exactly what the _spawn_index-ordered collection must survive.
async function defineDoubler(name: string) {
  await client.PUT("/definitions", {
    body: {
      name,
      input_schema: {
        type: "object",
        properties: { n: { type: "integer" } },
        required: ["n"],
      },
      tasks: [
        {
          id: "wait",
          action: { type: "delay" as const, for: "$: (6 - input.n) * 30" },
          switch: [{ goto: "next" }],
        },
        { id: "done", switch: [{ goto: "end" }] },
      ],
      output: { doubled: "$: input.n * 2", original: "$: input.n" },
    },
  });
}

test("child_list — result is an array in input order despite out-of-order completion", async () => {
  const uid = crypto.randomUUID().slice(0, 8);
  const leaf = `cfa_leaf_${uid}`;
  const parent = `cfa_parent_${uid}`;
  await defineDoubler(leaf);

  await client.PUT("/definitions", {
    body: {
      name: parent,
      input_schema: {
        type: "object",
        properties: {
          items: {
            type: "array",
            items: {
              type: "object",
              properties: { n: { type: "integer" } },
              required: ["n"],
            },
          },
        },
        required: ["items"],
      },
      tasks: [
        {
          id: "spread",
          action: {
            type: "child_list" as const,
            name: leaf,
            over: "$: input.items",
            result_schema: {
              type: "object",
              properties: { doubled: { type: "number" }, original: { type: "number" } },
              required: ["doubled", "original"],
            },
          },
          output: "$: self.result",
          switch: [{ goto: "end" }],
        },
      ],
      output: { results: "$: outputs.spread" },
    },
  });

  const items = [{ n: 1 }, { n: 2 }, { n: 3 }, { n: 4 }, { n: 5 }];
  const { data: startData, error: startError } = await client.POST("/instances", {
    body: { process: parent, input: { items } },
  });
  expect(startError).toBeUndefined();
  const id = startData!.id;

  expect(await waitForInstance(id, 15_000)).toBe("completed");

  const { data } = await client.GET("/instances/{id}/detail", { params: { path: { id } } });
  const results = (data?.state?.output as any)?.results;
  // Order MUST match the input array, not the (reversed) completion order.
  expect(results).toEqual([
    { doubled: 2, original: 1 },
    { doubled: 4, original: 2 },
    { doubled: 6, original: 3 },
    { doubled: 8, original: 4 },
    { doubled: 10, original: 5 },
  ]);

  // One child spawned per element.
  const children = (await listAllInstances()).filter((i) => i.process === leaf);
  expect(children).toHaveLength(items.length);
  expect(children.every((i) => i.status === "completed")).toBe(true);
});

test("child_list — empty array spawns no children and yields an empty array", async () => {
  const uid = crypto.randomUUID().slice(0, 8);
  const leaf = `cfa_empty_leaf_${uid}`;
  const parent = `cfa_empty_parent_${uid}`;
  await defineDoubler(leaf);

  await client.PUT("/definitions", {
    body: {
      name: parent,
      input_schema: {
        type: "object",
        properties: {
          items: {
            type: "array",
            items: {
              type: "object",
              properties: { n: { type: "integer" } },
              required: ["n"],
            },
          },
        },
        required: ["items"],
      },
      tasks: [
        {
          id: "spread",
          action: {
            type: "child_list" as const,
            name: leaf,
            over: "$: input.items",
            result_schema: {
              type: "object",
              properties: { doubled: { type: "number" }, original: { type: "number" } },
              required: ["doubled", "original"],
            },
          },
          output: "$: self.result",
          switch: [{ goto: "end" }],
        },
      ],
      output: { results: "$: outputs.spread" },
    },
  });

  const { data: startData } = await client.POST("/instances", {
    body: { process: parent, input: { items: [] } },
  });
  const id = startData!.id;

  expect(await waitForInstance(id, 10_000)).toBe("completed");

  const { data } = await client.GET("/instances/{id}/detail", { params: { path: { id } } });
  expect((data?.state?.output as any)?.results).toEqual([]);

  // No child of the leaf process was ever created.
  const children = (await listAllInstances()).filter((i) => i.process === leaf);
  expect(children).toHaveLength(0);
});

test("child_list — registration rejects an element type incompatible with the child input", async () => {
  const uid = crypto.randomUUID().slice(0, 8);
  const leaf = `cfa_reject_leaf_${uid}`;
  const parent = `cfa_reject_parent_${uid}`;
  await defineDoubler(leaf); // wants { n: integer }

  // over is an array of integers, but the child input is an object → not a subset.
  const { error } = await client.PUT("/definitions", {
    body: {
      name: parent,
      input_schema: {
        type: "object",
        properties: { nums: { type: "array", items: { type: "integer" } } },
        required: ["nums"],
      },
      tasks: [
        {
          id: "spread",
          action: {
            type: "child_list" as const,
            name: leaf,
            over: "$: input.nums",
          },
          switch: [{ goto: "end" }],
        },
      ],
    },
  });
  expect(error).toBeDefined();
  expect(JSON.stringify(error)).toContain("not compatible");
});

// ── type-inference / validation errors on `over` (rejected at registration) ──────

// Builds a child_list parent with the given input_schema + over expression and
// returns the PUT error (undefined if it unexpectedly succeeds).
async function overError(inputSchema: unknown, over: string, leaf: string) {
  const parent = `cl_over_parent_${crypto.randomUUID().slice(0, 8)}`;
  const { error } = await client.PUT("/definitions", {
    body: {
      name: parent,
      input_schema: inputSchema as never,
      tasks: [
        {
          id: "spread",
          action: { type: "child_list" as const, name: leaf, over },
          switch: [{ goto: "end" }],
        },
      ],
    },
  });
  return error;
}

test("child_list — rejects `over` that does not evaluate to an array", async () => {
  const leaf = `cl_scalar_leaf_${crypto.randomUUID().slice(0, 8)}`;
  await defineDoubler(leaf);
  // input.n is an integer, not an array.
  const error = await overError(
    { type: "object", properties: { n: { type: "integer" } }, required: ["n"] },
    "$: input.n",
    leaf,
  );
  expect(error).toBeDefined();
  expect(JSON.stringify(error)).toContain("array");
});

test("child_list — rejects `over` that may be null", async () => {
  const leaf = `cl_nullable_leaf_${crypto.randomUUID().slice(0, 8)}`;
  await defineDoubler(leaf);
  // `items` is present in properties but NOT required → input.items is array|null.
  const error = await overError(
    {
      type: "object",
      properties: {
        items: {
          type: "array",
          items: { type: "object", properties: { n: { type: "integer" } }, required: ["n"] },
        },
      },
    },
    "$: input.items",
    leaf,
  );
  expect(error).toBeDefined();
  expect(JSON.stringify(error)).toContain("null");
});

test("child_list — rejects `over` array with no declared element type", async () => {
  const leaf = `cl_untyped_leaf_${crypto.randomUUID().slice(0, 8)}`;
  await defineDoubler(leaf); // has an input_schema, so the element must be checked
  // `items` is an array but declares no item schema → element type is unknown.
  const error = await overError(
    { type: "object", properties: { items: { type: "array" } }, required: ["items"] },
    "$: input.items",
    leaf,
  );
  expect(error).toBeDefined();
  expect(JSON.stringify(error)).toContain("element type");
});

test("child_list — rejects `over` referencing an unknown field", async () => {
  const leaf = `cl_unknown_leaf_${crypto.randomUUID().slice(0, 8)}`;
  await defineDoubler(leaf);
  const error = await overError(
    { type: "object", properties: { items: { type: "array", items: { type: "object" } } }, required: ["items"] },
    "$: input.nope",
    leaf,
  );
  expect(error).toBeDefined();
});

// ── runtime behaviour ───────────────────────────────────────────────────────────

test("child_list — a result_schema the child's output can't satisfy is rejected at registration", async () => {
  const uid = crypto.randomUUID().slice(0, 8);
  const leaf = `cl_mismatch_leaf_${uid}`;
  const parent = `cl_mismatch_parent_${uid}`;
  await defineDoubler(leaf); // produces { doubled, original }

  // result_schema requires a field the child's output type never produces. This is a
  // *static* incompatibility (the child's output type is not a subset of result_schema),
  // so it is caught at registration rather than surfacing per-item at collect.
  const { error } = await client.PUT("/definitions", {
    body: {
      name: parent,
      input_schema: {
        type: "object",
        properties: {
          items: {
            type: "array",
            items: { type: "object", properties: { n: { type: "integer" } }, required: ["n"] },
          },
        },
        required: ["items"],
      },
      tasks: [
        {
          id: "spread",
          action: {
            type: "child_list" as const,
            name: leaf,
            over: "$: input.items",
            result_schema: {
              type: "object",
              properties: { missing: { type: "string" } },
              required: ["missing"],
            },
          },
          switch: [{ goto: "end" }],
        },
      ],
    },
  });

  expect(error).toBeDefined();
  expect(JSON.stringify(error)).toContain("result_schema");
});

test("child_list — the collected array is usable downstream by index", async () => {
  const uid = crypto.randomUUID().slice(0, 8);
  const leaf = `cl_index_leaf_${uid}`;
  const parent = `cl_index_parent_${uid}`;
  await defineDoubler(leaf);

  await client.PUT("/definitions", {
    body: {
      name: parent,
      input_schema: {
        type: "object",
        properties: {
          items: {
            type: "array",
            items: { type: "object", properties: { n: { type: "integer" } }, required: ["n"] },
          },
        },
        required: ["items"],
      },
      tasks: [
        {
          id: "spread",
          action: {
            type: "child_list" as const,
            name: leaf,
            over: "$: input.items",
            result_schema: {
              type: "object",
              properties: { doubled: { type: "number" }, original: { type: "number" } },
              required: ["doubled", "original"],
            },
          },
          output: "$: self.result",
          switch: [{ goto: "end" }],
        },
      ],
      // Read specific elements of the collected array by index.
      output: {
        first: "$: outputs.spread[0].doubled",
        third: "$: outputs.spread[2].doubled",
      },
    },
  });

  const { data: startData } = await client.POST("/instances", {
    body: { process: parent, input: { items: [{ n: 1 }, { n: 2 }, { n: 3 }] } },
  });
  const id = startData!.id;
  expect(await waitForInstance(id, 10_000)).toBe("completed");

  const { data } = await client.GET("/instances/{id}/detail", { params: { path: { id } } });
  const output = data?.state?.output as any;
  expect(output.first).toBe(2);
  expect(output.third).toBe(6);
});

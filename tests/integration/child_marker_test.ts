import { expect, test } from "vitest";
import { client, spliceObjects, waitForInstance } from "../helpers/client.ts";

/**
 * The blob as the CHILD stored it. Asserted on the child rather than routed back through the
 * parent's output: the round trip needs result-schema plumbing that has nothing to do with the
 * boundary under test, and the child's own row is where a marker would have landed.
 */
async function childInput(parentID: string): Promise<unknown> {
  const { data: trail } = await client.GET("/instances/{id}/logs", {
    params: { path: { id: parentID }, query: { limit: 200, recursive: true } },
  });
  const kid = (trail!.items ?? []).find((l) => l.instance !== parentID)?.instance;
  expect(kid, "the parent spawned a child").toBeDefined();
  const { data } = await client.GET("/instances/{id}", { params: { path: { id: kid! } } });
  await spliceObjects(data);
  // eslint-disable-next-line @typescript-eslint/no-explicit-any
  return (data!.context as any).input.blob;
}

const BLOB = "B".repeat(20 * 1024);

/**
 * A child's input is a boundary a reference must not cross.
 *
 * Once an expression can COPY a reference instead of loading it (specs/lazy-context.md), a
 * parent that passes a slot straight into a child hands it a marker. Two things then break, and
 * the first is loud: the child's input is CONFORMED, and a conform cannot inspect or normalize a
 * value it would have to load to see — `expected type string, got *model.ObjectRef`. The second
 * is silent: the value lands on the child's row, and a claim written only for objects that write
 * produced would leave the child referencing content it never held.
 *
 * All three child types cross the same boundary, so all three are covered here.
 */

async function defineChild(name: string) {
  await client.PUT("/definitions", {
    body: {
      name,
      input_schema: {
        type: "object",
        properties: { blob: { type: "string" } },
        required: ["blob"],
      },
      output: { got: "$: input.blob" },
      tasks: [{ id: "leaf", switch: [{ goto: "end" }] }],
    },
  });
}

// `"$: input"` copies the WHOLE slot — a copy position, so the marker travels rather than the
// value. Naming a field (`input.blob`) would read through and materialize, missing the case.
const COPY_WHOLE_SLOT = "$: input";

test("child — a copied input slot is materialized at the boundary", async () => {
  const child = `cmk_child_${crypto.randomUUID()}`;
  const parent = `cmk_parent_${crypto.randomUUID()}`;
  await defineChild(child);
  await client.PUT("/definitions", {
    body: {
      name: parent,
      input_schema: {
        type: "object",
        properties: { blob: { type: "string" } },
        required: ["blob"],
      },
      tasks: [
        {
          id: "spawn",
          action: { type: "child" as const, name: child, input: COPY_WHOLE_SLOT },
          switch: [{ goto: "end" }],
        },
      ],
      // eslint-disable-next-line @typescript-eslint/no-explicit-any
    } as any,
  });

  const { data: started } = await client.POST("/instances", {
    body: { process: parent, input: { blob: BLOB } },
  });
  const id = started!.id;
  expect(await waitForInstance(id, 10_000)).toBe("completed");

  expect(await childInput(id)).toBe(BLOB);
});

test("child_map — a copied input slot is materialized at the boundary", async () => {
  const child = `cmk_m_child_${crypto.randomUUID()}`;
  const parent = `cmk_m_parent_${crypto.randomUUID()}`;
  await defineChild(child);
  await client.PUT("/definitions", {
    body: {
      name: parent,
      input_schema: {
        type: "object",
        properties: { blob: { type: "string" } },
        required: ["blob"],
      },
      tasks: [
        {
          id: "spawn",
          action: {
            type: "child_map" as const,
            children: { out: { name: child, input: COPY_WHOLE_SLOT } },
          },
          switch: [{ goto: "end" }],
        },
      ],
      // eslint-disable-next-line @typescript-eslint/no-explicit-any
    } as any,
  });

  const { data: started } = await client.POST("/instances", {
    body: { process: parent, input: { blob: BLOB } },
  });
  const id = started!.id;
  expect(await waitForInstance(id, 10_000)).toBe("completed");

  expect(await childInput(id)).toBe(BLOB);
});

test("child_list — copied fan-out elements are materialized at the boundary", async () => {
  const child = `cmk_l_child_${crypto.randomUUID()}`;
  const parent = `cmk_l_parent_${crypto.randomUUID()}`;
  await defineChild(child);
  await client.PUT("/definitions", {
    body: {
      name: parent,
      input_schema: {
        type: "object",
        properties: { blob: { type: "string" } },
        required: ["blob"],
      },
      // `over` builds the array from a copied slot, so each element carries the marker.
      tasks: [
        {
          id: "spawn",
          action: { type: "child_list" as const, name: child, over: "$: [input]" },
          switch: [{ goto: "end" }],
        },
      ],
      // eslint-disable-next-line @typescript-eslint/no-explicit-any
    } as any,
  });

  const { data: started } = await client.POST("/instances", {
    body: { process: parent, input: { blob: BLOB } },
  });
  const id = started!.id;
  expect(await waitForInstance(id, 10_000)).toBe("completed");

  expect(await childInput(id)).toBe(BLOB);
});

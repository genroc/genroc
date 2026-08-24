import { expect, test } from "vitest";
import { client, fetchObject, objectAt, spliceObjects, waitForInstance } from "../helpers/client.ts";

const proc = `big_values_${crypto.randomUUID()}`;

// A value larger than the externalization threshold (8 KiB) so it is stored in the
// object store rather than inline on the instance row.
const BLOB = "B".repeat(20 * 1024);

async function defineProc() {
  await client.PUT("/definitions", {
    body: {
      name: proc,
      input_schema: {
        type: "object",
        properties: { blob: { type: "string" } },
        required: ["blob"],
      },
      // The output reads the (externalized) input, exercising lazy resolution through
      // the engine's output projection.
      output: { echo: "$: input.blob" },
      tasks: [{ id: "work", switch: [{ goto: "end" }] }],
    },
  });
}

// By default a large input/output slot is NOT pulled out of the object store: the
// detail view returns a lightweight {ref, size} reference instead of the value.
test("big values are returned as references by default", async () => {
  await defineProc();
  const { data: started } = await client.POST("/instances", {
    body: { process: proc, input: { blob: BLOB } },
  });
  const id = started!.id;
  await waitForInstance(id);

  const { data, error } = await client.GET("/instances/{id}", {
    params: { path: { id } },
  });
  expect(error).toBeUndefined();
  // The big input and the big computed output are LISTED, and absent from the data — nothing
  // in the context can be mistaken for a reference, because no marker is left behind.
  expect((data!.context as any).input).toBeUndefined();
  expect((data!.context as any).output).toBeUndefined();
  const input = objectAt(data, ["context", "input"]);
  expect(input, "the big input is listed").toBeDefined();
  expect(input!.size).toBeGreaterThan(BLOB.length);
  expect(objectAt(data, ["context", "output"]), "the big output is listed").toBeDefined();
});

// The recipient fetches what it wants and puts it back. The server never materializes a whole
// context on request: that put an unbounded response behind one query parameter.
test("big values are spliced back by the recipient, not by the server", async () => {
  await defineProc();
  const { data: started } = await client.POST("/instances", {
    body: { process: proc, input: { blob: BLOB } },
  });
  const id = started!.id;
  await waitForInstance(id);

  const { data, error } = await client.GET("/instances/{id}", { params: { path: { id } } });
  expect(error).toBeUndefined();
  // The slots are ABSENT, not markers: nothing in the data can be mistaken for a reference.
  expect((data!.context as any).input?.blob).toBeUndefined();
  expect(objectAt(data, ["context", "input"]), "input is listed as an object").toBeDefined();

  await spliceObjects(data);
  expect((data!.context as any).input.blob).toBe(BLOB);
  expect((data!.context as any).output.echo).toBe(BLOB);
});

// A large log payload is externalized: the entry carries no inline data, and the response's
// objects section names where it belongs. A log entry is not a special kind of response.
test("large log payloads are externalized and fetchable", async () => {
  await defineProc();
  const { data: started } = await client.POST("/instances", {
    body: { process: proc, input: { blob: BLOB } },
  });
  const id = started!.id;
  await waitForInstance(id);

  const { data, error } = await client.GET("/instances/{id}/logs", {
    params: { path: { id }, query: { limit: 100 } },
  });
  expect(error).toBeUndefined();
  const completed = (data!.items ?? []).find(
    (l) => l.event === "inst_completed",
  );
  expect(completed).toBeDefined();
  // Too big to carry inline: no data on the entry, and the entry lists it instead — at a path
  // rooted at the ENTRY, so accumulating pages or reversing rows cannot invalidate it.
  expect(completed!.data).toBeFalsy();
  const listed = objectAt(completed, ["data"]);
  expect(listed, "the externalized log payload is listed by its entry").toBeDefined();

  // Fetched by content hash, from the one endpoint that serves objects.
  expect(await fetchObject(listed!.ref)).toBe(JSON.stringify({ echo: BLOB }));
});

// Within one instance, only the slots that exceed the threshold are externalized: a
// small slot stays an inline value even when a sibling slot is a reference.
test("only oversized slots become references; small ones stay inline", async () => {
  const name = `mixed_slots_${crypto.randomUUID()}`;
  await client.PUT("/definitions", {
    body: {
      name,
      input_schema: {
        type: "object",
        properties: { blob: { type: "string" } },
        required: ["blob"],
      },
      // input is big (externalized); output is a small constant (stays inline).
      output: { ok: "done" },
      tasks: [{ id: "work", switch: [{ goto: "end" }] }],
    },
  });
  const { data: started } = await client.POST("/instances", {
    body: { process: name, input: { blob: BLOB } },
  });
  const id = started!.id;
  await waitForInstance(id);

  const { data, error } = await client.GET("/instances/{id}", {
    params: { path: { id } },
  });
  expect(error).toBeUndefined();
  // Big input → listed and absent; small output → carried inline, and NOT listed.
  expect((data!.context as any).input).toBeUndefined();
  expect(objectAt(data, ["context", "input"])).toBeDefined();
  expect((data!.context as any).output).toEqual({ ok: "done" });
  expect(objectAt(data, ["context", "output"])).toBeUndefined();
});

// A secret inside a LARGE (externalized) value. The slot is listed rather than carried, so a
// detail read leaks nothing whatever it holds — and fetching the object returns it in full,
// which is the documented consequence of redaction being a recording concern.
test("an externalized value is listed, and comes back whole when fetched", async () => {
  const name = `secret_big_ctx_${crypto.randomUUID()}`;
  const secret = "S".repeat(20 * 1024); // > threshold → the input slot is externalized
  await client.PUT("/definitions", {
    body: {
      name,
      input_schema: {
        type: "object",
        required: ["token"],
        properties: { token: { type: "string", secret: true } },
      },
      tasks: [{ id: "work", switch: [{ goto: "end" }] }],
    },
  });
  const { data: started } = await client.POST("/instances", {
    body: { process: name, input: { token: secret } },
  });
  const id = started!.id;
  await waitForInstance(id);

  // Default: the value is never loaded — a bare reference, no secret anywhere.
  const { data } = await client.GET("/instances/{id}", { params: { path: { id } } });
  // The slot is listed, not carried — so the detail read stays small whatever it holds.
  expect((data!.context as any).input).toBeUndefined();
  expect(objectAt(data, ["context", "input"])).toBeDefined();
  expect(JSON.stringify(data)).not.toContain("SSSSSSSSSS");

  // Fetched, it comes back IN FULL — secret included. Redaction is a recording concern, not a
  // read concern: it protects the server's stdout, where a value is read by someone who did not
  // ask for it, and an object endpoint addressed by content hash is not that.
  // specs/object-store.md §Redaction.
  await spliceObjects(data);
  expect((data!.context as any).input.token).toBe(secret);
});

// A subtree log's externalized payload was written by a child, not the queried root. That used
// to matter — the fetch was scoped to the owning instance — and no longer does: an object is
// addressed by its content hash, so a child's payload is listed and fetched like any other.
test("recursive + resolve inlines a child instance's externalized payload", async () => {
  const child = `recos_child_${crypto.randomUUID()}`;
  const parent = `recos_parent_${crypto.randomUUID()}`;
  await client.PUT("/definitions", {
    body: {
      name: child,
      input_schema: {
        type: "object",
        properties: { blob: { type: "string" } },
        required: ["blob"],
      },
      tasks: [{ id: "leaf", switch: [{ goto: "end" }] }],
    },
  });
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
            children: {
              out: { name: child, input: { blob: "$: input.blob" } },
            },
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

  // The child's inst_created log carries the big (externalized) input it received. The whole
  // response is kept, not just the entry: the objects section is a sibling of items, so finding
  // an entry's payload means knowing its index in the page.
  const childCreated = async () => {
    const { data: body } = await client.GET("/instances/{id}/logs", {
      params: { path: { id }, query: { limit: 200, recursive: true } },
    });
    const entry = (body!.items ?? []).find(
      (l) => l.event === "inst_created" && l.instance !== id,
    );
    return { entry, body };
  };

  const { entry: found } = await childCreated();
  expect(found, "the child's inst_created entry is present").toBeDefined();
  expect(found!.data, "its big payload is not carried inline").toBeFalsy();
  const childListed = objectAt(found, ["data"]);
  expect(childListed, "and the entry lists it instead").toBeDefined();
  expect(await fetchObject(childListed!.ref)).toBe(JSON.stringify({ blob: BLOB }));
});

// A big value passed into a child's input and returned in the child's output flows all
// the way back: the parent collects the child's (externalized) output, re-externalizes
// it into its own context, and exposes it as a reference by default / the full value
// under resolve. Exercises the collect path (child output → parent) with a large value.
test("a big value round-trips through a child's input and output back to the parent", async () => {
  const child = `bv_rt_child_${crypto.randomUUID()}`;
  const parent = `bv_rt_parent_${crypto.randomUUID()}`;
  await client.PUT("/definitions", {
    body: {
      name: child,
      input_schema: {
        type: "object",
        properties: { blob: { type: "string" } },
        required: ["blob"],
      },
      // The child returns the big value it received straight back in its output.
      output: { echo: "$: input.blob" },
      tasks: [{ id: "leaf", switch: [{ goto: "end" }] }],
    },
  });
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
            children: {
              out: {
                name: child,
                input: { blob: "$: input.blob" },
                result_schema: {
                  type: "object",
                  properties: { echo: { type: "string" } },
                  required: ["echo"],
                },
              },
            },
          },
          // Collect the child's (big) output into this task's output…
          output: "$: self.result.out",
          switch: [{ goto: "end" }],
        },
      ],
      // …and surface it again as the parent's own output.
      output: { echo: "$: outputs.spawn.echo" },
      // eslint-disable-next-line @typescript-eslint/no-explicit-any
    } as any,
  });

  const { data: started } = await client.POST("/instances", {
    body: { process: parent, input: { blob: BLOB } },
  });
  const id = started!.id;
  expect(await waitForInstance(id, 10_000)).toBe("completed");

  // Default: the collected child output and the parent's own output are both references.
  const { data: lazy, error } = await client.GET("/instances/{id}", {
    params: { path: { id } },
  });
  expect(error).toBeUndefined();
  expect(objectAt(lazy, ["context", "outputs", "spawn"])).toBeDefined();
  expect(objectAt(lazy, ["context", "output"])).toBeDefined();

  // Spliced: the big value is intact after the full parent → child → parent round-trip.
  await spliceObjects(lazy);
  expect((lazy!.context as any).outputs.spawn.echo).toBe(BLOB);
  expect((lazy!.context as any).output.echo).toBe(BLOB);
});

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
  // The LEAF is cut, not the whole slot: what is over the target is `blob`, and its wrapper
  // stays inline. Cutting the slot would fold any sibling in with it, which is what stopped
  // three runs of one script from sharing the script.
  expect((data!.context as any).input.blob).toBeUndefined();
  expect((data!.context as any).output.echo).toBeUndefined();
  const input = objectAt(data, ["context", "input", "blob"]);
  expect(input, "the big input leaf is listed").toBeDefined();
  expect(input!.size).toBeGreaterThan(BLOB.length - 10);
  expect(objectAt(data, ["context", "output", "echo"]), "the big output leaf is listed").toBeDefined();
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
  expect(objectAt(data, ["context", "input", "blob"]), "the input's big leaf is listed").toBeDefined();

  await spliceObjects(data);
  expect((data!.context as any).input.blob).toBe(BLOB);
  expect((data!.context as any).output.echo).toBe(BLOB);
});

// A log payload is cut exactly like a context slot: the oversized LEAF moves out, the shell
// stays inline, and the entry lists where it went. Same shape, and therefore the same object --
// a payload repeating a value the instance already externalized shares it instead of storing a
// second copy, which is what made three runs of one script cost three copies of the script.
test("large log payloads are cut per-leaf and share the instance's object", async () => {
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
  // The shell is carried, the leaf is listed -- at a path rooted at the ENTRY, so accumulating
  // pages or reversing rows cannot invalidate it.
  expect(completed!.data, "the shell around the cut leaf is still carried").toEqual({});
  const listed = objectAt(completed, ["data", "echo"]);
  expect(listed, "the oversized log LEAF is listed by its entry").toBeDefined();
  expect(await fetchObject(listed!.ref)).toBe(JSON.stringify(BLOB));

  // The same bytes the instance's own output externalized: one object, two claims.
  const { data: detail } = await client.GET("/instances/{id}", { params: { path: { id } } });
  const slot = objectAt(detail, ["context", "output", "echo"]);
  expect(listed!.ref, "the log shares the context slot's object rather than copying it").toBe(slot!.ref);

  // And the recipient splices an entry's section exactly as it splices the body's.
  await spliceObjects(data);
  expect((completed as { data?: unknown }).data).toEqual({ echo: BLOB });
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
  expect((data!.context as any).input.blob).toBeUndefined();
  expect(objectAt(data, ["context", "input", "blob"])).toBeDefined();
  expect((data!.context as any).output).toEqual({ ok: "done" });
  expect(objectAt(data, ["context", "output"])).toBeUndefined();
});

// A secret inside a LARGE (externalized) value. The slot is listed rather than carried, so a
// detail read leaks nothing whatever it holds — and fetching the object returns it in full,
// which is the documented consequence of redaction being a recording concern.
test("an externalized value is listed, and comes back whole when fetched", async () => {
  const name = `secret_big_ctx_${crypto.randomUUID()}`;
  const secret = "S".repeat(20 * 1024); // > threshold → the leaf is externalized
  await client.PUT("/definitions", {
    body: {
      name,
      input_schema: { type: "object", properties: { token: { type: "string" } } },
      tasks: [{ id: "t", output: { got: "$: input.token" }, switch: [{ goto: "end" }] }],
      output: "$: outputs.t",
    } as never,
  });
  const { data: started } = await client.POST("/instances", {
    body: { process: name, input: { token: secret } },
  });
  await waitForInstance(started!.id);

  const { data } = await client.GET("/instances/{id}", { params: { path: { id: started!.id } } });
  // Listed, not carried: a detail read stays small whatever the slot holds.
  expect((data!.context as any).input.token).toBeUndefined();
  expect(objectAt(data, ["context", "input", "token"])).toBeDefined();
  expect(JSON.stringify(data)).not.toContain("SSSSSSSSSS");

  // And it comes back whole when asked for. The API returns what happened; `secret: true` keeps
  // a value off the server's console and does nothing here. specs/object-store.md §Redaction.
  await spliceObjects(data);
  expect((data!.context as any).input.token).toBe(secret);
});

// A subtree log's externalized payload was written by a child, not the queried root. That used
// to matter -- the fetch was scoped to the owning instance -- and no longer does: an object is
// addressed by its content hash, so a child's payload is listed and fetched like any other.
test("a subtree log lists a child instance's externalized payload", async () => {
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
  expect(found!.data, "the shell around the cut leaf is carried inline").toEqual({});
  const childListed = objectAt(found, ["data", "blob"]);
  expect(childListed, "and the entry lists the oversized leaf instead").toBeDefined();
  expect(await fetchObject(childListed!.ref)).toBe(JSON.stringify(BLOB));
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
  expect(objectAt(lazy, ["context", "outputs", "spawn", "echo"])).toBeDefined();
  expect(objectAt(lazy, ["context", "output", "echo"])).toBeDefined();

  // Spliced: the big value is intact after the full parent → child → parent round-trip.
  await spliceObjects(lazy);
  expect((lazy!.context as any).outputs.spawn.echo).toBe(BLOB);
  expect((lazy!.context as any).output.echo).toBe(BLOB);
});

// The section's own contract, rather than a value passing through it.
test("objects — absent when nothing is externalized, and a 404 for a ref that is not there", async () => {
  const name = `objects_shape_${crypto.randomUUID()}`;
  await client.PUT("/definitions", {
    body: {
      name,
      tasks: [{ id: "t", output: { small: "inline" }, switch: [{ goto: "end" }] }],
      output: "$: outputs.t",
    } as never,
  });
  const { data: started } = await client.POST("/instances", { body: { process: name } });
  await waitForInstance(started!.id);

  // Nothing crossed the threshold, so there is no section at all — a recipient checks for the
  // field, and one shape everywhere beats a distinction between absent and empty.
  const { data } = await client.GET("/instances/{id}", { params: { path: { id: started!.id } } });
  expect(data!.objects).toBeUndefined();
  expect((data!.context as any).output).toEqual({ small: "inline" });

  // A hash nobody holds is a 404, not an empty body: the store either has the content or it
  // does not, and a caller splicing a stale reference has to be able to tell.
  const { error } = await client.GET("/objects/{ref}", {
    params: { path: { ref: "00000000000000000000000000000000" } },
  });
  expect(error, "an unknown ref must be refused").toBeTruthy();
});

import { expect, test } from "vitest";
import { client, fetchObject, objectAt, spliceObjects, waitForInstance } from "../helpers/client.ts";

const proc = `big_values_${crypto.randomUUID()}`;

// Past the 2 KiB externalization threshold, so it lands in the object store.
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
      // Reads the externalized input, exercising lazy resolution through the output projection.
      output: { echo: "$: input.blob" },
      tasks: [{ id: "work", switch: [{ goto: "end" }] }],
    },
  });
}

test("big values are returned as references by default", async () => {
  await defineProc();
  const { data: started } = await client.POST("/instances", {
    body: { process: proc, input: { blob: BLOB } },
  });
  const id = started!.id;
  await waitForInstance(id);

  const { data, error } = await client.GET("/instances/{id}/detail", {
    params: { path: { id } },
  });
  expect(error).toBeUndefined();
  // The LEAF is cut, not the slot: cutting the slot would fold siblings in with it and stop
  // identical values sharing one object.
  expect((data!.state as any).input.blob).toBeUndefined();
  expect((data!.output as any).echo).toBeUndefined();
  const input = objectAt(data, ["state", "input", "blob"]);
  expect(input, "the big input leaf is listed").toBeDefined();
  expect(input!.size).toBeGreaterThan(BLOB.length - 10);
  expect(objectAt(data, ["output", "echo"]), "the big output leaf is listed").toBeDefined();
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

  const { data, error } = await client.GET("/instances/{id}/detail", { params: { path: { id } } });
  expect(error).toBeUndefined();
  // The slots are ABSENT, not markers: nothing in the data can be mistaken for a reference.
  expect((data!.state as any).input?.blob).toBeUndefined();
  expect(objectAt(data, ["state", "input", "blob"]), "the input's big leaf is listed").toBeDefined();

  await spliceObjects(data);
  expect((data!.state as any).input.blob).toBe(BLOB);
  expect((data!.output as any).echo).toBe(BLOB);
});

// A log payload is cut per leaf like a context slot, so a value the instance already externalized
// shares that object instead of storing a second copy.
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
  const { data: detail } = await client.GET("/instances/{id}/detail", { params: { path: { id } } });
  const slot = objectAt(detail, ["output", "echo"]);
  expect(listed!.ref, "the log shares the context slot's object rather than copying it").toBe(slot!.ref);

  // And the recipient splices an entry's section exactly as it splices the body's.
  await spliceObjects(data);
  expect((completed as { data?: unknown }).data).toEqual({ echo: BLOB });
});

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

  const { data, error } = await client.GET("/instances/{id}/detail", {
    params: { path: { id } },
  });
  expect(error).toBeUndefined();
  expect((data!.state as any).input.blob).toBeUndefined();
  expect(objectAt(data, ["state", "input", "blob"])).toBeDefined();
  expect((data!.output as any)).toEqual({ ok: "done" });
  expect(objectAt(data, ["output"])).toBeUndefined();
});

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

  const { data } = await client.GET("/instances/{id}/detail", { params: { path: { id: started!.id } } });
  expect((data!.state as any).input.token).toBeUndefined();
  expect(objectAt(data, ["state", "input", "token"])).toBeDefined();
  expect(JSON.stringify(data)).not.toContain("SSSSSSSSSS");

  // And it comes back whole when asked for. The API returns what happened; `secret: true` keeps
  // a value off the server's console and does nothing here. specs/object-store.md §Redaction.
  await spliceObjects(data);
  expect((data!.state as any).input.token).toBe(secret);
});

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

  // The objects section is a sibling of items, so the whole response is kept, not just the entry.
  const childCreated = async () => {
    const { data: body } = await client.GET("/instances/{id}/logs", {
      params: { path: { id }, query: { limit: 200, recursive: true } },
    });
    const entry = (body!.items ?? []).find(
      (l) => l.event === "inst_created" && l.instance_id !== id,
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
  const { data: lazy, error } = await client.GET("/instances/{id}/detail", {
    params: { path: { id } },
  });
  expect(error).toBeUndefined();
  expect(objectAt(lazy, ["state", "outputs", "spawn", "echo"])).toBeDefined();
  expect(objectAt(lazy, ["output", "echo"])).toBeDefined();

  // Spliced: the big value is intact after the full parent → child → parent round-trip.
  await spliceObjects(lazy);
  expect((lazy!.state as any).outputs.spawn.echo).toBe(BLOB);
  expect((lazy!.output as any).echo).toBe(BLOB);
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
  const { data } = await client.GET("/instances/{id}/detail", { params: { path: { id: started!.id } } });
  expect(data!.objects).toBeUndefined();
  expect((data!.output as any)).toEqual({ small: "inline" });

  // A hash nobody holds is a 404, not an empty body: the store either has the content or it
  // does not, and a caller splicing a stale reference has to be able to tell.
  const { error } = await client.GET("/objects/{ref}", {
    params: { path: { ref: "00000000000000000000000000000000" } },
  });
  expect(error, "an unknown ref must be refused").toBeTruthy();
});

// The marker travels through the expression and the write re-emits the same reference, so a copy
// costs no load or re-hash. specs/lazy-context.md.
test("a copied slot keeps its reference rather than being loaded and rewritten", async () => {
  const name = `bv_copy_${crypto.randomUUID()}`;
  await client.PUT("/definitions", {
    body: {
      name,
      input_schema: {
        type: "object",
        properties: { blob: { type: "string" } },
        required: ["blob"],
      },
      // The process output COPIES task a's output whole -- it never reads into it.
      output: { final: "$: outputs.a" },
      tasks: [{ id: "a", output: { kept: "$: input.blob" }, switch: [{ goto: "end" }] }],
    },
  });
  const { data: started } = await client.POST("/instances", {
    body: { process: name, input: { blob: BLOB } },
  });
  const id = started!.id;
  expect(await waitForInstance(id)).toBe("completed");

  const { data, error } = await client.GET("/instances/{id}/detail", { params: { path: { id } } });
  expect(error).toBeUndefined();

  const source = objectAt(data, ["state", "outputs", "a", "kept"]);
  const copied = objectAt(data, ["output", "final", "kept"]);
  expect(source, "the task output's big leaf is externalized").toBeDefined();
  expect(copied, "and the copy carries a reference at the same place, not an inlined value").toBeDefined();
  expect(copied!.ref, "the copy must SHARE the object, not write a second one").toBe(source!.ref);

  await spliceObjects(data);
  expect((data!.output as any).final.kept).toBe(BLOB);
});

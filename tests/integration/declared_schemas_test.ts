import { expect, test } from "vitest";
import { client, startMockService, waitForInstance } from "../helpers/client.ts";
import { waitForParked } from "../helpers/external.ts";

// The RUNTIME half of a declared slot schema: the value is conformed to the declaration before
// it leaves the slot. specs/declared-slot-schemas.md §4.
//
// The case the feature exists for is the null: an optional non-nullable property fed null needs
// its key REMOVED, absence being valid there, and genroc has no filter builtin to do it by hand.
// Registration accepts it (the relation admits exactly the gaps this conform closes), so without
// the conform the stored value would carry a null the declaration forbids.

/** A process whose only output is `discount`, taken straight from a nullable input. */
function nullableOutput(name: string, outputSchema: Record<string, unknown>) {
  return {
    name,
    input_schema: {
      type: "object",
      properties: { n: { type: ["number", "null"] } },
    },
    tasks: [{ id: "only", switch: "end" }],
    output: { discount: "$: input.n" },
    output_schema: outputSchema,
  };
}

async function run(def: object, input: Record<string, unknown>) {
  // eslint-disable-next-line @typescript-eslint/no-explicit-any
  const { error: putErr } = await client.PUT("/definitions", { body: def as any });
  expect(putErr).toBeUndefined();
  // eslint-disable-next-line @typescript-eslint/no-explicit-any
  const name = (def as any).name;
  const { data, error } = await client.POST("/instances", { body: { process: name, input } });
  expect(error).toBeUndefined();
  const status = await waitForInstance(data!.id, 10_000);
  const { data: inst } = await client.GET("/instances/{id}/detail", {
    params: { path: { id: data!.id } },
  });
  return { status, output: inst?.state?.output as Record<string, unknown> | undefined };
}

test("a null in an optional non-nullable slot leaves NO key, rather than a null one", async () => {
  const name = `decl_null_${crypto.randomUUID().slice(0, 8)}`;
  const { status, output } = await run(
    nullableOutput(name, {
      type: "object",
      properties: { discount: { type: "number" } },
    }),
    { n: null },
  );
  expect(status).toBe("completed");
  // The repair: absent, not null. `toEqual({})` would pass on a stored `{discount: null}` in
  // some matchers, so the key itself is what is asserted.
  expect(Object.keys(output ?? {})).toEqual([]);
  expect(output).not.toHaveProperty("discount");
});

test("the same slot keeps a non-null value untouched", async () => {
  const name = `decl_keep_${crypto.randomUUID().slice(0, 8)}`;
  const { status, output } = await run(
    nullableOutput(name, {
      type: "object",
      properties: { discount: { type: "number" } },
    }),
    { n: 5 },
  );
  expect(status).toBe("completed");
  expect(output).toEqual({ discount: 5 });
});

// The rule must NOT fire where the target is itself nullable: both states are valid there, so
// removing the key would invent a canonical form the schema never named.
test("a nullable target keeps its null", async () => {
  const name = `decl_nullable_${crypto.randomUUID().slice(0, 8)}`;
  const { status, output } = await run(
    nullableOutput(name, {
      type: "object",
      properties: { discount: { type: ["number", "null"] } },
    }),
    { n: null },
  );
  expect(status).toBe("completed");
  expect(output).toEqual({ discount: null });
});

// A declared schema with no runtime effect must still round-trip the value unchanged — the
// conform is an assertion, so on the ordinary path it is invisible.
test("a declaration that matches exactly changes nothing", async () => {
  const name = `decl_noop_${crypto.randomUUID().slice(0, 8)}`;
  const { status, output } = await run(
    {
      name,
      tasks: [{ id: "only", switch: "end" }],
      output: { a: 1, b: "two" },
      output_schema: {
        type: "object",
        properties: { a: { type: "number" }, b: { type: "string" } },
        required: ["a", "b"],
      },
    },
    {},
  );
  expect(status).toBe("completed");
  expect(output).toEqual({ a: 1, b: "two" });
});

// Registration is where a bad declaration is caught, and the message must name the key rather
// than report a type mismatch somewhere inside an object.
test("a body that sets a key its declaration does not name is refused at registration", async () => {
  const name = `decl_reject_${crypto.randomUUID().slice(0, 8)}`;
  const { error } = await client.PUT("/definitions", {
    body: {
      name,
      tasks: [
        {
          id: "call",
          action: {
            type: "fetch",
            url: "http://127.0.0.1:1/x",
            method: "post",
            body: { amont: 1 },
            body_schema: {
              type: "object",
              properties: { amount: { type: "number" } },
            },
          },
          switch: "end",
        },
      ],
      // eslint-disable-next-line @typescript-eslint/no-explicit-any
    } as any,
  });
  expect(JSON.stringify(error ?? {})).toContain("amont");
});

test("additionalProperties in a declared schema is refused at registration", async () => {
  const name = `decl_ap_${crypto.randomUUID().slice(0, 8)}`;
  const { error } = await client.PUT("/definitions", {
    body: {
      name,
      tasks: [{ id: "only", switch: "end" }],
      output: { a: 1 },
      output_schema: {
        type: "object",
        properties: { a: { type: "number" } },
        additionalProperties: { type: "string" },
      },
      // eslint-disable-next-line @typescript-eslint/no-explicit-any
    } as any,
  });
  expect(JSON.stringify(error ?? {})).toContain("additionalProperties");
});

// child_list's declaration types ONE element, so the conform runs per element — the same rule
// `result_schema` follows there. A null in an optional non-nullable slot of an element is
// repaired for that element alone, and its siblings are untouched.
test("child_list conforms each element, repairing one without disturbing the others", async () => {
  const child = `decl_cl_child_${crypto.randomUUID().slice(0, 8)}`;
  const parent = `decl_cl_${crypto.randomUUID().slice(0, 8)}`;

  // The child echoes what it was sent, so the parent's output shows what each child received.
  const { error: childErr } = await client.PUT("/definitions", {
    body: {
      name: child,
      input_schema: {
        type: "object",
        properties: { discount: { type: "number" } },
      },
      tasks: [{ id: "only", switch: "end" }],
      output: { seen: "$: input" },
      // eslint-disable-next-line @typescript-eslint/no-explicit-any
    } as any,
  });
  expect(childErr).toBeUndefined();

  const { error: parentErr } = await client.PUT("/definitions", {
    body: {
      name: parent,
      input_schema: {
        type: "object",
        properties: {
          rows: {
            type: "array",
            items: { type: "object", properties: { discount: { type: ["number", "null"] } } },
          },
        },
        required: ["rows"],
      },
      tasks: [
        {
          id: "fan",
          action: {
            type: "child_list",
            name: child,
            over: "$: input.rows",
            input_schema: {
              type: "object",
              properties: { discount: { type: "number" } },
            },
            // Types each element of the fan-out's result, which is what makes it readable.
            result_schema: {
              type: "object",
              properties: {
                seen: {
                  type: "object",
                  properties: { discount: { type: ["number", "null"] } },
                },
              },
            },
          },
          // `outputs.fan` comes from the task's own output map, never from the action result.
          output: "$: self.result",
          switch: "end",
        },
      ],
      output: { got: "$: outputs.fan" },
      // eslint-disable-next-line @typescript-eslint/no-explicit-any
    } as any,
  });
  expect(parentErr).toBeUndefined();

  const { data, error } = await client.POST("/instances", {
    body: { process: parent, input: { rows: [{ discount: null }, { discount: 7 }] } },
  });
  expect(error).toBeUndefined();
  expect(await waitForInstance(data!.id, 10_000)).toBe("completed");

  const { data: inst } = await client.GET("/instances/{id}/detail", {
    params: { path: { id: data!.id } },
  });
  // eslint-disable-next-line @typescript-eslint/no-explicit-any
  const got = (inst?.state?.output as any)?.got as { seen: Record<string, unknown> }[];
  expect(got).toHaveLength(2);
  // The first element's null was removed; the second was left exactly as it arrived.
  expect(got[0].seen).toEqual({});
  expect(got[1].seen).toEqual({ discount: 7 });
});

// The registration half of a declared child input: what the call site says it sends must fit
// what the child accepts. This one needs a server, because it is the only check here that reads
// the child out of the database.
test("a declaration that does not fit the child is refused, naming both sides", async () => {
  const child = `decl_fit_child_${crypto.randomUUID().slice(0, 8)}`;
  await client.PUT("/definitions", {
    body: {
      name: child,
      input_schema: {
        type: "object",
        properties: { count: { type: "number" } },
        required: ["count"],
      },
      tasks: [{ id: "only", switch: "end" }],
      // eslint-disable-next-line @typescript-eslint/no-explicit-any
    } as any,
  });

  const { error } = await client.PUT("/definitions", {
    body: {
      name: `decl_fit_${crypto.randomUUID().slice(0, 8)}`,
      tasks: [
        {
          id: "call",
          action: {
            type: "child",
            name: child,
            // Consistent with its own declaration, so the offline check passes and the one
            // against the child is what speaks.
            input: { count: "x" },
            // The call site promises a string where the child requires a number.
            input_schema: {
              type: "object",
              properties: { count: { type: "string" } },
              required: ["count"],
            },
          },
          switch: "end",
        },
      ],
      // eslint-disable-next-line @typescript-eslint/no-explicit-any
    } as any,
  });
  expect(JSON.stringify(error ?? {})).toContain("input_schema");
});

// Its converse, and the reason the check had to change: the raw input carries a null the
// conform removes, so comparing the INFERRED type against the child would refuse a call that
// works. The declaration is what the child receives.
test("a nullable input a declaration repairs is accepted against a child that forbids null", async () => {
  const child = `decl_rep_child_${crypto.randomUUID().slice(0, 8)}`;
  const { error: childErr } = await client.PUT("/definitions", {
    body: {
      name: child,
      input_schema: { type: "object", properties: { discount: { type: "number" } } },
      tasks: [{ id: "only", switch: "end" }],
      output: { seen: "$: input" },
      // eslint-disable-next-line @typescript-eslint/no-explicit-any
    } as any,
  });
  expect(childErr).toBeUndefined();

  const parent = `decl_rep_${crypto.randomUUID().slice(0, 8)}`;
  const { error } = await client.PUT("/definitions", {
    body: {
      name: parent,
      input_schema: {
        type: "object",
        properties: { n: { type: ["number", "null"] } },
      },
      tasks: [
        {
          id: "call",
          action: {
            type: "child",
            name: child,
            // Inferred as number|null, which the child's schema alone would refuse.
            input: { discount: "$: input.n" },
            input_schema: { type: "object", properties: { discount: { type: "number" } } },
            result_schema: {
              type: "object",
              properties: {
                seen: { type: "object", properties: { discount: { type: ["number", "null"] } } },
              },
            },
          },
          output: "$: self.result",
          switch: "end",
        },
      ],
      output: { got: "$: outputs.call" },
      // eslint-disable-next-line @typescript-eslint/no-explicit-any
    } as any,
  });
  expect(error).toBeUndefined();

  const { data } = await client.POST("/instances", {
    body: { process: parent, input: { n: null } },
  });
  expect(await waitForInstance(data!.id, 10_000)).toBe("completed");
  const { data: inst } = await client.GET("/instances/{id}/detail", {
    params: { path: { id: data!.id } },
  });
  // eslint-disable-next-line @typescript-eslint/no-explicit-any
  expect((inst?.state?.output as any)?.got?.seen).toEqual({});
});

// ─── The conform at every OTHER slot ────────────────────────────────────────────
//
// The repair above is the process output's. Each slot below runs the same conform on a
// different value, and each is observable somewhere different — which is why they are separate
// tests rather than one: a slot wired to no conform passes every check that does not look at
// what actually left it.

test("a fetch body drops an optional null before the request is sent", async () => {
  const mock = await startMockService(0, { response: { ok: true } });
  const name = `decl_body_rt_${crypto.randomUUID().slice(0, 8)}`;
  const { error: putErr } = await client.PUT("/definitions", {
    body: {
      name,
      input_schema: { type: "object", properties: { n: { type: ["number", "null"] } } },
      tasks: [
        {
          id: "call",
          action: {
            type: "fetch",
            url: `http://localhost:${mock.port}/x`,
            method: "post",
            body: { discount: "$: input.n", keep: 1 },
            body_schema: {
              type: "object",
              properties: { discount: { type: "number" }, keep: { type: "number" } },
            },
          },
          switch: "end",
        },
      ],
      // eslint-disable-next-line @typescript-eslint/no-explicit-any
    } as any,
  });
  expect(putErr).toBeUndefined();

  const { data } = await client.POST("/instances", { body: { process: name, input: { n: null } } });
  expect(await waitForInstance(data!.id, 10_000)).toBe("completed");

  // The wire is the only place this is observable — the stored body would look the same either
  // way if the conform ran after the request rather than before it.
  const sent = JSON.parse(mock.requestBodies()[0]);
  expect(sent).toEqual({ keep: 1 });
  expect(sent).not.toHaveProperty("discount");
  mock.stop();
});

// The one slot where the conform can change NOTHING observable, and it is worth saying so
// rather than leaving a test that looks like it covers something. A query value that is null
// omits its parameter at serialisation anyway (fetch-http-surface.md §1), so the conform and
// that rule land on the same bytes; an undeclared key never reaches runtime, because the closed
// check refuses it at registration. What this pins is that the two rules AGREE — disabling the
// conform does not move this assertion, by design.
test("a declared query and the null-omit rule agree on what is sent", async () => {
  const mock = await startMockService(0, { response: { ok: true } });
  const name = `decl_query_rt_${crypto.randomUUID().slice(0, 8)}`;
  await client.PUT("/definitions", {
    body: {
      name,
      input_schema: { type: "object", properties: { n: { type: ["number", "null"] } } },
      tasks: [
        {
          id: "call",
          action: {
            type: "fetch",
            url: `http://localhost:${mock.port}/x`,
            method: "get",
            query: { page: "$: input.n", size: 10 },
            query_schema: {
              type: "object",
              properties: { page: { type: "number" }, size: { type: "number" } },
            },
          },
          switch: "end",
        },
      ],
      // eslint-disable-next-line @typescript-eslint/no-explicit-any
    } as any,
  });
  const { data } = await client.POST("/instances", { body: { process: name, input: { n: null } } });
  expect(await waitForInstance(data!.id, 10_000)).toBe("completed");
  expect(mock.requestUrls()[0]).toBe("/x?size=10");
  mock.stop();
});

test("an external input snapshot is conformed before a worker ever sees it", async () => {
  const name = `decl_ext_rt_${crypto.randomUUID().slice(0, 8)}`;
  await client.PUT("/definitions", {
    body: {
      name,
      input_schema: { type: "object", properties: { n: { type: ["number", "null"] } } },
      tasks: [
        {
          id: "wait",
          action: {
            type: "external",
            input: { discount: "$: input.n", job: "x" },
            input_schema: {
              type: "object",
              properties: { discount: { type: "number" }, job: { type: "string" } },
            },
          },
          switch: "end",
        },
      ],
      // eslint-disable-next-line @typescript-eslint/no-explicit-any
    } as any,
  });
  const { data } = await client.POST("/instances", { body: { process: name, input: { n: null } } });
  const parked = await waitForParked(data!.id);
  // The snapshot IS the contract a worker reads, so the declaration has to describe it.
  expect(parked.input).toEqual({ job: "x" });
  // Answered so the instance does not sit parked for the rest of the run.
  await client.POST("/external-tasks/resolve", { body: { token: parked.token, result: {} } });
  expect(await waitForInstance(data!.id, 10_000)).toBe("completed");
});

test("a task output is conformed, and what later tasks read is the repaired value", async () => {
  const name = `decl_task_rt_${crypto.randomUUID().slice(0, 8)}`;
  const { error: putErr } = await client.PUT("/definitions", {
    body: {
      name,
      input_schema: { type: "object", properties: { n: { type: ["number", "null"] } } },
      tasks: [
        {
          id: "first",
          output: { discount: "$: input.n", keep: 1 },
          output_schema: {
            type: "object",
            properties: { discount: { type: "number" }, keep: { type: "number" } },
          },
          switch: "next",
        },
        { id: "second", switch: "end" },
      ],
      output: { got: "$: outputs.first" },
      // eslint-disable-next-line @typescript-eslint/no-explicit-any
    } as any,
  });
  expect(putErr).toBeUndefined();

  const { data } = await client.POST("/instances", { body: { process: name, input: { n: null } } });
  expect(await waitForInstance(data!.id, 10_000)).toBe("completed");
  const { data: inst } = await client.GET("/instances/{id}/detail", {
    params: { path: { id: data!.id } },
  });
  // eslint-disable-next-line @typescript-eslint/no-explicit-any
  expect((inst?.state?.output as any)?.got).toEqual({ keep: 1 });
});

// The conform's OTHER half, which nothing at runtime exercised until now: a required nullable
// the shape never sets is written in as an explicit null rather than left absent.
test("a required nullable property the shape never sets is written in as null", async () => {
  const name = `decl_insert_${crypto.randomUUID().slice(0, 8)}`;
  const { status, output } = await run(
    {
      name,
      tasks: [{ id: "only", switch: "end" }],
      output: { a: 1 },
      output_schema: {
        type: "object",
        properties: { a: { type: "number" }, seen: { type: ["string", "null"] } },
        required: ["a", "seen"],
      },
    },
    {},
  );
  expect(status).toBe("completed");
  expect(output).toEqual({ a: 1, seen: null });
});

test("a child_map entry conforms its own input, per entry", async () => {
  const child = `decl_cm_child_${crypto.randomUUID().slice(0, 8)}`;
  const { error: childErr } = await client.PUT("/definitions", {
    body: {
      name: child,
      input_schema: { type: "object", properties: { discount: { type: "number" } } },
      tasks: [{ id: "only", switch: "end" }],
      output: { seen: "$: input" },
      // eslint-disable-next-line @typescript-eslint/no-explicit-any
    } as any,
  });
  expect(childErr).toBeUndefined();

  const resultSchema = {
    type: "object",
    properties: {
      seen: { type: "object", properties: { discount: { type: ["number", "null"] } } },
    },
  };
  const parent = `decl_cm_${crypto.randomUUID().slice(0, 8)}`;
  const { error: putErr } = await client.PUT("/definitions", {
    body: {
      name: parent,
      input_schema: { type: "object", properties: { n: { type: ["number", "null"] } } },
      tasks: [
        {
          id: "fan",
          action: {
            type: "child_map",
            children: {
              // One entry declares and repairs; the other declares nothing and passes through.
              declared: {
                name: child,
                input: { discount: "$: input.n" },
                input_schema: { type: "object", properties: { discount: { type: "number" } } },
                result_schema: resultSchema,
              },
              plain: { name: child, input: {}, result_schema: resultSchema },
            },
          },
          output: "$: self.result",
          switch: "end",
        },
      ],
      output: { got: "$: outputs.fan" },
      // eslint-disable-next-line @typescript-eslint/no-explicit-any
    } as any,
  });
  expect(putErr).toBeUndefined();

  const { data } = await client.POST("/instances", { body: { process: parent, input: { n: null } } });
  expect(await waitForInstance(data!.id, 10_000)).toBe("completed");
  const { data: inst } = await client.GET("/instances/{id}/detail", {
    params: { path: { id: data!.id } },
  });
  // eslint-disable-next-line @typescript-eslint/no-explicit-any
  const got = (inst?.state?.output as any)?.got;
  // The declared entry's null was removed before the child ever received it. The undeclared
  // one is the control: the child's own conform is what shaped it, and it is untouched here.
  expect(got.declared.seen).toEqual({});
  expect(got.plain.seen).toEqual({});
});

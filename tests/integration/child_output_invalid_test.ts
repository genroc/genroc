import { expect, test } from "vitest";
import { client, waitForInstance, childrenOfTask } from "../helpers/client.ts";

// A child that exports the TOP TYPE and a caller that narrows it with result_schema: the
// registration check passes by construction (no declared output = nothing to compare), so
// the conform at collect is the only gate. When the caller's bet loses, that is
// `output.invalid` — catchable on the child task — not the terminal engine.collect it used
// to be. specs/error-extensions.md §X2-c.

// forwarder echoes whatever it is given, leaving its output untyped — the generic-wrapper
// shape the whole feature exists for.
async function putForwarder(name: string) {
  await client.PUT("/definitions", {
    body: {
      name,
      input_schema: { type: "object", properties: { value: {} } },
      tasks: [{ id: "done", switch: [{ goto: "end" }] }],
      output: "$: input.value",
    },
  });
}

test("a lost narrowing bet is output.invalid, and an on_error rule catches it", async () => {
  const uid = crypto.randomUUID().slice(0, 8);
  const child = `oi_child_${uid}`;
  const parent = `oi_caught_${uid}`;
  await putForwarder(child);

  const { error: putErr } = await client.PUT("/definitions", {
    body: {
      name: parent,
      tasks: [
        {
          id: "call",
          action: {
            type: "child" as const,
            name: child,
            input: { value: 42 },
            result_schema: { type: "object", properties: { ok: { type: "boolean" } } },
          },
          // R5 rejected this pattern before X2-c: output.invalid is the one dotted code a
          // child task can name.
          on_error: [{ code: ["output.invalid"], goto: "$fallback" }],
          switch: [{ goto: "end" }],
        },
        { id: "fallback", output: { code: "$: error.code" }, switch: [{ goto: "end" }] },
      ],
      output: "$: outputs.fallback",
    },
  });
  expect(putErr, "a child task may name output.invalid in on_error").toBeUndefined();

  const { data: started } = await client.POST("/instances", { body: { process: parent } });
  const id = started!.id;
  expect(await waitForInstance(id)).toBe("completed");

  const { data } = await client.GET("/instances/{id}/detail", { params: { path: { id } } });
  expect(
    (data?.state?.output as any)?.code,
    "the handler reads the mismatch as error.code, so the route is the mismatch and not something else",
  ).toBe("output.invalid");
});

test("with no rule the parent still fails terminally — as output.invalid, not engine.collect", async () => {
  const uid = crypto.randomUUID().slice(0, 8);
  const child = `oi_child2_${uid}`;
  const parent = `oi_uncaught_${uid}`;
  await putForwarder(child);

  await client.PUT("/definitions", {
    body: {
      name: parent,
      tasks: [
        {
          id: "call",
          action: {
            type: "child" as const,
            name: child,
            input: { value: 42 },
            result_schema: { type: "object", properties: { ok: { type: "boolean" } } },
          },
          switch: [{ goto: "end" }],
        },
      ],
    },
  });

  const { data: started } = await client.POST("/instances", { body: { process: parent } });
  const id = started!.id;
  expect(await waitForInstance(id)).toBe("failed");

  const { data } = await client.GET("/instances/{id}/detail", { params: { path: { id } } });
  expect(data?.error_code, "the split renames only this failure").toBe("output.invalid");
  expect(data?.error_message).not.toContain("engine.collect");

  // The error being diagnosed survives it: the child kept its output, and only the
  // parent's row names the mismatch.
  const childId = (await childrenOfTask(started!.id, "call")) as string;
  const { data: kid } = await client.GET("/instances/{id}/detail", {
    params: { path: { id: childId } },
  });
  expect(kid?.status, "a mismatch must not retroactively fail the child").toBe("completed");
  expect(kid?.state?.output).toBe(42);
});

// The catchable set widened by exactly ONE code, not by a family: every other engine code is
// still unreachable on a child task, so naming one is the typo R5 exists to catch.
test("only output.invalid joins the set — another engine code is still refused", async () => {
  const uid = crypto.randomUUID().slice(0, 8);
  const child = `oi_child3_${uid}`;
  await putForwarder(child);

  const { error } = await client.PUT("/definitions", {
    body: {
      name: `oi_narrow_${uid}`,
      tasks: [
        {
          id: "call",
          action: { type: "child" as const, name: child, input: { value: 42 } },
          on_error: [{ code: ["output.parse"], goto: "end" }],
          switch: [{ goto: "end" }],
        },
      ],
    },
  });
  expect(JSON.stringify(error)).toContain("no child of this task can raise");
});

// The conform runs per collected child, so the split covers the fan-out shapes too: one bad
// element takes the whole batch to output.invalid rather than engine.collect.
test("a child_map entry that fails its own narrowing reports output.invalid", async () => {
  const uid = crypto.randomUUID().slice(0, 8);
  const child = `oi_map_child_${uid}`;
  const parent = `oi_map_${uid}`;
  await putForwarder(child);

  await client.PUT("/definitions", {
    body: {
      name: parent,
      tasks: [
        {
          id: "call",
          action: {
            type: "child_map" as const,
            children: {
              // Each entry narrows on its own, so the failing one is the one that decides.
              good: { name: child, input: { value: { ok: true } } },
              bad: {
                name: child,
                input: { value: 42 },
                result_schema: { type: "object", properties: { ok: { type: "boolean" } } },
              },
            },
          },
          on_error: [{ code: ["output.invalid"], goto: "$fallback" }],
          switch: [{ goto: "end" }],
        },
        { id: "fallback", output: { code: "$: error.code" }, switch: [{ goto: "end" }] },
      ],
      output: "$: outputs.fallback",
    },
  });

  const { data: started } = await client.POST("/instances", { body: { process: parent } });
  expect(await waitForInstance(started!.id)).toBe("completed");
  const { data } = await client.GET("/instances/{id}/detail", { params: { path: { id: started!.id } } });
  expect((data?.state?.output as any)?.code).toBe("output.invalid");
});

test("a child_list element that fails the narrowing reports output.invalid", async () => {
  const uid = crypto.randomUUID().slice(0, 8);
  const child = `oi_list_child_${uid}`;
  const parent = `oi_list_${uid}`;
  await putForwarder(child);

  await client.PUT("/definitions", {
    body: {
      name: parent,
      tasks: [
        {
          id: "call",
          action: {
            type: "child_list" as const,
            name: child,
            over: '$: [{"value": 1}, {"value": 2}]',
            result_schema: { type: "object", properties: { ok: { type: "boolean" } } },
          },
          on_error: [{ code: ["output.invalid"], goto: "$fallback" }],
          switch: [{ goto: "end" }],
        },
        { id: "fallback", output: { code: "$: error.code" }, switch: [{ goto: "end" }] },
      ],
      output: "$: outputs.fallback",
    },
  });

  const { data: started } = await client.POST("/instances", { body: { process: parent } });
  expect(await waitForInstance(started!.id)).toBe("completed");
  const { data } = await client.GET("/instances/{id}/detail", { params: { path: { id: started!.id } } });
  expect((data?.state?.output as any)?.code).toBe("output.invalid");
});

import { expect, test } from "vitest";
import { client, waitForInstance } from "../helpers/client.ts";

// A parent catches a child's raised code via on_error on the child task; with no matching rule the
// raise degrades to a defect carrying the child's code and message (docs §5.2). Unique child names
// keep each version-pinned parent on the child its own test registered.

async function putChild(name: string, raiseCode: string) {
  await client.PUT("/definitions", {
    body: {
      name,
      tasks: [
        {
          id: "decide",
          switch: [
            {
              case: "true",
              raise: { code: raiseCode, message: `child raised ${raiseCode}` },
            },
            { goto: "end" },
          ],
        },
      ],
    },
  });
}

// `%` is the only wildcard (`_` is literal). Registration checks the pattern can match a raise (R5);
// matchOnError routes at runtime.
test("catch — a wildcard pattern matches the child's raised code", async () => {
  const suffix = crypto.randomUUID().slice(0, 8).replace(/-/g, "_");
  const child = `like_child_${suffix}`;
  await putChild(child, "fourth_failed");

  const parent = `like_parent_${suffix}`;
  await client.PUT("/definitions", {
    body: {
      name: parent,
      tasks: [
        {
          id: "pay",
          action: { type: "child_map" as const, children: { a: { name: child } } },
          on_error: [{ code: ["fourth_%"], goto: "end" }],
          switch: "end",
        },
      ],
    },
  });

  const { data: started } = await client.POST("/instances", {
    body: { process: parent },
  });
  const id = started!.id;
  expect(await waitForInstance(id)).toBe("completed");
});

test("catch — a matching rule routes the parent to a recovery task", async () => {
  const suffix = crypto.randomUUID().slice(0, 8).replace(/-/g, "_");
  const child = `catch_child_${suffix}`;
  await putChild(child, "declined");

  const parent = `catch_parent_${suffix}`;
  await client.PUT("/definitions", {
    body: {
      name: parent,
      tasks: [
        {
          id: "pay",
          action: { type: "child_map" as const, children: { a: { name: child } } },
          on_error: [{ code: ["declined"], goto: "$recover" }],
          switch: "end",
        },
        {
          id: "recover",
          output: "$: last_error.code",
          switch: "end",
        },
      ],
    },
  });

  const { data: started } = await client.POST("/instances", {
    body: { process: parent },
  });
  const id = started!.id;
  expect(await waitForInstance(id)).toBe("completed");

  const { data } = await client.GET("/instances/{id}/detail", {
    params: { path: { id } },
  });
  // The routed task keeps its context and sees the raised error.
  expect((data?.state?.outputs as Record<string, unknown>)?.recover).toBe(
    "declined",
  );
});

test("catch — a rule routes to end, completing the parent (and computes output)", async () => {
  const suffix = crypto.randomUUID().slice(0, 8).replace(/-/g, "_");
  const child = `catchend_child_${suffix}`;
  await putChild(child, "declined");

  const parent = `catchend_parent_${suffix}`;
  await client.PUT("/definitions", {
    body: {
      name: parent,
      // Static, so its presence proves resolution's goto:end ran computeOutput.
      output: '$: "handled"',
      tasks: [
        {
          id: "pay",
          action: { type: "child_map" as const, children: { a: { name: child } } },
          on_error: [{ code: ["declined"], goto: "end" }],
          switch: "end",
        },
      ],
    },
  });

  const { data: started } = await client.POST("/instances", {
    body: { process: parent },
  });
  const id = started!.id;
  expect(await waitForInstance(id)).toBe("completed");

  const { data } = await client.GET("/instances/{id}/detail", {
    params: { path: { id } },
  });
  expect(data?.status).toBe("completed");
  expect(data?.output).toBe("handled");
});

// resolveRaisedBatch's panic branch.
test("catch — a rule panics, failing the parent with the authored code", async () => {
  const suffix = crypto.randomUUID().slice(0, 8).replace(/-/g, "_");
  const child = `catchpanic_child_${suffix}`;
  await putChild(child, "declined");

  const parent = `catchpanic_parent_${suffix}`;
  await client.PUT("/definitions", {
    body: {
      name: parent,
      tasks: [
        {
          id: "pay",
          action: { type: "child_map" as const, children: { a: { name: child } } },
          on_error: [
            {
              code: ["declined"],
              panic: { code: "cannot_settle", message: "the batch cannot be settled" },
            },
          ],
          switch: "end",
        },
      ],
    },
  });

  const { data: started } = await client.POST("/instances", {
    body: { process: parent },
  });
  const id = started!.id;
  expect(await waitForInstance(id)).toBe("failed");

  const { data } = await client.GET("/instances/{id}/detail", {
    params: { path: { id } },
  });
  expect(data?.status).toBe("failed");
  expect(data?.error_code).toBe("cannot_settle");
  expect(data?.error_message).toBe("the batch cannot be settled");
});

test("catch — a rule re-raises, so the error propagates one named level up", async () => {
  const suffix = crypto.randomUUID().slice(0, 8).replace(/-/g, "_");
  const child = `reraise_child_${suffix}`;
  await putChild(child, "declined");

  const parent = `reraise_parent_${suffix}`;
  await client.PUT("/definitions", {
    body: {
      name: parent,
      tasks: [
        {
          id: "pay",
          action: { type: "child_map" as const, children: { a: { name: child } } },
          on_error: [
            {
              code: ["declined"],
              raise: { code: "payment_failed", message: "payment could not complete" },
            },
          ],
          switch: "end",
        },
      ],
    },
  });

  const { data: started } = await client.POST("/instances", {
    body: { process: parent },
  });
  const id = started!.id;
  expect(await waitForInstance(id)).toBe("raised");

  const { data } = await client.GET("/instances/{id}/detail", {
    params: { path: { id } },
  });
  expect(data?.status).toBe("raised");
  expect(data?.error_code).toBe("payment_failed");
  // Underneath, `error` still mirrors the child that caused it.
  const err = data?.state?.last_error as Record<string, unknown>;
  expect(err?.code).toBe("declined");
  expect(err?.child_key).toBe("a");
});

test("unhandled — the parent fails mirroring the child's raised code and message", async () => {
  const suffix = crypto.randomUUID().slice(0, 8).replace(/-/g, "_");
  // `surprise` is raisable but has no rule, so it reaches resolution unhandled — the gap R5
  // deliberately allows (D3).
  const child = `unhandled_child_${suffix}`;
  await client.PUT("/definitions", {
    body: {
      name: child,
      tasks: [
        {
          id: "decide",
          switch: [
            { case: "true", raise: { code: "surprise", message: "an unhandled surprise" } },
            { case: "false", raise: { code: "handled", message: "h" } },
            { goto: "end" },
          ],
        },
      ],
    },
  });
  const parent = `unhandled_parent_${suffix}`;
  await client.PUT("/definitions", {
    body: {
      name: parent,
      tasks: [
        {
          id: "pay",
          action: { type: "child_map" as const, children: { a: { name: child } } },
          // Rule for a raisable-but-not-raised code; passes R5, never fires at runtime.
          on_error: [{ code: ["handled"], goto: "$done" }],
          switch: "end",
        },
        { id: "done", switch: "end" },
      ],
    },
  });

  const { data: started } = await client.POST("/instances", {
    body: { process: parent },
  });
  const id = started!.id;
  expect(await waitForInstance(id)).toBe("failed");

  const { data } = await client.GET("/instances/{id}/detail", {
    params: { path: { id } },
  });
  expect(data?.status).toBe("failed");
  expect(data?.error_code).toBe("surprise");
  expect(data?.error_message).toContain("surprise");
  expect(data?.error_message).toContain("an unhandled surprise");
  expect(data?.error_message).not.toContain("engine.collect");
});

// Slots 1 and 3 both raise; the first by child_index routes, whatever the completion order (§5.2, I3).
test("batch — the first raised child (by child_index) routes the parent", async () => {
  const suffix = crypto.randomUUID().slice(0, 8).replace(/-/g, "_");
  // A child that raises based on its input, so a child_list fan-out raises on some items.
  const child = `fanout_child_${suffix}`;
  await client.PUT("/definitions", {
    body: {
      name: child,
      input_schema: {
        type: "object",
        properties: { ok: { type: "boolean" } },
        required: ["ok"],
      },
      tasks: [
        {
          id: "decide",
          switch: [
            {
              case: "input.ok == false",
              raise: { code: "bad_item", message: "the item was rejected" },
            },
            { goto: "end" },
          ],
        },
      ],
    },
  });

  const parent = `fanout_parent_${suffix}`;
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
              properties: { ok: { type: "boolean" } },
              required: ["ok"],
            },
          },
        },
        required: ["items"],
      },
      tasks: [
        {
          id: "fan",
          action: {
            type: "child_list" as const,
            name: child,
            over: "$: input.items",
          },
          on_error: [{ code: ["bad_item"], goto: "$report" }],
          switch: "end",
        },
        { id: "report", output: "$: last_error", switch: "end" },
      ],
    },
  });

  const { data: started } = await client.POST("/instances", {
    body: {
      process: parent,
      input: {
        items: [{ ok: true }, { ok: false }, { ok: true }, { ok: false }],
      },
    },
  });
  const id = started!.id;
  expect(await waitForInstance(id)).toBe("completed");

  const { data } = await client.GET("/instances/{id}/detail", {
    params: { path: { id } },
  });
  const reported = (data?.state?.outputs as Record<string, unknown>)
    ?.report as Record<string, unknown>;
  // First raised child (index 1, not 0) routes, and `error` mirrors it.
  expect(reported?.child_index).toBe(1);
  expect(reported?.code).toBe("bad_item");
  expect(reported?.message).toBe("the item was rejected");
});

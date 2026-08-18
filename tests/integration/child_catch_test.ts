import { expect, test } from "vitest";
import { client, waitForInstance } from "../helpers/client.ts";

// Phase 2/3: a parent catches a child's raised error through on_error rules on the child
// task. The raised code is matched by the same patterns as an action task's on_error
// (`%` is the only wildcard, so `order_%` matches `order_placed`); a matching rule routes the parent
// (goto / raise / panic), and no matching rule degrades the raise to a defect that fails
// the parent — carrying the child's own code and message forward (docs §5.2).
//
// A unique-per-test child name keeps the version-pinned parent pointed at exactly the
// child this test registered.

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

// A wildcard pattern (not just a literal code) catches the child's raised code. `%` is
// the only wildcard, so `fourth_%` matches `fourth_failed` (the underscore is literal).
// Registration confirms the pattern can match a raise (R5); matchOnError routes at runtime.
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

// A rule that routes to a task: the parent recovers and completes past the batch.
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
          output: "$: error.code",
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

  const { data } = await client.GET("/instances/{id}", {
    params: { path: { id } },
  });
  // The recovery task read `error.code` — the routed task keeps its context and sees the
  // raised error, mirroring what the child raised.
  expect((data?.context?.outputs as Record<string, unknown>)?.recover).toBe(
    "declined",
  );
});

// A rule that routes to `end`: the parent *completes* — this is the playground pattern
// (`on_error: [{code, goto: end}]`). Unlike handleCallError's end branch, resolution's
// end branch computes the process output, so a static output is produced.
test("catch — a rule routes to end, completing the parent (and computes output)", async () => {
  const suffix = crypto.randomUUID().slice(0, 8).replace(/-/g, "_");
  const child = `catchend_child_${suffix}`;
  await putChild(child, "declined");

  const parent = `catchend_parent_${suffix}`;
  await client.PUT("/definitions", {
    body: {
      name: parent,
      // A static output (does not read `error`, so no reachability complication): its
      // presence in the completed instance proves resolution's goto:end ran computeOutput.
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

  const { data } = await client.GET("/instances/{id}", {
    params: { path: { id } },
  });
  expect(data?.status).toBe("completed");
  expect(data?.context?.output).toBe("handled");
});

// A rule that panics: the parent fails uncatchably with the authored panic code. This is
// resolveRaisedBatch's panic branch (the child_task analogue of an on_error panic).
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

  const { data } = await client.GET("/instances/{id}", {
    params: { path: { id } },
  });
  expect(data?.status).toBe("failed");
  expect(data?.error_code).toBe("cannot_settle");
  expect(data?.error).toBe("the batch cannot be settled");
});

// A rule that raises: the parent re-raises with its own code, and `error` mirrors the
// child underneath.
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

  const { data } = await client.GET("/instances/{id}", {
    params: { path: { id } },
  });
  expect(data?.status).toBe("raised");
  expect(data?.error_code).toBe("payment_failed");
  // Underneath, `error` still mirrors the child that caused it.
  const err = data?.context?.error as Record<string, unknown>;
  expect(err?.code).toBe("declined");
  expect(err?.child_key).toBe("a");
});

// No matching rule: the raise degrades to a defect. The parent fails, and — the point of
// this whole change — its error mirrors the child's raised code and message, not a
// generic engine.collect.
test("unhandled — the parent fails mirroring the child's raised code and message", async () => {
  const suffix = crypto.randomUUID().slice(0, 8).replace(/-/g, "_");
  // The child can raise two codes; the parent has a rule only for the other one, so at
  // runtime `surprise` reaches resolution unhandled. This is the runtime-surfaced gap R5
  // deliberately allows (D3): the rule is reachable, just not the code that fired.
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

  const { data } = await client.GET("/instances/{id}", {
    params: { path: { id } },
  });
  expect(data?.status).toBe("failed");
  // Mirrors the child: error_code is the raised code, not engine.collect; the message
  // carries the child's message and names the child.
  expect(data?.error_code).toBe("surprise");
  expect(data?.error).toContain("surprise");
  expect(data?.error).toContain("an unhandled surprise");
  expect(data?.error).not.toContain("engine.collect");
});

// A fan-out where several children raise: the first (by child_index) routes
// deterministically, regardless of completion order (§5.2, I3). Slots 1 and 3 both raise;
// slot 1 is the one that drives the parent.
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
        { id: "report", output: "$: error", switch: "end" },
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

  const { data } = await client.GET("/instances/{id}", {
    params: { path: { id } },
  });
  const reported = (data?.context?.outputs as Record<string, unknown>)
    ?.report as Record<string, unknown>;
  // First raised child (index 1, not 0) routes, and `error` mirrors it.
  expect(reported?.child_index).toBe(1);
  expect(reported?.code).toBe("bad_item");
  expect(reported?.message).toBe("the item was rejected");
});

import { expect, test } from "vitest";
import { client, startMockService, waitForInstance } from "../helpers/client.ts";
import type { components } from "../generated/api.ts";

// A raise or panic may carry `data`: a shape evaluated in the same scope as the message
// and landed on the instance's own error.data, where an operator reads it. This is the half
// that reaches nobody else — a parent reads a raise's data only where the calling task
// declares its shape. specs/error-extensions.md §X2-c.

test("a raise carries data onto its own row, evaluated in the clause's scope", async () => {
  const name = `raise_data_${crypto.randomUUID()}`;
  const { error: putErr } = await client.PUT("/definitions", {
    body: {
      name,
      input_schema: {
        type: "object",
        properties: { code: { type: "string" } },
        required: ["code"],
      },
      tasks: [
        {
          id: "charge",
          switch: [
            {
              case: "true",
              raise: {
                code: "card_declined",
                message: "the card was declined",
                // The structured half of the same condition the message describes in prose:
                // a scheduling caller needs the number, not the sentence.
                data: { decline_code: "$: input.code", retry_after: 3600 },
              },
            },
            { goto: "end" },
          ],
        },
      ],
    },
  });
  expect(putErr).toBeUndefined();

  const { data: started } = await client.POST("/instances", {
    body: { process: name, input: { code: "51" } },
  });
  const id = started!.id;
  expect(await waitForInstance(id)).toBe("raised");

  const { data } = await client.GET("/instances/{id}", { params: { path: { id } } });
  expect(data?.error_code).toBe("card_declined");
  expect((data?.context?.error as any)?.data).toEqual({
    decline_code: "51",
    retry_after: 3600,
  });
});

test("a panic's data stays on the instance that authored it — ancestors inherit code and message only", async () => {
  const uid = crypto.randomUUID().slice(0, 8);
  const child = `panic_data_child_${uid}`;
  const parent = `panic_data_parent_${uid}`;

  await client.PUT("/definitions", {
    body: {
      name: child,
      tasks: [
        {
          id: "check",
          switch: [
            {
              case: "true",
              panic: {
                code: "script_broken",
                message: "the script is broken",
                // The case X2-a always had: a stack trace is unreadable in a one-line
                // message, and this is the only place it was ever going to be read.
                data: { kind: "syntax", stack: "at foo (x.ts:1:1)\nat bar (x.ts:9:2)" },
              },
            },
            { goto: "end" },
          ],
        },
      ],
    },
  });
  await client.PUT("/definitions", {
    body: {
      name: parent,
      tasks: [
        {
          id: "run",
          action: { type: "child" as const, name: child },
          switch: [{ goto: "end" }],
        },
      ],
    },
  });

  const { data: started } = await client.POST("/instances", { body: { process: parent } });
  const id = started!.id;
  expect(await waitForInstance(id)).toBe("failed");

  const { data: parentInst } = await client.GET("/instances/{id}", {
    params: { path: { id } },
  });
  // A panic poisons its ancestors, which inherit its code and message (§2.3) — but the
  // payload does not travel: copying it onto every ancestor bloats each row to say the
  // same thing, and nothing up there can catch a panic to read it anyway.
  expect(parentInst?.error_code).toBe("script_broken");
  expect((parentInst?.context?.error as any)?.data).toBeUndefined();

  const childId = (parentInst?.context as any)?._children?.run as string;
  const { data: childInst } = await client.GET("/instances/{id}", {
    params: { path: { id: childId } },
  });
  expect((childInst?.context?.error as any)?.data).toEqual({
    kind: "syntax",
    stack: "at foo (x.ts:1:1)\nat bar (x.ts:9:2)",
  });
});

// The reuse of the `error.data` slot has one sharp edge: whatever a rule CAUGHT is sitting
// there when the raise fires. A raise says what it carries — forwarding the caught body is
// one line, and a raise that says nothing carries nothing.
test("a raise forwards the caught body only when it asks to; otherwise the slot is cleared", async () => {
  const failing = await startMockService(0, {
    statusCode: 404,
    response: { detail: "no such order", code: "ORDER_MISSING" },
  });

  async function put(name: string, data?: components["schemas"]["ModelFault"]["data"]) {
    const raise: components["schemas"]["ModelFault"] = {
      code: "lookup_failed",
      message: "order lookup failed: ${error.data.detail}",
      ...(data !== undefined ? { data } : {}),
    };
    const { error } = await client.PUT("/definitions", {
      body: {
        name,
        tasks: [
          {
            id: "call",
            action: {
              type: "fetch" as const,
              url: `http://localhost:${failing.port}/orders/1`,
              method: "GET",
              responses: {
                200: { type: "object" },
                404: {
                  type: "object",
                  properties: { detail: { type: "string" }, code: { type: "string" } },
                  required: ["detail"],
                },
              },
            },
            timeout: 2000,
            on_error: [{ code: ["http.404"], raise }],
            switch: [{ goto: "end" }],
          },
        ],
      },
    });
    expect(error).toBeUndefined();
  }

  const uid = crypto.randomUUID().slice(0, 8);
  const forwards = `fault_fwd_${uid}`;
  const silent = `fault_silent_${uid}`;
  await put(forwards, "$: error.data");
  await put(silent);

  const ids: Record<string, string> = {};
  for (const name of [forwards, silent]) {
    const { data: started } = await client.POST("/instances", { body: { process: name } });
    ids[name] = started!.id;
    expect(await waitForInstance(started!.id)).toBe("raised");
  }

  const { data: fwd } = await client.GET("/instances/{id}", {
    params: { path: { id: ids[forwards] } },
  });
  expect((fwd?.context?.error as any)?.data).toEqual({
    detail: "no such order",
    code: "ORDER_MISSING",
  });

  const { data: quiet } = await client.GET("/instances/{id}", {
    params: { path: { id: ids[silent] } },
  });
  const err = quiet?.context?.error as any;
  // Cleared, not inherited: a parent that declares a shape for `lookup_failed` must never
  // receive a body this raise did not choose to send. The cause itself is still on the row.
  expect(err?.data, "a data-less raise carries nothing, not what it caught").toBeUndefined();
  expect(err?.code).toBe("http.404");
  expect(quiet?.error).toContain("no such order");

  failing.stop();
});

test("a data expression is type-checked at registration, in the clause's own scope", async () => {
  const name = `fault_data_check_${crypto.randomUUID()}`;
  const { error } = await client.PUT("/definitions", {
    body: {
      name,
      tasks: [
        {
          id: "decide",
          switch: [
            {
              case: "true",
              raise: {
                code: "nope",
                message: "m",
                data: { who: "$: input.missing" },
              },
            },
            { goto: "end" },
          ],
        },
      ],
    },
  });
  expect(
    JSON.stringify(error),
    "an unresolvable reference in `data` must be caught where every other shape's is",
  ).toContain("raise data");
});

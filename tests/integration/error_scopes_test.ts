import { expect, test } from "vitest";
import { client, startMockService, waitForInstance } from "../helpers/client.ts";

// `error` is what the rule CAUGHT; `last_error` is what ROUTED control here, and the only one a
// handler's own slots see. specs/task-scopes.md §The error axis.

const BODY = { type: "object", properties: { who: { type: "string" } }, required: ["who"] };

// `handler` was entered on `upstream`'s failure and then fails itself, so inside its rule the two
// errors are different values.
test("a rule sees the error it caught and the one that routed it here, at once", async () => {
  const upstream = await startMockService(0, { statusCode: 404, response: { who: "upstream" } });
  const downstream = await startMockService(0, { statusCode: 500, response: { who: "handler" } });

  const name = `err_scopes_both_${crypto.randomUUID()}`;
  const { error: putErr } = await client.PUT("/definitions", {
    body: {
      name,
      tasks: [
        {
          id: "call",
          action: {
            type: "fetch" as const,
            url: `http://localhost:${upstream.port}/`,
            method: "get",
            responses: { 200: {}, 404: BODY },
            timeout: 2000,
          },
          on_error: [{ code: ["http.404"], goto: "$handler" }],
          switch: [{ goto: "end" }],
        },
        {
          id: "handler",
          action: {
            type: "fetch" as const,
            url: `http://localhost:${downstream.port}/`,
            method: "get",
            responses: { 200: {}, 500: BODY },
            timeout: 2000,
          },
          on_error: [
            {
              code: ["http.500"],
              raise: {
                code: "both_seen",
                message: "caught ${error.data.who} while holding ${last_error.data.who}",
              },
            },
          ],
          switch: [{ goto: "end" }],
        },
      ],
    } as never,
  });
  expect(putErr, JSON.stringify(putErr)).toBeUndefined();

  const { data: started } = await client.POST("/instances", { body: { process: name } as never });
  expect(await waitForInstance(started!.id)).toBe("raised");
  const { data } = await client.GET("/instances/{id}/detail", { params: { path: { id: started!.id } } });

  expect(data?.error_message, "the rule names both failures, and they are not the same one").toBe(
    "caught handler while holding upstream",
  );

  await upstream.stop();
  await downstream.stop();
});

// The batch path writes the failure through a different door (collect.go, a raised CHILD).
test("a rule catching a raised child still sees the failure that routed the task here", async () => {
  const upstream = await startMockService(0, { statusCode: 404, response: { who: "upstream" } });

  const child = `err_scopes_child_${crypto.randomUUID()}`;
  await client.PUT("/definitions", {
    body: {
      name: child,
      tasks: [{ id: "no", switch: [{ raise: { code: "refused", message: "the child refused" } }] }],
    } as never,
  });

  const parent = `err_scopes_batch_${crypto.randomUUID()}`;
  const { error: putErr } = await client.PUT("/definitions", {
    body: {
      name: parent,
      tasks: [
        {
          id: "call",
          action: {
            type: "fetch" as const,
            url: `http://localhost:${upstream.port}/`,
            method: "get",
            responses: { 200: {}, 404: BODY },
            timeout: 2000,
          },
          on_error: [{ code: ["http.404"], goto: "$handler" }],
          switch: [{ goto: "end" }],
        },
        {
          id: "handler",
          action: { type: "child" as const, name: child, result_schema: {} },
          on_error: [
            {
              code: ["refused"],
              raise: {
                code: "both_seen",
                message: "child said ${error.code} while holding ${last_error.data.who}",
              },
            },
          ],
          switch: [{ goto: "end" }],
        },
      ],
    } as never,
  });
  expect(putErr, JSON.stringify(putErr)).toBeUndefined();

  const { data: started } = await client.POST("/instances", { body: { process: parent } as never });
  expect(await waitForInstance(started!.id)).toBe("raised");
  const { data } = await client.GET("/instances/{id}/detail", { params: { path: { id: started!.id } } });
  expect(data?.error_message).toBe("child said refused while holding upstream");

  await upstream.stop();
});

/** A definition whose `handler` task is entered by an error edge carrying `body`. */
function routed(name: string, task: Record<string, unknown>, body: unknown = BODY) {
  return {
    name,
    tasks: [
      {
        id: "call",
        action: { type: "fetch" as const, url: "http://127.0.0.1:1/", method: "get", responses: { 200: {}, 404: body }, timeout: 2000 },
        on_error: [{ code: ["http.404"], goto: "$handler" }],
        switch: [{ goto: "end" }],
      },
      { id: "handler", switch: [{ goto: "end" }], ...task },
    ],
  };
}

// Enforced at registration, so a definition still reading `error` in a handler fails loudly.
test("a handler's own slots read last_error, and `error` is not in scope there", async () => {
  const bad = await client.PUT("/definitions", {
    body: routed(`err_scopes_bad_${crypto.randomUUID()}`, { output: { code: "$: error.code" } }) as never,
  });
  expect(
    (bad.error as { error?: string } | undefined)?.error,
    "`error` belongs to a rule, not to the task it routes to, and the refusal says which field",
  ).toContain('field "error" not found in schema');

  const good = await client.PUT("/definitions", {
    body: routed(`err_scopes_good_${crypto.randomUUID()}`, {
      output: { code: "$: last_error.code" },
    }) as never,
  });
  expect(good.error, JSON.stringify(good.error)).toBeUndefined();
});

// A retry policy is evaluated in the rule's scope, like `case`: the same failure, matched then measured.
test("a retry policy reads the failure it is retrying", async () => {
  // Fails every time, and says how long to wait in the body — the shape of a Retry-After that
  // an endpoint puts in its payload rather than a header.
  const busy = await startMockService(0, { statusCode: 503, response: { wait: 10 } });

  const name = `err_scopes_retry_${crypto.randomUUID()}`;
  const { error: putErr } = await client.PUT("/definitions", {
    body: {
      name,
      tasks: [
        {
          id: "call",
          action: {
            type: "fetch" as const,
            url: `http://localhost:${busy.port}/`,
            method: "get",
            responses: {
              200: {},
              503: { type: "object", properties: { wait: { type: "number" } }, required: ["wait"] },
            },
            timeout: 2000,
          },
          on_error: [{ code: ["http.503"], retry: { retries: 2, delay: "$: error.data.wait" } }],
          switch: [{ goto: "end" }],
        },
      ],
    } as never,
  });
  expect(putErr, JSON.stringify(putErr)).toBeUndefined();

  const { data: started } = await client.POST("/instances", { body: { process: name } as never });
  expect(await waitForInstance(started!.id)).toBe("failed");
  const { data } = await client.GET("/instances/{id}/detail", { params: { path: { id: started!.id } } });

  // An unresolvable policy would fail on the first error with engine.expression and retry_count 0.
  expect(data?.error_code, JSON.stringify(data?.error_message)).toBe("http.503");
  expect(data?.retry_count, "the policy resolved and the budget was spent").toBe(2);
  expect(busy.requestCount()).toBe(3);

  await busy.stop();
}, 20_000);

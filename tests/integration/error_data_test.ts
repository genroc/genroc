import { expect, test } from "vitest";
import { client, startMockService, waitForInstance } from "../helpers/client.ts";

// A 4xx declared in `responses` is typed and routed at once: it still raises http.404 and
// still runs on_error, and the body it carried arrives at the handler as error.data. Without
// a declaration the same body is unreachable at any type — it survives only as the trimmed
// text in the audit trail.
test("error.data — a declared 4xx body reaches the handler that catches it", async () => {
  const failing = await startMockService(0, {
    statusCode: 404,
    response: { detail: "no such order", code: "ORDER_MISSING" },
  });

  const name = `error_data_${crypto.randomUUID()}`;
  const { error: putErr } = await client.PUT("/definitions", {
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
                properties: { detail: { type: "string" } },
                required: ["detail"],
              },
            },
          },
          on_error: [{ code: ["http.404"], goto: "$handler" }],
          timeout: 2000,
          switch: [{ goto: "end" }],
        },
        {
          id: "handler",
          // Read a field, not the whole value: the rule catches exactly one declared status,
          // so error.data is non-nullable here and a bare member access must type-check.
          output: { reason: "$: error.data.detail", code: "$: error.code" },
          switch: [{ goto: "end" }],
        },
      ],
    },
  });
  expect(putErr).toBeUndefined();

  const { data: started } = await client.POST("/instances", { body: { process: name } });
  const id = started!.id;
  expect(await waitForInstance(id)).toBe("completed");

  const { data } = await client.GET("/instances/{id}", { params: { path: { id } } });
  expect((data?.context?.outputs as any)?.handler).toEqual({
    reason: "no such order",
    code: "http.404",
  });

  failing.stop();
});

// The schema is enforced on the error channel too: a 404 whose body does not fit the shape
// the definition declared raises output.invalid INSTEAD of http.404, so the rule written for
// http.404 does not fire. Uniform with the success side, and the reason error.data can be
// non-nullable at all.
test("error.data — a declared 4xx body that does not conform replaces the status code", async () => {
  const failing = await startMockService(0, {
    statusCode: 404,
    response: { unexpected: true },
  });

  const name = `error_data_invalid_${crypto.randomUUID()}`;
  await client.PUT("/definitions", {
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
                properties: { detail: { type: "string" } },
                required: ["detail"],
              },
            },
          },
          on_error: [
            { code: ["http.404"], goto: "$wrong" },
            { code: ["output.invalid"], goto: "$handler" },
          ],
          timeout: 2000,
          switch: [{ goto: "end" }],
        },
        { id: "wrong", output: { took: "$: 'http404'" }, switch: [{ goto: "end" }] },
        { id: "handler", output: { took: "$: error.code" }, switch: [{ goto: "end" }] },
      ],
    },
  });

  const { data: started } = await client.POST("/instances", { body: { process: name } });
  const id = started!.id;
  expect(await waitForInstance(id)).toBe("completed");

  const { data } = await client.GET("/instances/{id}", { params: { path: { id } } });
  const outputs = (data?.context?.outputs ?? {}) as any;
  expect(outputs.wrong).toBeUndefined();
  expect(outputs.handler).toEqual({ took: "output.invalid" });

  failing.stop();
});

// `error` is scoped to the task its rule routed to, and the engine drops it on the next
// ordinary transition. Inference already refuses to type it downstream; this pins the other
// half — that the value is really gone from the stored context, rather than lingering there
// as a stale failure a later reader could still be served.
test("error — dropped from the context once the handler routes onward", async () => {
  const failing = await startMockService(0, { statusCode: 500, response: { boom: true } });

  const name = `error_scope_${crypto.randomUUID()}`;
  await client.PUT("/definitions", {
    body: {
      name,
      tasks: [
        {
          id: "call",
          action: { type: "fetch" as const, url: `http://localhost:${failing.port}/x`, method: "GET" },
          on_error: [{ code: ["http.%"], goto: "$handler" }],
          timeout: 2000,
          switch: [{ goto: "end" }],
        },
        // Carries what it needs forward explicitly, which is the supported way.
        { id: "handler", output: { code: "$: error.code" }, switch: [{ goto: "next" }] },
        { id: "after", output: { seen: "$: outputs.handler.code" }, switch: [{ goto: "end" }] },
      ],
    },
  });

  const { data: started } = await client.POST("/instances", { body: { process: name } });
  const id = started!.id;
  expect(await waitForInstance(id)).toBe("completed");

  const { data } = await client.GET("/instances/{id}", { params: { path: { id } } });
  const ctx = (data?.context ?? {}) as any;
  expect(ctx.outputs.after).toEqual({ seen: "http.500" });
  expect(ctx.error).toBeUndefined();

  failing.stop();
});

// error.data is persisted like a task output: inline while small, externalized to the object
// store past the cutoff, and resolved lazily on the way back. Two things to hold: the row
// must not swell with the body — an error payload is as large as any response — and a handler
// that reads it must still see the value, not the marker standing in for it.
test("error.data — a large error body externalizes and is still readable", async () => {
  const detail = "x".repeat(8000); // well past the ~2 KiB inline cutoff
  const failing = await startMockService(0, { statusCode: 422, response: { detail } });

  const name = `error_big_${crypto.randomUUID()}`;
  const { error: putErr } = await client.PUT("/definitions", {
    body: {
      name,
      tasks: [
        {
          id: "call",
          action: {
            type: "fetch" as const,
            url: `http://localhost:${failing.port}/x`,
            method: "GET",
            responses: {
              200: { type: "object" },
              422: {
                type: "object",
                properties: { detail: { type: "string" } },
                required: ["detail"],
              },
            },
          },
          on_error: [{ code: ["http.422"], goto: "$handler" }],
          timeout: 2000,
          switch: [{ goto: "end" }],
        },
        // A small verdict derived from the big body: if the marker were handed to the
        // expression unresolved, error.data.detail would read null and this would be false.
        { id: "handler", output: { readable: "$: error.data.detail != null" }, switch: [{ goto: "end" }] },
      ],
    },
  });
  expect(putErr).toBeUndefined();

  const { data: started } = await client.POST("/instances", { body: { process: name } });
  const id = started!.id;
  expect(await waitForInstance(id)).toBe("completed");

  const { data } = await client.GET("/instances/{id}", { params: { path: { id } } });
  const ctx = (data?.context ?? {}) as any;
  expect(ctx.outputs?.handler).toEqual({ readable: true });
  // Served as a reference, which is what says the body went to the object store instead of
  // swelling the instance row.
  expect(ctx.error?.data?.ref, "a body past the cutoff must externalize").toBeDefined();
  expect(ctx.error?.code).toBe("http.422");

  failing.stop();
});

import { expect, test } from "vitest";
import { client, startMockService, waitForInstance } from "../helpers/client.ts";

// What the 2xx default does once `responses` says something — the rule that applies when a
// task declares NO accepted_status. Asserted end to end because the runtime and inference
// have to agree about it, and they once did not: the engine kept accepting any 2xx while
// inference read the declared set, so an undeclared 2xx body reached self.result unvalidated,
// typed as something it had never been checked against.
const CLAUSES = [
  {
    name: "nothing declared — any 2xx succeeds, and no result is stored",
    responses: undefined,
    serve: { statusCode: 200, response: { fee: 1 } },
    caught: undefined, // no error
    resultReadable: false, // an undeclared body is neither readable nor exportable
  },
  {
    name: "a declared 2xx — only what it declares is accepted",
    responses: { 200: { type: "object", properties: { fee: { type: "number" } }, required: ["fee"] } },
    serve: { statusCode: 201, response: { fee: 1 } },
    caught: "http.201", // NOT a quiet success with an unchecked body
    resultReadable: true,
  },
  {
    name: "only an error status declared — the success side stays undeclared",
    // The trap: the 2xx default still accepts the response, so self.result must not be typed
    // `null` here — a real body arrives and nothing described it.
    responses: { 404: { type: "object", properties: { detail: { type: "string" } }, required: ["detail"] } },
    serve: { statusCode: 200, response: { fee: 1 } },
    caught: undefined,
    resultReadable: false,
  },
  {
    name: "a declared bodyless 2xx — accepted, and really is null",
    responses: { 202: null },
    serve: { statusCode: 202 },
    caught: undefined,
    resultReadable: true,
  },
] as const;

function definition(name: string, port: number, responses: unknown, output?: unknown) {
  const call: Record<string, unknown> = {
    id: "call",
    action: { type: "fetch", url: `http://localhost:${port}/x`, method: "GET", ...(responses ? { responses } : {}) },
    on_error: [{ code: ["http.%"], goto: "$caught" }],
    timeout: 2000,
    switch: [{ goto: "end" }],
  };
  if (output) call.output = output;
  return {
    name,
    tasks: [call, { id: "caught", output: { code: "$: error.code" }, switch: [{ goto: "end" }] }],
  };
}

for (const c of CLAUSES) {
  test(`responses default acceptance — ${c.name}`, async () => {
    const svc = await startMockService(0, c.serve as any);
    const name = `clause_${crypto.randomUUID()}`;

    const { error: putErr } = await client.PUT("/definitions", {
      body: definition(name, svc.port, c.responses) as any,
    });
    expect(putErr).toBeUndefined();

    const { data: started } = await client.POST("/instances", { body: { process: name } });
    const id = started!.id;
    expect(await waitForInstance(id)).toBe("completed");

    const { data } = await client.GET("/instances/{id}/detail", { params: { path: { id } } });
    const outputs = (data?.state?.outputs ?? {}) as any;
    expect(outputs.caught?.code).toBe(c.caught);

    // Whether the body may be read at all is the other half of the rule: a status nobody
    // described leaves self.result undeclared, and undeclared data is never accessible.
    const probe = await client.PUT("/definitions", {
      body: definition(`${name}_probe`, svc.port, c.responses, "$: self.result") as any,
    });
    if (c.resultReadable) {
      expect(probe.error, "declared status: self.result must be exportable").toBeUndefined();
    } else {
      expect(probe.error, "undeclared body: exporting self.result must be refused").toBeDefined();
    }

    svc.stop();
  });
}

// The declared bodyless status is a value, not a parse failure — the bug the feature began
// from, where a 202 carrying no body was accepted and then failed to decode.
test("responses — a bodyless 2xx reaches the definition as null", async () => {
  const svc = await startMockService(0, { statusCode: 202 });
  const name = `bodyless_${crypto.randomUUID()}`;
  await client.PUT("/definitions", {
    body: {
      name,
      tasks: [
        {
          id: "kick",
          action: {
            type: "fetch" as const,
            url: `http://localhost:${svc.port}/jobs`,
            responses: {
              200: { type: "object", properties: { id: { type: "string" } }, required: ["id"] },
              202: null,
            },
          },
          output: { started: "$: self.result == null" },
          timeout: 2000,
          switch: [{ goto: "end" }],
        },
      ],
    },
  });

  const { data: started } = await client.POST("/instances", { body: { process: name } });
  const id = started!.id;
  expect(await waitForInstance(id)).toBe("completed");

  const { data } = await client.GET("/instances/{id}/detail", { params: { path: { id } } });
  expect((data?.state?.outputs as any)?.kick).toEqual({ started: true });

  svc.stop();
});

// Only 2xx declarations influence the automatic accepted set, so declaring an error status
// alone leaves it at the 2xx default — the 404 stays an error, and stays typed. This is the
// shape for an endpoint whose success body you do not care about but whose failures you do.
test("responses — a lone error declaration types the failure without accepting it", async () => {
  const svc = await startMockService(0, {
    statusCode: 404,
    response: { detail: "no such order" },
  });

  const name = `lone_error_${crypto.randomUUID()}`;
  const { error: putErr } = await client.PUT("/definitions", {
    body: {
      name,
      tasks: [
        {
          id: "call",
          action: {
            type: "fetch" as const,
            url: `http://localhost:${svc.port}/orders/1`,
            method: "GET",
            responses: {
              404: {
                type: "object",
                properties: { detail: { type: "string" } },
                required: ["detail"],
              },
            },
          },
          on_error: [{ code: ["http.404"], goto: "$missing" }],
          timeout: 2000,
          switch: [{ goto: "end" }],
        },
        // error.data is non-nullable here: the rule catches exactly the one declared status.
        { id: "missing", output: { reason: "$: error.data.detail" }, switch: [{ goto: "end" }] },
      ],
    },
  });
  expect(putErr).toBeUndefined();

  const { data: started } = await client.POST("/instances", { body: { process: name } });
  const id = started!.id;
  expect(await waitForInstance(id)).toBe("completed");

  const { data } = await client.GET("/instances/{id}/detail", { params: { path: { id } } });
  expect((data?.state?.outputs as any)?.missing).toEqual({ reason: "no such order" });

  svc.stop();
});

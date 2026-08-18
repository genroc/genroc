import { expect, test } from "vitest";
import { client, startMockService, waitForInstance } from "../helpers/client.ts";

// Regression: a rest endpoint may contain {{ }} expressions (e.g. a base URL from
// config or input). Previously the endpoint was passed verbatim to the transport,
// so the request hit the literal template string and failed.
test("rest endpoint is evaluated as a template", async () => {
  const mock = await startMockService(0, { response: { slept: 1 } });

  const name = `endpoint_tmpl_${crypto.randomUUID()}`;
  const { error: putErr } = await client.PUT("/definitions", {
    body: {
      name,
      input_schema: {
        type: "object",
        properties: { base: { type: "string" } },
        required: ["base"],
      },
      tasks: [
        {
          id: "call",
          action: {
            type: "fetch" as const,
            url: "${ input.base }/action",
            responses: { 200: {
              type: "object",
              properties: { slept: { type: "number" } },
              required: ["slept"],
            } },
          },
          output: "$: self.result",
          switch: "end",
        },
      ],
      output: "$: outputs.call",
      // eslint-disable-next-line @typescript-eslint/no-explicit-any
    } as any,
  });
  expect(putErr).toBeUndefined();

  const { data: startData, error } = await client.POST("/instances", {
    body: { process: name, input: { base: `http://localhost:${mock.port}` } },
  });
  expect(error).toBeUndefined();
  const id = startData!.id;
  expect(await waitForInstance(id)).toBe("completed");

  const { data } = await client.GET("/instances/{id}", { params: { path: { id } } });
  // The request reached the mock at the resolved URL and returned its body.
  // eslint-disable-next-line @typescript-eslint/no-explicit-any
  expect((data?.context as any)?.output).toEqual({ slept: 1 });

  mock.stop();
});

// Regression for the playground: a config value used as the base URL in a rest
// endpoint. config.endpoint_url resolves from GENROC_GLOBAL_ENDPOINT_URL (set on the
// test server to http://localhost:14101 — a port dedicated to this file so it never
// clashes with secret_log_test's 14100 when Vitest runs the files in parallel), so
// the request reaches the mock there.
test("a config value can build a rest endpoint URL", async () => {
  const mock = await startMockService(14101, { response: { slept: 2 } });

  const name = `config_endpoint_${crypto.randomUUID()}`;
  const { error: putErr } = await client.PUT("/definitions", {
    body: {
      name,
      config_schema: {
        type: "object",
        required: ["endpoint_url"],
        properties: { endpoint_url: { type: "string" } },
      },
      tasks: [
        {
          id: "call",
          action: {
            type: "fetch" as const,
            url: "${ config.endpoint_url }/second",
            responses: { 200: {
              type: "object",
              properties: { slept: { type: "number" } },
              required: ["slept"],
            } },
          },
          output: "$: self.result",
          switch: "end",
        },
      ],
      output: "$: outputs.call",
      // eslint-disable-next-line @typescript-eslint/no-explicit-any
    } as any,
  });
  expect(putErr).toBeUndefined();

  const { data: startData, error } = await client.POST("/instances", {
    body: { process: name },
  });
  expect(error).toBeUndefined();
  const id = startData!.id;
  expect(await waitForInstance(id)).toBe("completed");

  const { data } = await client.GET("/instances/{id}", { params: { path: { id } } });
  // eslint-disable-next-line @typescript-eslint/no-explicit-any
  expect((data?.context as any)?.output).toEqual({ slept: 2 });

  mock.stop();
});

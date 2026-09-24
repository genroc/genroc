import { expect, test } from "vitest";
import { client, startMockService, waitForInstance } from "../helpers/client.ts";
import { startGenroc } from "../helpers/server.ts";

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
            method: "post",
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

  const { data } = await client.GET("/instances/{id}/detail", { params: { path: { id } } });
  // The request reached the mock at the resolved URL and returned its body.
  // eslint-disable-next-line @typescript-eslint/no-explicit-any
  expect((data?.output as any)).toEqual({ slept: 1 });

  mock.stop();
});

// Regression for the playground: a config value used as the base URL in a rest
// endpoint. config.endpoint_url resolves from GENROC_GLOBAL_ENDPOINT_URL, which is read at
// server START — so this test owns a server, with the mock's port baked into its env,
// rather than pinning a port on the shared one.
test("a config value can build a rest endpoint URL", async () => {
  const mock = await startMockService(0, { response: { slept: 2 } });
  const own = await startGenroc({ env: { GENROC_GLOBAL_ENDPOINT_URL: `http://localhost:${mock.port}` } });
  const api = own.client;

  const name = `config_endpoint_${crypto.randomUUID()}`;
  const { error: putErr } = await api.PUT("/definitions", {
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
            method: "post",
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

  const { data: startData, error } = await api.POST("/instances", {
    body: { process: name },
  });
  expect(error).toBeUndefined();
  const id = startData!.id;
  expect(await waitForInstance(id, undefined, api)).toBe("completed");

  const { data } = await api.GET("/instances/{id}/detail", { params: { path: { id } } });
  // eslint-disable-next-line @typescript-eslint/no-explicit-any
  expect((data?.output as any)).toEqual({ slept: 2 });

  await own.stop();
  mock.stop();
});

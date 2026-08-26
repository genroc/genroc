import { expect, test } from "vitest";
import { client, waitForInstance } from "../helpers/client.ts";

// Config vars resolve from GENROC_<PROCESS>_<NAME>, falling back to
// GENROC_GLOBAL_<NAME>, and are exposed to expressions under the "config"
// namespace. These tests use the global tier (process names are random).
// Fixtures set on the test server (see helpers/server.ts):
//   GENROC_GLOBAL_E2E_URL   = https://config.example.test
//   GENROC_GLOBAL_E2E_PORT  = 8080
//   GENROC_GLOBAL_E2E_TOKEN = supersecret-token-value

// Resolution + coercion + default end-to-end: a string passes through, an
// integer is coerced to a number, and an unset optional var falls back to its
// default — all reachable in expressions as config.<NAME>.
test("config resolves from the environment and is usable in expressions", async () => {
  const name = `config_resolve_${crypto.randomUUID()}`;
  const { error: putErr } = await client.PUT("/definitions", {
    body: {
      name,
      config_schema: {
        type: "object",
        required: ["e2e_url"],
        properties: {
          e2e_url: { type: "string" },
          e2e_port: { type: "integer" },
          e2e_region: { type: "string", default: "us" },
        },
      },
      tasks: [{ id: "route", switch: "end" }],
      output: {
        url: "$: config.e2e_url",
        port: "$: config.e2e_port",
        region: "$: config.e2e_region",
      },
      // eslint-disable-next-line @typescript-eslint/no-explicit-any
    } as any,
  });
  expect(putErr).toBeUndefined();

  const { data: startData } = await client.POST("/instances", {
    body: { process: name },
  });
  const id = startData!.id;
  expect(await waitForInstance(id)).toBe("completed");

  const { data } = await client.GET("/instances/{id}/detail", {
    params: { path: { id } },
  });
  // eslint-disable-next-line @typescript-eslint/no-explicit-any
  const output = (data?.state as any)?.output;
  expect(output.url).toBe("https://config.example.test");
  expect(output.port).toBe(8080); // coerced to a number, not the string "8080"
  expect(output.region).toBe("us"); // default applied (e2e_region unset)
});

// A required config var with no corresponding environment variable is rejected
// when the definition is registered, so the missing GENROC_<PROCESS>_<NAME> surfaces
// up front rather than waiting until an instance is started.
test("registering a definition fails when a required config var is unset", async () => {
  const name = `config_missing_${crypto.randomUUID()}`;
  const { data, error } = await client.PUT("/definitions", {
    body: {
      name,
      config_schema: {
        type: "object",
        required: ["e2e_not_set"],
        properties: { e2e_not_set: { type: "string" } },
      },
      tasks: [{ id: "route", switch: "end" }],
      // eslint-disable-next-line @typescript-eslint/no-explicit-any
    } as any,
  });
  expect(data).toBeUndefined();
  expect(error).toBeDefined();
  // eslint-disable-next-line @typescript-eslint/no-explicit-any
  expect(JSON.stringify(error)).toContain("e2e_not_set");
});

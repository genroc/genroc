import { expect, test } from "vitest";
import { client, waitForInstance } from "../helpers/client.ts";

// These use the GENROC_GLOBAL_ tier (process names are random); its E2E_URL, E2E_PORT and
// E2E_TOKEN fixtures are set in helpers/server.ts.

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
  const output = (data?.output as any);
  expect(output.url).toBe("https://config.example.test");
  expect(output.port).toBe(8080); // coerced to a number, not the string "8080"
  expect(output.region).toBe("us"); // default applied (e2e_region unset)
});

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

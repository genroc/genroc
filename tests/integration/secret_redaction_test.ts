import { expect, test } from "vitest";
import { client, waitForInstance } from "../helpers/client.ts";

// This test asserted the OPPOSITE until 2026-08-24: that a secret config value came back "***"
// over the API. `secret: true` now keeps a value out of the server's CONSOLE and nothing else —
// the durable trail and every API response carry what actually happened, because protecting a
// value at rest is encryption's job and redacting on read was never that. The console half is
// pinned by secret_log_test.ts, which is where the guarantee now lives.
// Fixture: GENROC_GLOBAL_API_KEY = "supersecret-api-key" (see helpers/server.ts).
// specs/object-store.md §Redaction.
test("a secret config value is returned by the API, not redacted", async () => {
  const name = `secret_redact_${crypto.randomUUID()}`;
  const { error: putErr } = await client.PUT("/definitions", {
    body: {
      name,
      config_schema: {
        type: "object",
        required: ["api_key"],
        properties: { api_key: { type: "string", secret: true } },
      },
      tasks: [{ id: "route", switch: "end" }],
      output: {
        auth: "Bearer ${ config.api_key }",
        note: "public value",
      },
      // eslint-disable-next-line @typescript-eslint/no-explicit-any
    } as any,
  });
  expect(putErr).toBeUndefined();

  const { data: startData } = await client.POST("/instances", { body: { process: name } });
  const id = startData!.id;
  expect(await waitForInstance(id)).toBe("completed");

  const { data } = await client.GET("/instances/{id}", { params: { path: { id } } });
  // eslint-disable-next-line @typescript-eslint/no-explicit-any
  const output = (data?.context as any)?.output;
  expect(output.note).toBe("public value");
  expect(output.auth).toBe("Bearer supersecret-api-key");
});

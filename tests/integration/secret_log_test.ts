import { expect, test } from "vitest";
import { client, startMockService, waitForInstance, fetchObject, objectAt } from "../helpers/client.ts";

// A secret config value used to build the endpoint URL must be redacted to "***"
// in the stored audit log — the raw secret must never reach the logs table.
// GENROC_GLOBAL_SERVER_URL = http://localhost:14100 (fixture in helpers/server.ts).
test("a secret config value in the endpoint URL is redacted in stored logs", async () => {
  const mock = await startMockService(14100, { response: { slept: 1 } });
  const name = `secret_url_log_${crypto.randomUUID()}`;

  await client.PUT("/definitions/batch", {
    body: {
      definitions: [
        {
          name,
          config_schema: {
            type: "object",
            required: ["server_url"],
            properties: { server_url: { type: "string", secret: true } },
          },
          tasks: [
            {
              id: "call",
              action: {
                type: "fetch",
                url: "${ config.server_url }/action",
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
        },
      ],
      channel: "latest",
    },
    // eslint-disable-next-line @typescript-eslint/no-explicit-any
  } as any);

  const { data: startData } = await client.POST("/instances", { body: { process: name } });
  const id = startData!.id;
  expect(await waitForInstance(id)).toBe("completed");

  const { data: logs } = await client.GET("/instances/{id}/logs", { params: { path: { id } } });
  const blob = JSON.stringify(logs);
  // The raw secret host must not be stored anywhere in the trail...
  expect(blob).not.toContain("localhost:14100");
  // ...but the redacted URL is kept (action_started meta.url).
  expect(blob).toContain("***/action");

  mock.stop();
});

// The obscuring is not config-specific: an input_schema secret used to build the
// URL is also redacted in the logs (via RedactContext over the eval context).
test("a secret INPUT value in the endpoint URL is redacted in stored logs", async () => {
  // A random port (not the pinned 14100) — the URL comes from input here, so this
  // test needs no fixed fixture, and keeping 14100 to a single test in this file
  // avoids racing its own not-yet-released listener when Vitest runs the cases.
  const mock = await startMockService(0, { response: { slept: 1 } });
  const name = `secret_input_url_${crypto.randomUUID()}`;

  await client.PUT("/definitions/batch", {
    body: {
      definitions: [
        {
          name,
          input_schema: {
            type: "object",
            required: ["base"],
            properties: { base: { type: "string", secret: true } },
          },
          tasks: [
            {
              id: "call",
              action: {
                type: "fetch",
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
        },
      ],
      channel: "latest",
    },
    // eslint-disable-next-line @typescript-eslint/no-explicit-any
  } as any);

  const base = `http://localhost:${mock.port}`;
  const { data: startData } = await client.POST("/instances", {
    body: { process: name, input: { base } },
  });
  const id = startData!.id;
  expect(await waitForInstance(id)).toBe("completed");

  const { data: logs } = await client.GET("/instances/{id}/logs", { params: { path: { id } } });
  const blob = JSON.stringify(logs);
  expect(blob).not.toContain(`localhost:${mock.port}`);
  expect(blob).toContain("***/action");

  mock.stop();
});

// A secret in the URL must not leak via a failed request's transport error either:
// net/http builds the error from the real URL, but the audit sink find/replaces
// every collected secret value out of the message before the log is stored.
test("a secret in a failed request's transport error is obscured in logs", async () => {
  const name = `secret_err_${crypto.randomUUID()}`;
  await client.PUT("/definitions/batch", {
    body: {
      definitions: [
        {
          name,
          input_schema: {
            type: "object",
            required: ["host"],
            properties: { host: { type: "string", secret: true } },
          },
          // No scheme → "unsupported protocol scheme" error built from the real URL.
          tasks: [{ id: "t", action: { type: "fetch", url: "${ input.host }/x" }, switch: "end" }],
        },
      ],
      channel: "latest",
    },
    // eslint-disable-next-line @typescript-eslint/no-explicit-any
  } as any);

  const { data: startData } = await client.POST("/instances", {
    body: { process: name, input: { host: "SECRET12345HOST" } },
  });
  const id = startData!.id;
  expect(await waitForInstance(id)).toBe("failed");

  const { data: logs } = await client.GET("/instances/{id}/logs", { params: { path: { id } } });
  expect(JSON.stringify(logs)).not.toContain("SECRET12345HOST");
});

// Regression: when secret values are nested prefixes of one another (e.g. an
// input array [5, 50, 500]), redaction must replace the longest value first.
// Replacing a shorter prefix first consumes the shared lead and leaves the longer
// secrets' tails exposed in the stored log ("***0", "***00"). The secret collector
// returns values longest-first so each is redacted whole.
test("nested-prefix secret values are fully redacted in stored logs", async () => {
  const name = `secret_prefix_${crypto.randomUUID()}`;
  await client.PUT("/definitions/batch", {
    body: {
      definitions: [
        {
          name,
          input_schema: { type: "array", items: { type: "string", secret: true } },
          tasks: [{ id: "route", switch: "end" }],
        },
      ],
      channel: "latest",
    },
    // eslint-disable-next-line @typescript-eslint/no-explicit-any
  } as any);

  // "AAAA" is a prefix of "AAAABBBB": redact the shorter first and the longer
  // leaks its "BBBB" tail as "***BBBB" in the instance_created input snippet.
  const { data: startData } = await client.POST("/instances", {
    body: { process: name, input: ["AAAA", "AAAABBBB"] },
  });
  const id = startData!.id;
  expect(await waitForInstance(id)).toBe("completed");

  const { data: logs } = await client.GET("/instances/{id}/logs", { params: { path: { id } } });
  const blob = JSON.stringify(logs);
  expect(blob).not.toContain("BBBB"); // the longer secret's tail must not survive
  expect(blob).not.toContain("AAAA");
});

// A secret inside a LARGE (externalized) value must still be scrubbed from logs.
// The token is >8KiB so it lives in the object store, not inline; on the advance that
// calls the action the input is a lazy marker, resolved into the request body. The
// secret reaches a log line (the request snippet) only via that resolution, so it is
// collected for scrubbing from the per-advance resolve cache — proving the lazy path
// doesn't open a leak.
test("a secret in a large (externalized) request body is redacted in stored logs", async () => {
  const mock = await startMockService(0, { response: { ok: 1 } });
  const name = `secret_big_body_${crypto.randomUUID()}`;
  const secret = "Z".repeat(9000); // exceeds the 2 KiB externalization threshold

  await client.PUT("/definitions/batch", {
    body: {
      definitions: [
        {
          name,
          input_schema: {
            type: "object",
            required: ["token"],
            properties: { token: { type: "string", secret: true } },
          },
          tasks: [
            {
              id: "call",
              action: {
                type: "fetch",
                url: `http://localhost:${mock.port}/x`,
                body: { auth: "$: input.token" },
                responses: { 200: {
                  type: "object",
                  properties: { ok: { type: "number" } },
                  required: ["ok"],
                } },
              },
              output: "$: self.result",
              switch: "end",
            },
          ],
        },
      ],
      channel: "latest",
    },
    // eslint-disable-next-line @typescript-eslint/no-explicit-any
  } as any);

  const { data: startData } = await client.POST("/instances", {
    body: { process: name, input: { token: secret } },
  });
  const id = startData!.id;
  expect(await waitForInstance(id)).toBe("completed");

  const { data: logs } = await client.GET("/instances/{id}/logs", {
    params: { path: { id }, query: { limit: 100 } },
  });
  // The raw secret must not survive anywhere in the trail — not inline, not in a
  // preview, and not in any externalized log object.
  expect(JSON.stringify(logs)).not.toContain("ZZZZZZZZZZ");
  mock.stop();
});

// Log payloads are redacted BEFORE the inline-vs-externalize decision: a payload
// that only exceeds the cap because of its secrets shrinks to a few bytes once
// scrubbed, so it must be stored inline — no log object is written for it.
test("a large payload that shrinks below the threshold after redaction is stored inline", async () => {
  const name = `secret_shrink_${crypto.randomUUID()}`;
  const secret = "X".repeat(4000); // alone exceeds the 2 KiB payload cap

  await client.PUT("/definitions/batch", {
    body: {
      definitions: [
        {
          name,
          input_schema: {
            type: "object",
            required: ["token"],
            properties: { token: { type: "string", secret: true } },
          },
          tasks: [{ id: "route", switch: "end" }],
        },
      ],
      channel: "latest",
    },
    // eslint-disable-next-line @typescript-eslint/no-explicit-any
  } as any);

  const { data: startData } = await client.POST("/instances", {
    body: { process: name, input: { token: secret } },
  });
  const id = startData!.id;
  expect(await waitForInstance(id)).toBe("completed");

  const { data: logs } = await client.GET("/instances/{id}/logs", { params: { path: { id } } });
  const created = (logs!.items ?? []).find((l) => l.event === "inst_created");
  expect(created).toBeDefined();
  // Redacted to {"token":"***"} — small enough to be carried inline, so nothing is listed.
  expect(objectAt(created, ["data"])).toBeUndefined();
  expect(created!.data).toContain("***");
  expect(JSON.stringify(logs)).not.toContain("XXXXXXXXXX");
});

// A payload still over the cap AFTER redaction is externalized — but the stored
// log object must hold the redacted content: resolving it inlines the scrubbed
// payload, never the raw secret.
test("an externalized log payload is stored pre-redacted", async () => {
  const name = `secret_blob_${crypto.randomUUID()}`;
  const secret = "SECRET_HIDDEN_VALUE_42";
  const pad = "P".repeat(4000); // keeps the payload over the cap even after redaction

  await client.PUT("/definitions/batch", {
    body: {
      definitions: [
        {
          name,
          input_schema: {
            type: "object",
            required: ["token", "pad"],
            properties: {
              token: { type: "string", secret: true },
              pad: { type: "string" },
            },
          },
          tasks: [{ id: "route", switch: "end" }],
        },
      ],
      channel: "latest",
    },
    // eslint-disable-next-line @typescript-eslint/no-explicit-any
  } as any);

  const { data: startData } = await client.POST("/instances", {
    body: { process: name, input: { token: secret, pad } },
  });
  const id = startData!.id;
  expect(await waitForInstance(id)).toBe("completed");

  const { data: logs } = await client.GET("/instances/{id}/logs", { params: { path: { id } } });
  const created = (logs!.items ?? []).find((l) => l.event === "inst_created");
  expect(created).toBeDefined();
  const listed = objectAt(created, ["data"]);
  expect(listed, "over the cap even after redaction, so it is externalized").toBeDefined();
  // Nothing in the listing itself leaks the secret.
  expect(JSON.stringify(logs)).not.toContain(secret);

  // Fetch the object: the stored payload is the SCRUBBED one. Redaction happens when the log is
  // written, so there is no unredacted version of it to serve.
  const full = await fetchObject(listed!.ref);
  expect(full).toContain("PPPPPPPP");
  expect(full).toContain("***");
  expect(full).not.toContain(secret);
});

import { spawn, type ChildProcess } from "child_process";
import { createServer } from "http";
import type { AddressInfo } from "net";
import { afterAll, beforeAll, expect, test } from "vitest";
import { buildGenrocBinary, tmpPath } from "../helpers/server.ts";
import { createClientTyped } from "../helpers/client.ts";

// `secret: true` has exactly one job: keep a value out of the server's STDOUT, where an operator
// reads it without having asked. Everything else — the durable trail, every API response —
// carries what actually happened, because protecting a value at rest is encryption's job and
// redacting on read was never that. specs/object-store.md §Redaction.
//
// This file needs its own server: the shared one runs at --log error with stdout discarded, and
// the whole assertion is about what reaches stdout.

const SECRET = "supersecret-api-key-value";
const PORT = 14140;

let server: ChildProcess;
let stdout = ""; // the server's console stream
let client: ReturnType<typeof createClientTyped>;
let mock: { port: number; stop: () => void };

beforeAll(async () => {
  const bin = await buildGenrocBinary();
  mock = await new Promise((resolve) => {
    const s = createServer((_req, res) => {
      res.writeHead(200, { "content-type": "application/json" });
      res.end("{}");
    });
    s.listen(0, () => resolve({ port: (s.address() as AddressInfo).port, stop: () => s.close() }));
  });

  // stderr, which is where the server's console handler writes (cmd/genroc/main.go). "stdout"
  // here means the operator's console either way -- what matters is that it is not the trail.
  server = spawn(bin, ["--db", tmpPath("secretlog", ".db"), "--http", `:${PORT}`, "--log", "debug"], {
    stdio: ["ignore", "ignore", "pipe"],
    env: { ...process.env, GENROC_GLOBAL_LOG_TOKEN: SECRET },
  });
  server.stderr!.on("data", (c: Buffer) => {
    stdout += c.toString();
  });
  client = createClientTyped({ baseUrl: `http://localhost:${PORT}/api` });
  // /healthz is root-mounted, so the readiness poll cannot go through the API client.
  const probe = createClientTyped({ baseUrl: `http://localhost:${PORT}` });
  for (let i = 0; i < 100; i++) {
    try {
      const { error } = await probe.GET("/healthz", {});
      if (!error) break;
    } catch {
      /* not up yet */
    }
    await new Promise((r) => setTimeout(r, 100));
  }
}, 90_000);

afterAll(() => {
  server?.kill();
  mock?.stop();
});

test("a secret config value is scrubbed from stdout and kept verbatim everywhere else", async () => {
  const name = `secretlog_${crypto.randomUUID()}`;
  const { error: putErr } = await client.PUT("/definitions", {
    body: {
      name,
      config_schema: {
        type: "object",
        properties: { log_token: { type: "string", secret: true, default: "" } },
      },
      tasks: [
        {
          id: "call",
          action: {
            type: "fetch" as const,
            // In the URL, so the secret reaches the log line through meta rather than only
            // through a payload snippet, which a deployment can turn off.
            url: `http://localhost:${mock.port}/\${ config.log_token }`,
            responses: { 200: {} },
          },
          timeout: 5000,
          switch: [{ goto: "end" }],
        },
      ],
    } as never,
  });
  expect(putErr, `put failed: ${JSON.stringify(putErr)}`).toBeUndefined();

  const { data: started } = await client.POST("/instances", { body: { process: name } });
  const id = started!.id;
  for (let i = 0; i < 200; i++) {
    const { data } = await client.GET("/instances/{id}", { params: { path: { id } } });
    if (data?.status !== "running") break;
    await new Promise((r) => setTimeout(r, 100));
  }

  // Stdout: scrubbed. This is the one place the marker acts.
  expect(stdout, "the server printed a secret to its console").not.toContain(SECRET);
  expect(stdout, "and it printed the placeholder in its place").toContain("***");

  // The durable trail: verbatim. A log is data the API returns, not a second redaction point —
  // and there is no unredacted version being withheld, because none was ever made.
  const { data: logs } = await client.GET("/instances/{id}/logs", {
    params: { path: { id }, query: { limit: 200 } },
  });
  const body = JSON.stringify(logs);
  expect(body, "the stored trail must carry what actually happened").toContain(SECRET);
});

test("secret: true is refused outside config_schema", async () => {
  // The scrubber finds secrets by knowing their values verbatim, which it can do for config and
  // cannot for anything a process computes. Accepting the marker elsewhere would promise a
  // protection nothing delivers, so registration refuses it rather than ignoring it.
  for (const [where, def] of [
    [
      "input_schema",
      {
        input_schema: { type: "object", properties: { token: { type: "string", secret: true } } },
        tasks: [{ id: "t", switch: [{ goto: "end" }] }],
      },
    ],
    [
      "a fetch response body",
      {
        tasks: [
          {
            id: "t",
            action: {
              type: "fetch",
              url: "http://localhost:1/x",
              responses: { 200: { type: "object", properties: { tok: { type: "string", secret: true } } } },
            },
            switch: [{ goto: "end" }],
          },
        ],
      },
    ],
  ] as const) {
    const { error } = await client.PUT("/definitions", {
      body: { name: `secret_scope_${crypto.randomUUID()}`, ...def } as never,
    });
    expect(error, `secret: true in ${where} must be refused`).toBeTruthy();
    expect(JSON.stringify(error)).toContain("config_schema");
  }
});

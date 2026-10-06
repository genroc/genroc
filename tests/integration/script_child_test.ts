import { afterAll, beforeAll, expect, test } from "vitest";
import { spawn, type ChildProcess } from "child_process";
import { readFileSync } from "node:fs";
import { join } from "path";
import { load as loadYaml } from "js-yaml";
import { client, waitForInstance } from "../helpers/client.ts";
import { BASE_URL } from "../helpers/constants.ts";

// The `script` child process (custom-tasks.md's middle tier), applied verbatim from the playground
// file; nothing else in the suite touches it.

const ROOT = new URL("../../", import.meta.url).pathname;
const script: any = loadYaml(readFileSync(join(ROOT, "tests/playground/script-node.genroc.yaml"), "utf8"));

let worker: ChildProcess;

beforeAll(async () => {
  const { error } = await client.PUT("/definitions", { body: script });
  expect(error, `the playground's script.yaml no longer registers: ${JSON.stringify(error)}`).toBeUndefined();

  // TASK scopes the fleet to script.yaml's own task id; an unfiltered worker would claim
  // every parked external task on the shared test server.
  worker = spawn("node", [join(ROOT, "eval-node/worker.ts")], {
    env: { ...process.env, GENROC_SERVER: BASE_URL, POLL_MS: "50", TASK: "eval_node", WORKER_ID: `child-${process.pid}` },
    stdio: ["ignore", "pipe", "inherit"],
  });
  await new Promise<void>((resolve, reject) => {
    const timer = setTimeout(() => reject(new Error("evaluator worker did not start within 10s")), 10_000);
    worker.stdout!.on("data", (c: Buffer) => {
      if (c.toString().includes("polling")) {
        clearTimeout(timer);
        resolve();
      }
    });
    worker.on("error", reject);
  });
}, 30_000);

afterAll(() => worker?.kill());

async function callScript(name: string, code: string, caller: Record<string, unknown>) {
  // The catch task exists only where the caller declared `raises`: undeclared, reading error.data
  // is refused at registration.
  const catches = "raises" in caller;
  const call: Record<string, unknown> = {
    id: "call",
    action: { type: "child" as const, name: script.name, input: { code, input: {} }, ...caller },
    output: "$: self.result",
    switch: [{ goto: "end" }],
  };
  if (catches) call.on_error = [{ code: ["script_threw"], goto: "$caught" }];
  const tasks: unknown[] = [call];
  if (catches) {
    tasks.push({
      id: "caught",
      // The name comes off the PAYLOAD and the text off error.MESSAGE — script.yaml
      // recomposes the refusal that way, and registration now holds it to it.
      output: { name: "$: last_error.data.name", message: "$: last_error.message" },
      switch: [{ goto: "end" }],
    });
  }
  const { error } = await client.PUT("/definitions", { body: { name, tasks } as never });
  expect(error, `put ${name} failed: ${JSON.stringify(error)}`).toBeUndefined();
  const { data: started } = await client.POST("/instances", { body: { process: name } });
  const status = await waitForInstance(started!.id, 30_000);
  const { data } = await client.GET("/instances/{id}/detail", { params: { path: { id: started!.id } } });
  return { status, data };
}

test("script child — a return value comes back through the wrapper, narrowed by result_schema", async () => {
  const { status, data } = await callScript(
    `script_child_ok_${crypto.randomUUID()}`,
    "export default () => ({ fee: 25, extra: 'dropped' });",
    { result_schema: { type: "object", properties: { fee: { type: "number" } }, required: ["fee"] } },
  );
  expect(status).toBe("completed");
  // The wrapper's own output is the top type; the CALLER's result_schema is what narrows it,
  // and the conform drops what the caller did not declare.
  expect((data?.state?.outputs as any)?.call).toEqual({ fee: 25 });
});

test("script child — the caller's timeout_ms is the one budget, and an overrun is script_timeout", async () => {
  const { status, data } = await callScript(
    `script_child_timeout_${crypto.randomUUID()}`,
    "export default () => { while (true) {} };",
    // Below the evaluator's 5000 default, so a worker ignoring the deadline answers too late and
    // the caller sees script_unknown instead.
    { result_schema: {}, input: { code: "export default () => { while (true) {} };", input: {}, timeout_ms: 1_500 } },
  );
  expect(status).toBe("failed");
  expect(data?.error_code, `an overrun must be told apart from no worker at all: ${data?.error_message}`).toBe("script_timeout");
});

// `script_threw` carries {name, stack} and puts the text on error.message; a declaration that reads
// the wrong slot yields null rather than the message.
test("script child — script_threw's name and text both reach a caller that declares it", async () => {
  const { status, data } = await callScript(
    `script_child_threw_${crypto.randomUUID()}`,
    "export default () => { const e = new Error('the sky is closed'); e.name = 'UpstreamError'; throw e; };",
    {
      result_schema: {},
      raises: {
        script_threw: {
          type: "object",
          // `stack` is nullable because the evaluator sends it only when the throw carried
          // one; requiring a plain string here is refused, which is the check working.
          properties: { name: { type: "string" }, stack: { type: ["string", "null"] } },
          required: ["name"],
        },
      },
    },
  );
  expect(status, `expected the caller's script_threw rule to fire: ${data?.error_message}`).toBe("completed");
  expect((data?.state?.outputs as any)?.caught).toEqual({
    name: "UpstreamError",
    message: "script threw (UpstreamError): the sky is closed",
  });
});

test("script child — a caller declaring a slot script_threw never sets is refused", async () => {
  const { error } = await client.PUT("/definitions", {
    body: {
      name: `script_child_bad_${crypto.randomUUID()}`,
      tasks: [
        {
          id: "call",
          action: {
            type: "child" as const,
            name: script.name,
            input: { code: "export default () => { throw new Error('x'); };", input: {} },
            raises: {
              script_threw: {
                type: "object",
                properties: { name: { type: "string" }, message: { type: "string" } },
                required: ["name", "message"],
              },
            },
          },
          switch: [{ goto: "end" }],
        },
      ],
    } as never,
  });
  expect(
    JSON.stringify(error),
    "the text is on error.message, not in the payload — a declaration that requires it there can never conform",
  ).toContain("message: declared required, never set");
});

test("script child — a broken script panics rather than raising something catchable", async () => {
  const { status, data } = await callScript(
    `script_child_broken_${crypto.randomUUID()}`,
    "export default () => {",
    { result_schema: {} },
  );
  // A panic is uncatchable by design: a script that will not compile is not a condition a
  // caller could react to, so it must not look like one.
  expect(status).toBe("failed");
  expect(data?.error_code).toBe("script_broken");
});

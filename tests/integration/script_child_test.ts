import { afterAll, beforeAll, expect, test } from "vitest";
import { spawn, type ChildProcess } from "child_process";
import { readFileSync } from "node:fs";
import { join } from "path";
import { load as loadYaml } from "js-yaml";
import { client, waitForInstance } from "../helpers/client.ts";
import { BASE_URL } from "../helpers/constants.ts";

// The `script` CHILD PROCESS — the documented wrapper a caller uses instead of writing an
// external task and its own error handling (custom-tasks.md's middle tier). The definition
// under test is the real playground file, applied verbatim, so this doubles as an executable
// check that it works: nothing else in the suite touches it, and it broke silently once when
// the evaluator moved its message from `data` onto `error.message`.

const ROOT = new URL("../../", import.meta.url).pathname;
const script: any = loadYaml(readFileSync(join(ROOT, "tests/playground/script.yaml"), "utf8"));

let worker: ChildProcess;

beforeAll(async () => {
  const { error } = await client.PUT("/definitions", { body: script });
  expect(error, `the playground's script.yaml no longer registers: ${JSON.stringify(error)}`).toBeUndefined();

  // TASK scopes the fleet to script.yaml's own task id; an unfiltered worker would claim
  // every parked external task on the shared test server.
  worker = spawn("node", [join(ROOT, "evaluator/worker.ts")], {
    env: { ...process.env, GENROC_SERVER: BASE_URL, POLL_MS: "50", TASK: "eval", WORKER_ID: `child-${process.pid}` },
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
  // The catch task only exists where the caller declared `raises`: without a declaration
  // error.data is absent, and reading it is refused at registration — which is the type system
  // doing exactly what the undeclared-code rule promises.
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
      output: { name: "$: error.data.name", message: "$: error.data.message" },
      switch: [{ goto: "end" }],
    });
  }
  const { error } = await client.PUT("/definitions", { body: { name, tasks } as never });
  expect(error, `put ${name} failed: ${JSON.stringify(error)}`).toBeUndefined();
  const { data: started } = await client.POST("/instances", { body: { process: name } });
  const status = await waitForInstance(started!.id, 30_000);
  const { data } = await client.GET("/instances/{id}", { params: { path: { id: started!.id } } });
  return { status, data };
}

test("script child — a return value comes back through the wrapper, narrowed by result_schema", async () => {
  const { status, data } = await callScript(
    `script_child_ok_${crypto.randomUUID()}`,
    "return { fee: 25, extra: 'dropped' };",
    { result_schema: { type: "object", properties: { fee: { type: "number" } }, required: ["fee"] } },
  );
  expect(status).toBe("completed");
  // The wrapper's own output is the top type; the CALLER's result_schema is what narrows it,
  // and the conform drops what the caller did not declare.
  expect((data?.context?.outputs as any)?.call).toEqual({ fee: 25 });
});

// The regression this file exists for. `script_threw` carries {name, message}, and the caller
// declares that shape under `raises` — a mismatch is not a wrong value but an output.invalid
// that takes the raised code away, so the caller's rule stops firing entirely.
test("script child — script_threw carries {name, message} for a caller that declares it", async () => {
  const { status, data } = await callScript(
    `script_child_threw_${crypto.randomUUID()}`,
    "const e = new Error('the sky is closed'); e.name = 'UpstreamError'; throw e;",
    {
      result_schema: {},
      raises: {
        script_threw: {
          type: "object",
          properties: { name: { type: "string" }, message: { type: "string" } },
          required: ["name", "message"],
        },
      },
    },
  );
  expect(status, `expected the caller's script_threw rule to fire: ${data?.error_message}`).toBe("completed");
  expect((data?.context?.outputs as any)?.caught).toEqual({
    name: "UpstreamError",
    message: "the sky is closed",
  });
});

test("script child — a broken script panics rather than raising something catchable", async () => {
  const { status, data } = await callScript(
    `script_child_broken_${crypto.randomUUID()}`,
    "return {",
    { result_schema: {} },
  );
  // A panic is uncatchable by design: a script that will not compile is not a condition a
  // caller could react to, so it must not look like one.
  expect(status).toBe("failed");
  expect(data?.error_code).toBe("script_broken");
});

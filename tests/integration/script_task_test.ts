import { afterAll, beforeAll, expect, test } from "vitest";
import { spawn, type ChildProcess } from "child_process";
import { join } from "path";
import { client, waitForInstance } from "../helpers/client.ts";
import { evaluate } from "../../bun-runtime/eval.ts";

// A script task is a `fetch` at a Bun evaluator — no new engine capability. What these
// tests pin is the seam between the two: the runner's STATUS is the retryability class
// on_error matches, and everything finer is a body field a switch branches on.
//
// See bun-runtime/README.md for the wire contract.

const ROOT = new URL("../../", import.meta.url).pathname;

let runner: ChildProcess;
let port: number;

beforeAll(async () => {
  runner = spawn("bun", [join(ROOT, "bun-runtime/server.ts")], {
    env: { ...process.env, PORT: "0" },
    stdio: ["ignore", "pipe", "inherit"],
  });
  port = await new Promise<number>((resolve, reject) => {
    const timer = setTimeout(() => reject(new Error("script runner did not report a port within 10s")), 10_000);
    runner.stdout!.on("data", (chunk: Buffer) => {
      const m = chunk.toString().match(/localhost:(\d+)/);
      if (m) {
        clearTimeout(timer);
        resolve(Number(m[1]));
      }
    });
    runner.on("error", reject);
  });
});

afterAll(() => runner?.kill());

/** A task calling the runner. `code` is passed through verbatim — mind `$${` (see below). */
function scriptTask(code: string, extra: Record<string, unknown> = {}) {
  return {
    id: "run",
    action: {
      type: "fetch" as const,
      url: `http://localhost:${port}/eval`,
      method: "POST",
      body: { code, ...extra } as Record<string, unknown>,
      responses: {} as Record<string, unknown>,
    },
    timeout: 5000,
  };
}

/** `$: input` only resolves against a declared input_schema, so the two always travel together. */
function withInput(t: ReturnType<typeof scriptTask>) {
  t.action.body.input = "$: input";
  return t;
}

/** The 422 body, as every script-task definition declares it. */
const SCRIPT_ERROR = {
  type: "object",
  properties: {
    kind: { type: "string" },
    name: { type: "string" },
    message: { type: "string" },
  },
  required: ["kind", "name", "message"],
};

const AMOUNT_SCHEMA = { type: "object", properties: { amount: { type: "number" } }, required: ["amount"] };
const NAME_SCHEMA = { type: "object", properties: { name: { type: "string" } }, required: ["name"] };

async function run(name: string, tasks: unknown[], input?: unknown, inputSchema?: unknown) {
  const body: Record<string, unknown> = { name, tasks };
  if (inputSchema) body.input_schema = inputSchema;
  const { error } = await client.PUT("/definitions", { body: body as never });
  expect(error).toBeUndefined();
  const { data: started } = await client.POST("/instances", { body: { process: name, input } as never });
  const id = started!.id;
  const status = await waitForInstance(id);
  const { data } = await client.GET("/instances/{id}", { params: { path: { id } } });
  return { status, data };
}

// The bare-value contract: the script's return value IS the 200 body, so `responses: {200: T}`
// types self.result as exactly T and a script task reads like a typed function call. An
// envelope here would cost every definition a `self.result.result`.
test("script task — the return value is self.result, typed by responses.200", async () => {
  const t = withInput(scriptTask("return { fee: input.amount * 0.1 };"));
  t.action.responses = {
    200: { type: "object", properties: { fee: { type: "number" } }, required: ["fee"] },
  };

  const { status, data } = await run(
    `script_ok_${crypto.randomUUID()}`,
    [{ ...t, output: { fee: "$: self.result.fee" }, switch: [{ goto: "end" }] }],
    { amount: 250 },
    AMOUNT_SCHEMA,
  );

  expect(status).toBe("completed");
  expect((data?.context?.outputs as any)?.run).toEqual({ fee: 25 });
});

// The headline path. A script cannot name a genroc error code — the wire only ever yields
// http.NNN — so a throw becomes http.422, its detail arrives as error.data, and the
// DEFINITION turns that into an authored code. This is the standardized error passing.
test("script task — a throw becomes http.422 and the definition raises a named code", async () => {
  const t = withInput(
    scriptTask(
      [
        "if (input.amount > 100) {",
        "  const e = new Error('amount over the limit');",
        "  e.name = 'LimitExceeded';",
        "  throw e;",
        "}",
        "return { fee: input.amount * 0.1 };",
      ].join("\n"),
    ),
  );
  t.action.responses = { 200: { type: "object" }, "422": SCRIPT_ERROR };

  const { status, data } = await run(
    `script_throw_${crypto.randomUUID()}`,
    [
      { ...t, on_error: [{ code: ["http.422"], goto: "$failed" }], switch: [{ goto: "end" }] },
      {
        id: "failed",
        switch: [
          // A raise code is a literal (never an expression), so mapping a thrown error onto
          // one is a switch here rather than anything the runner could do.
          {
            case: `error.data.name == "LimitExceeded"`,
            raise: { code: "limit_exceeded", message: "the script rejected the amount" },
          },
          { goto: "end" },
        ],
      },
    ],
    { amount: 250 },
    AMOUNT_SCHEMA,
  );

  expect(status).toBe("raised");
  expect(data?.error_code).toBe("limit_exceeded");
});

// Compile errors and throws deliberately SHARE the 422 status: both are permanent, so
// splitting them across statuses would only invite an on_error rule that retries one of
// them. `kind` is what tells them apart, and it reaches a switch through error.data.
test("script task — a compile error shares 422 and is told apart by error.data.kind", async () => {
  const t = scriptTask("return {{{;");
  t.action.responses = { 200: { type: "object" }, "422": SCRIPT_ERROR };

  const { status, data } = await run(`script_compile_${crypto.randomUUID()}`, [
    { ...t, on_error: [{ code: ["http.422"], goto: "$failed" }], switch: [{ goto: "end" }] },
    {
      id: "failed",
      output: { kind: "$: error.data.kind", name: "$: error.data.name" },
      switch: [{ goto: "end" }],
    },
  ]);

  expect(status).toBe("completed");
  expect((data?.context?.outputs as any)?.failed).toEqual({ kind: "compile_error", name: "SyntaxError" });
});

// Retries re-execute, so an unpinned clock makes attempt two differ from attempt one. The
// runner pins `now` rather than deleting Date — deleting it would leave the generated types
// asserting what the runtime contradicts.
test("script task — Date.now() reads the pinned clock the caller passed", async () => {
  const pinned = 1755600000000;
  const t = scriptTask("return { now: Date.now(), year: new Date().getUTCFullYear() };", { now: pinned });
  t.action.responses = {
    200: {
      type: "object",
      properties: { now: { type: "integer" }, year: { type: "integer" } },
      required: ["now", "year"],
    },
  };

  const { status, data } = await run(`script_clock_${crypto.randomUUID()}`, [
    { ...t, output: { now: "$: self.result.now", year: "$: self.result.year" }, switch: [{ goto: "end" }] },
  ]);

  expect(status).toBe("completed");
  expect((data?.context?.outputs as any)?.run).toEqual({ now: pinned, year: 2025 });
});

// The trap every script author hits: a fetch body is a Shape, so `${` is genroc's
// interpolation marker and a JS template literal inside the code string is read by genroc
// rather than passed through. `$${` is the escape.
//
// The template interpolates a LAMBDA PARAMETER on purpose. A `${input.name}` would be a
// valid genroc expression too, so both layers would produce the same string and the test
// would discriminate nothing; `x` exists only inside the script, so the unescaped form
// cannot even register. That is what makes the escape necessary rather than stylistic.
const TEMPLATE_SCRIPT = (marker: string) =>
  `const items = ['a', 'b'];\nreturn { tags: items.map((x) => \`<${marker}{x}>\`).join('') };`;

test("script task — a template literal in the code escapes ${ as $${", async () => {
  const t = withInput(scriptTask(TEMPLATE_SCRIPT("$$")));
  t.action.responses = {
    200: { type: "object", properties: { tags: { type: "string" } }, required: ["tags"] },
  };

  const { status, data } = await run(
    `script_tmpl_${crypto.randomUUID()}`,
    [{ ...t, output: { tags: "$: self.result.tags" }, switch: [{ goto: "end" }] }],
    { name: "world" },
    NAME_SCHEMA,
  );

  expect(status).toBe("completed");
  expect((data?.context?.outputs as any)?.run).toEqual({ tags: "<a><b>" });
});

test("script task — the unescaped ${ is read by genroc and refused at registration", async () => {
  const t = withInput(scriptTask(TEMPLATE_SCRIPT("$")));
  const { error } = await client.PUT("/definitions", {
    body: {
      name: `script_tmpl_raw_${crypto.randomUUID()}`,
      input_schema: NAME_SCHEMA,
      tasks: [{ ...t, switch: [{ goto: "end" }] }],
    } as never,
  });

  // Registration must refuse it rather than silently shipping a mangled script: `x` is a
  // JS binding genroc cannot resolve, and this is the only point where that is visible.
  expect(JSON.stringify(error)).toContain("x");
});

// `Date` and `Math` are handed to the script as Proxies, and a Proxy forwards writes to its
// target — so without the write-refusing traps a script assigning `Math.random` patches the
// PROCESS-WIDE global and every later request reads the patch. Called directly rather than
// over HTTP: the leak is between two evaluations in one process, which is what this asserts.
test("evaluator — a script cannot patch the real Math or Date", async () => {
  const realRandom = Math.random;
  const realNow = Date.now;

  for (const code of ["Math.random = () => 42; return 1;", "Date.now = () => 0; return 1;"]) {
    const r = await evaluate({ code, now: 1755600000000, seed: "s" });
    expect(r.ok, `${code} must be refused, not silently applied`).toBe(false);
  }

  expect(Math.random, "the script poisoned the process-wide Math.random").toBe(realRandom);
  expect(Date.now, "the script poisoned the process-wide Date.now").toBe(realNow);

  // The pinning itself still works — refusing writes must not have broken the reads.
  const ok = await evaluate({ code: "return { d: Date.now() };", now: 1755600000000, seed: "s" });
  expect(ok).toEqual({ ok: true, body: JSON.stringify({ d: 1755600000000 }) });
});

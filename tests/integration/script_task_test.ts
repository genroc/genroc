import { afterAll, beforeAll, expect, test } from "vitest";
import { spawn, type ChildProcess } from "child_process";
import { join } from "path";
import { connect } from "node:net";
import { client, waitForInstance } from "../helpers/client.ts";

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
      const m = chunk.toString().match(/listening on http:\/\/\S+:(\d+)/);
      if (m) {
        clearTimeout(timer);
        resolve(Number(m[1]));
      }
    });
    runner.on("error", reject);
  });
});

afterAll(() => runner?.kill());

// The runner must outlast any caller's connection pool, and it is the caller — the side
// that knows whether a request is in flight — that must hang up first. Bun's 10s default
// sat exactly on a 10s polling cadence: every tick was a coin flip on whether the server's
// close raced the next request into a reset, which a POST cannot retry. genroc pools
// connections for 90s (internal/transport), so the runner may not close at all.
test("runner — never hangs up on an idle keep-alive connection", { timeout: 25_000 }, async () => {
  const socket = connect({ port, host: "127.0.0.1" });
  await new Promise<void>((resolve, reject) => {
    socket.once("connect", () => resolve());
    socket.once("error", reject);
  });

  const body = JSON.stringify({ code: "return 1" });
  socket.write(
    `POST /eval HTTP/1.1\r\nHost: localhost\r\nContent-Type: application/json\r\n` +
      `Content-Length: ${Buffer.byteLength(body)}\r\n\r\n${body}`,
  );
  await new Promise<void>((resolve) => socket.once("data", () => resolve()));

  let hungUp = false;
  socket.once("close", () => {
    hungUp = true;
  });
  // Past the ~12s at which the old default fired, so this bites rather than passes by luck.
  await new Promise((resolve) => setTimeout(resolve, 13_000));
  socket.destroy();

  expect(
    hungUp,
    "the runner closed an idle connection; a caller that reuses it races that close into a " +
      "reset it cannot retry, which is how a transient blip fails a whole process",
  ).toBe(false);
});

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

/** One evaluation, straight at the runner — no definition in the way. */
async function evalDirect(body: Record<string, unknown>) {
  const res = await fetch(`http://localhost:${port}/eval`, {
    method: "POST",
    headers: { "content-type": "application/json" },
    body: JSON.stringify(body),
  });
  return { status: res.status, body: await res.json() };
}

// The runner hands the script the realm's OWN globals — no pinned clock, no seeded RNG.
// Called directly rather than over HTTP: what this asserts is the relationship between two
// evaluations in one process, which no single definition can see.
test("evaluator — Math and Date are the realm's own, and die with it", async () => {
  const patched = await evalDirect({ code: "Math.random = () => 0.5; return Math.random();" });
  expect(patched.status, "a script owns its realm — patching a global in it is not a fault").toBe(200);
  expect(patched.body).toBe(0.5);

  const next = await evalDirect({ code: "return Math.random();" });
  expect(next.body, "the patch must have died with the realm that made it").not.toBe(0.5);

  const again = await evalDirect({ code: "return Math.random();" });
  expect(again.body, "two executions must draw differently — the RNG is not seeded per request").not.toBe(next.body);

  const clock = await evalDirect({ code: "return Date.now();" });
  expect(Math.abs((clock.body as number) - Date.now()), "the script reads the wall clock").toBeLessThan(5_000);
});

// ── the realm ───────────────────────────────────────────────────────────────────
//
// One Worker per execution. These pin the three things that buys, each of which the previous
// in-process evaluator could not do. See bun-runtime/README.md.

test("realm — a synchronous busy loop is bounded and the runner keeps serving", async () => {
  const t0 = Date.now();
  const r = await evalDirect({ code: "while (true) {}", timeout_ms: 400 });
  const elapsed = Date.now() - t0;

  // The whole reason the realm is a thread: no in-process timer can interrupt a loop that
  // never yields, so before this the runner hung forever on exactly this input.
  expect(r.status, `a busy loop must fault, not hang (took ${elapsed}ms)`).toBe(422);
  expect((r.body as { kind: string }).kind).toBe("timeout");
  expect(elapsed, "the budget must be enforced, not merely reported").toBeLessThan(3_000);

  const next = await evalDirect({ code: "return { alive: true };" });
  expect(next.status, "the killed thread must not have taken the runner with it").toBe(200);
  expect(next.body).toEqual({ alive: true });
});

test("realm — a script that ends its own realm faults instead of hanging", async () => {
  const r = await evalDirect({ code: "process.exit(7); return 1;", timeout_ms: 5_000 });

  // Without the close event this returned nothing at all and the caller waited out the full
  // budget for an answer that was never coming.
  expect(r.status).toBe(422);
  expect((r.body as { kind: string }).kind).toBe("exited");
  expect((await evalDirect({ code: "return { alive: true };" })).status).toBe(200);
});

test("realm — one execution cannot leave state behind for the next", async () => {
  const wrote = await evalDirect({ code: "globalThis.__leak = 'poison'; return { wrote: true };" });
  expect(wrote.status, "the write itself is allowed — it is the realm's own global").toBe(200);

  const read = await evalDirect({ code: "return { leak: globalThis.__leak ?? null };" });
  expect(read.body, "a fresh realm per execution is what stops one script configuring another").toEqual({
    leak: null,
  });
});

test("realm — a script can require a node builtin", async () => {
  // What the bundler emits for `import { platform } from "node:os"`: builtins are externalised
  // as `require`, and the realm binds one. Under the old browser target this was rewritten to
  // `{}` and failed at runtime with no diagnostic.
  const r = await evalDirect({ code: 'const os = require("node:os"); return { platform: os.platform() };' });
  expect(r.status, JSON.stringify(r.body)).toBe(200);
  expect(typeof (r.body as { platform: string }).platform).toBe("string");
});

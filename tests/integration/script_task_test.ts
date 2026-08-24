import { afterAll, beforeAll, expect, test } from "vitest";
import { spawn, type ChildProcess } from "child_process";
import { join } from "path";
import { client, waitForInstance } from "../helpers/client.ts";
import { BASE_URL } from "../helpers/constants.ts";
import { evaluate } from "../../evaluator/eval.ts";

// A script task is an `external` task: the engine parks and holds no worker, and an evaluator
// claims it off the queue, evaluates it in its own realm, and answers. What these tests pin is
// the seam — the failure KIND is the error code an on_error rule matches, so the definition
// never reads a payload to find out what went wrong.
//
// The realm's own properties are asserted by calling evaluate() directly at the bottom: they
// are relationships between two evaluations in one process, which no definition can observe.
// See evaluator/README.md.

const ROOT = new URL("../../", import.meta.url).pathname;

let worker: ChildProcess;

beforeAll(async () => {
  worker = spawn("node", [join(ROOT, "evaluator/worker.ts")], {
    // TASK scopes the fleet to this file's script tasks. Without a filter a worker claims
    // every parked external task on the server, including other suites' approvals — which is
    // the same mistake a real deployment makes when one fleet shares a genroc with another.
    env: { ...process.env, GENROC_SERVER: BASE_URL, POLL_MS: "50", TASK: "run", WORKER_ID: `test-${process.pid}` },
    stdio: ["ignore", "pipe", "inherit"],
  });
  await new Promise<void>((resolve, reject) => {
    const timer = setTimeout(() => reject(new Error("evaluator worker did not start within 10s")), 10_000);
    worker.stdout!.on("data", (chunk: Buffer) => {
      if (chunk.toString().includes("polling")) {
        clearTimeout(timer);
        resolve();
      }
    });
    worker.on("error", reject);
  });
});

afterAll(() => worker?.kill());

// The closed set an evaluator may answer with: the failure kinds in evaluator/eval.ts. A
// definition declares the ones it handles; a code outside this is refused at submission.
const SCRIPT_ERROR = { type: "object", properties: { name: { type: "string" } }, required: ["name"] };
const ALL_KINDS = {
  threw: SCRIPT_ERROR,
  timeout: SCRIPT_ERROR,
  compile_error: SCRIPT_ERROR,
  nonserializable: SCRIPT_ERROR,
  exited: SCRIPT_ERROR,
};

/** A task handing `code` to an evaluator. `code` is passed through verbatim — mind `$${`. */
function scriptTask(code: string, extra: Record<string, unknown> = {}) {
  return {
    id: "run",
    action: {
      type: "external" as const,
      input: { code, ...extra } as Record<string, unknown>,
      result_schema: {} as Record<string, unknown>,
      raises: ALL_KINDS as Record<string, unknown>,
    },
    // Above the evaluator's own budget, so an overrun comes back classified rather than as
    // external.timeout — which is unknowable, and so never retryable.
    timeout: 20_000,
  };
}

function withInput(t: ReturnType<typeof scriptTask>) {
  t.action.input.input = "$: input";
  return t;
}

const AMOUNT_SCHEMA = { type: "object", properties: { amount: { type: "number" } }, required: ["amount"] };

async function run(name: string, tasks: unknown[], input?: unknown, inputSchema?: unknown) {
  const body: Record<string, unknown> = { name, tasks };
  if (inputSchema) body.input_schema = inputSchema;
  const { error } = await client.PUT("/definitions", { body: body as never });
  expect(error, `put definition failed: ${JSON.stringify(error)}`).toBeUndefined();
  const { data: started } = await client.POST("/instances", { body: { process: name, input } as never });
  const id = started!.id;
  const status = await waitForInstance(id, 30_000);
  const { data } = await client.GET("/instances/{id}", { params: { path: { id } } });
  return { status, data };
}

// The bare-value contract: the script's return value IS the result, so `result_schema` types
// self.result and a script task reads like a typed function call. An envelope here would cost
// every definition a `self.result.result`.
test("script task — the return value is self.result, typed by result_schema", async () => {
  const t = withInput(scriptTask("return { fee: input.amount * 0.1 };"));
  t.action.result_schema = { type: "object", properties: { fee: { type: "number" } }, required: ["fee"] };

  const { status, data } = await run(
    `script_ok_${crypto.randomUUID()}`,
    [{ ...t, output: { fee: "$: self.result.fee" }, switch: [{ goto: "end" }] }],
    { amount: 250 },
    AMOUNT_SCHEMA,
  );

  expect(status).toBe("completed");
  expect((data?.context?.outputs as any)?.run).toEqual({ fee: 25 });
});

// The headline path, and what the error channel bought: the KIND is the code, so `code: [threw]`
// catches a throw directly. Under the old fetch shape every failure shared http.422 and the
// definition had to switch on error.data.kind to tell them apart.
test("script task — a throw is caught by its own code, and the definition raises a named one", async () => {
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

  const tasks = [
    { ...t, on_error: [{ code: ["threw"], goto: "$failed" }], switch: [{ goto: "end" }] },
    {
      id: "failed",
      switch: [
        {
          case: 'error.data.name == "LimitExceeded"',
          raise: { code: "limit_exceeded", message: "the script rejected the amount" },
        },
        { raise: { code: "script_failed", message: "the script failed" } },
      ],
    },
  ];

  const { status, data } = await run(`script_throw_${crypto.randomUUID()}`, tasks, { amount: 250 }, AMOUNT_SCHEMA);
  expect(status).toBe("raised");
  expect(data?.error_code).toBe("limit_exceeded");
});

// Three kinds that are all "the script is broken", each with its own code — so one arm can
// cover them without any of them being indistinguishable from a throw.
test("script task — compile_error, nonserializable and exited are distinct codes", async () => {
  for (const [label, code, want] of [
    ["a syntax error", "return {", "compile_error"],
    ["a cyclic return", "const a = {}; a.self = a; return a;", "nonserializable"],
    ["ending its own realm", "process.exit(7); return 1;", "exited"],
  ] as const) {
    const t = scriptTask(code);
    const tasks = [
      { ...t, on_error: [{ code: ["compile_error", "nonserializable", "exited"], goto: "$broken" }], switch: [{ goto: "end" }] },
      { id: "broken", output: { code: "$: error.code" }, switch: [{ goto: "end" }] },
    ];
    const { status, data } = await run(`script_${want}_${crypto.randomUUID()}`, tasks);
    expect(status, `${label} should complete via the handler`).toBe("completed");
    expect((data?.context?.outputs as any)?.broken, label).toEqual({ code: want });
  }
}, 60_000);

// The evaluator's own budget, enforced by killing the realm. It must come back as the
// classified `timeout` code, not as the task's external.timeout.
test("script task — a script over its budget reports `timeout`, not external.timeout", async () => {
  const t = scriptTask("while (true) {}", { timeout_ms: 400 });
  const tasks = [
    { ...t, on_error: [{ code: ["timeout"], goto: "$slow" }], switch: [{ goto: "end" }] },
    { id: "slow", output: { code: "$: error.code" }, switch: [{ goto: "end" }] },
  ];
  const { status, data } = await run(`script_timeout_${crypto.randomUUID()}`, tasks);
  expect(status).toBe("completed");
  expect((data?.context?.outputs as any)?.slow).toEqual({ code: "timeout" });
}, 30_000);

// The task input is a Shape, so `${` is genroc's interpolation marker and a JS template
// literal has to be escaped. Moving the code into a .ts file removes this — genctl doubles
// every `$` on splice. See specs/typed-values.md.
// The interpolated binding is the SCRIPT's, not the task context's — which is the case that
// actually bites, since a template literal usually reads a local.
const TEMPLATE_SCRIPT = (dollars: string) =>
  ["const who = input.name;", "return { greeting: `hi " + dollars + "{who}` };"].join("\n");

test("script task — a template literal in the code escapes ${ as $${", async () => {
  const t = withInput(scriptTask(TEMPLATE_SCRIPT("$$")));
  const { status, data } = await run(
    `script_escape_${crypto.randomUUID()}`,
    [{ ...t, output: "$: self.result", switch: [{ goto: "end" }] }],
    { name: "ada" },
    { type: "object", properties: { name: { type: "string" } }, required: ["name"] },
  );
  expect(status).toBe("completed");
  expect((data?.context?.outputs as any)?.run).toEqual({ greeting: "hi ada" });
});

test("script task — the unescaped ${ is read by genroc and refused at registration", async () => {
  const t = withInput(scriptTask(TEMPLATE_SCRIPT("$")));
  const { error } = await client.PUT("/definitions", {
    body: {
      name: `script_unescaped_${crypto.randomUUID()}`,
      input_schema: { type: "object", properties: { name: { type: "string" } }, required: ["name"] },
      tasks: [{ ...t, output: "$: self.result", switch: [{ goto: "end" }] }],
    } as never,
  });
  // Registration must refuse it rather than silently shipping a mangled script: `who` is a JS
  // binding genroc cannot resolve, and this is the only point where that is visible.
  expect(JSON.stringify(error), "an unescaped ${ must be refused, not silently interpolated").toContain("who");
});

// The queue's own property, and the reason for the move: the worker decides how many scripts
// run at once, so a backlog forms rather than overwhelming the evaluator.
test("script task — a backlog drains", async () => {
  const name = `script_backlog_${crypto.randomUUID()}`;
  const t = withInput(scriptTask("return { doubled: input.n * 2 };"));
  await client.PUT("/definitions", {
    body: {
      name,
      input_schema: { type: "object", properties: { n: { type: "number" } }, required: ["n"] },
      tasks: [{ ...t, output: "$: self.result", switch: [{ goto: "end" }] }],
    } as never,
  });

  const ids = await Promise.all(
    Array.from({ length: 8 }, async (_, i) => {
      const { data } = await client.POST("/instances", { body: { process: name, input: { n: i } } as never });
      return { id: data!.id, n: i };
    }),
  );
  for (const { id, n } of ids) {
    expect(await waitForInstance(id, 40_000)).toBe("completed");
    const { data } = await client.GET("/instances/{id}", { params: { path: { id } } });
    expect((data?.context?.outputs as any)?.run).toEqual({ doubled: n * 2 });
  }
}, 60_000);

// ── the realm ───────────────────────────────────────────────────────────────────
//
// One Worker per execution. These call evaluate() directly: each asserts a relationship
// between two evaluations in one process, which no single definition can see — and eval.ts is
// kept free of any genroc knowledge precisely so it can be driven like this.

test("evaluator — a return value JSON cannot represent is the script's fault", async () => {
  const r = await evaluate({ code: "const a = {}; a.self = a; return a;" });
  // Serialising INSIDE the realm is what makes this a classified failure rather than an
  // exception thrown out of the answer path.
  expect(r.ok).toBe(false);
  expect(r.ok === false && r.failure.kind).toBe("nonserializable");
});

test("evaluator — Math and Date are the realm's own, and die with it", async () => {
  const patched = await evaluate({ code: "Math.random = () => 0.5; return Math.random();" });
  expect(patched.ok, "a script owns its realm — patching a global in it is not a fault").toBe(true);
  expect(patched.ok === true && JSON.parse(patched.body)).toBe(0.5);

  const next = await evaluate({ code: "return Math.random();" });
  const nextValue = next.ok === true ? JSON.parse(next.body) : null;
  expect(nextValue, "the patch must have died with the realm that made it").not.toBe(0.5);

  const again = await evaluate({ code: "return Math.random();" });
  expect(again.ok === true && JSON.parse(again.body), "two executions must draw differently — the RNG is not seeded per request").not.toBe(nextValue);

  const clock = await evaluate({ code: "return Date.now();" });
  expect(Math.abs((clock.ok === true ? JSON.parse(clock.body) : 0) - Date.now()), "the script reads the wall clock").toBeLessThan(5_000);
});

// `stack` is renumbered to the lines the AUTHOR wrote. The compiled body sits under a wrapper
// whose preamble is ENGINE-specific, so the offset is measured at startup rather than assumed
// (evaluator/realm.ts) — a stack pointing confidently at the wrong line is worse than none.
test("stack — a throw reports the author's line, and the function that threw", async () => {
  const code = [
    "const rate = 0.1;", //          1
    "function fee(amount) {", //     2
    "  throw new Error('nope');", // 3
    "}", //                          4
    "return fee(10);", //            5
  ].join("\n");

  const r = await evaluate({ code });
  const stack = (r.ok === false && r.failure.stack) || "";
  expect(stack, `the throw is on line 3 of what the author wrote:\n${stack}`).toContain("at fee (script:3:");
  expect(stack, `the call is on line 5, and the top-level frame carries no name:\n${stack}`).toContain("at (script:5:");
});

test("stack — the runner's own frames and file path stay out of it", async () => {
  const r = await evaluate({ code: "\n\nthrow new Error('boom');" });
  const stack = (r.ok === false && r.failure.stack) || "";
  expect(stack, `line 3, and nothing above it:\n${stack}`).toContain("(script:3:");
  expect(stack, `a script's author cannot act on the runner's plumbing:\n${stack}`).not.toMatch(
    /realm\.ts|node:internal|MessagePort|evaluator\//,
  );
});

test("realm — a synchronous busy loop is bounded and the evaluator keeps working", async () => {
  const t0 = Date.now();
  const r = await evaluate({ code: "while (true) {}", timeout_ms: 400 });
  const elapsed = Date.now() - t0;

  // The whole reason the realm is a thread: no in-process timer can interrupt a loop that
  // never yields, so before this the runner hung forever on exactly this input.
  expect(r.ok, `a busy loop must fault, not hang (took ${elapsed}ms)`).toBe(false);
  expect(r.ok === false && r.failure.kind).toBe("timeout");
  expect(elapsed, "the budget must be enforced, not merely reported").toBeLessThan(3_000);

  const next = await evaluate({ code: "return { alive: true };" });
  expect(next.ok, "the killed thread must not have taken the process with it").toBe(true);
});

test("realm — a script that ends its own realm faults instead of hanging", async () => {
  const r = await evaluate({ code: "process.exit(7); return 1;", timeout_ms: 5_000 });
  // Without the close event this returned nothing at all and the caller waited out the full
  // budget for an answer that was never coming.
  expect(r.ok).toBe(false);
  expect(r.ok === false && r.failure.kind).toBe("exited");
});

test("realm — one execution cannot leave state behind for the next", async () => {
  const wrote = await evaluate({ code: "globalThis.__leak = 'poison'; return { wrote: true };" });
  expect(wrote.ok, "the write itself is allowed — it is the realm's own global").toBe(true);

  const read = await evaluate({ code: "return { leak: globalThis.__leak ?? null };" });
  expect(read.ok === true && JSON.parse(read.body), "a fresh realm per execution is what stops one script configuring another").toEqual({ leak: null });
});

test("realm — a script can require a node builtin", async () => {
  // What the bundler emits for `import { platform } from "node:os"`: builtins are externalised
  // as `require`, and the realm binds one. Under the old browser target this was rewritten to
  // `{}` and failed at runtime with no diagnostic.
  const r = await evaluate({ code: 'const os = require("node:os"); return { platform: os.platform() };' });
  expect(r.ok, JSON.stringify(r)).toBe(true);
  expect(typeof (r.ok === true && JSON.parse(r.body).platform)).toBe("string");
});

// The bundle case this whole store exists for: a script past the inline cutoff is stored ONCE as
// an object and listed by each task rather than copied into every instance. The worker follows
// the reference and caches it by content hash, which never invalidates.
test("script task — a large script is shared, not copied, and still runs", async () => {
  const name = `script_big_${crypto.randomUUID()}`;
  // Well past the 2 KiB cutoff, and identical across instances — which is what makes it one
  // object with many claims instead of one copy each.
  const pad = Array.from({ length: 400 }, (_, i) => `const pad_${i} = "${"x".repeat(64)}";`).join("\n");
  const t = withInput(scriptTask(`${pad}\nreturn { doubled: input.n * 2 };`));

  await client.PUT("/definitions", {
    body: {
      name,
      input_schema: { type: "object", properties: { n: { type: "number" } }, required: ["n"] },
      tasks: [{ ...t, output: "$: self.result", switch: [{ goto: "end" }] }],
    } as never,
  });

  const ids = await Promise.all(
    [1, 2, 3].map(async (n) => {
      const { data } = await client.POST("/instances", { body: { process: name, input: { n } } as never });
      return { id: data!.id, n };
    }),
  );

  for (const { id, n } of ids) {
    expect(await waitForInstance(id, 40_000)).toBe("completed");
    const { data } = await client.GET("/instances/{id}", { params: { path: { id } } });
    expect((data?.context?.outputs as any)?.run).toEqual({ doubled: n * 2 });
  }
}, 60_000);

// The property the test above cannot see: it passes whether or not the bundle is externalized,
// because a script carried inline still runs. What must be pinned is that the code LEFT the
// instance — one object, listed by every task, instead of a copy in each.
//
// The task id is deliberately not "run", so this file's worker (TASK=run) leaves it parked and
// the queue can be inspected.
test("script task — a large script leaves the instance and is listed, not carried", async () => {
  const name = `script_shared_${crypto.randomUUID()}`;
  const pad = Array.from({ length: 400 }, (_, i) => `const pad_${i} = "${"x".repeat(64)}";`).join("\n");
  const code = `${pad}\nreturn { ok: true };`;

  await client.PUT("/definitions", {
    body: {
      name,
      input_schema: { type: "object", properties: { n: { type: "number" } }, required: ["n"] },
      tasks: [
        {
          id: "parked",
          action: { type: "external" as const, input: { code, input: "$: input" }, result_schema: {} },
          switch: [{ goto: "end" }],
        },
      ],
    } as never,
  });
  await Promise.all(
    [1, 2].map((n) => client.POST("/instances", { body: { process: name, input: { n } } as never })),
  );

  const deadline = Date.now() + 20_000;
  let entries: any[] = [];
  while (Date.now() < deadline) {
    const { data } = await client.GET("/external-tasks", { params: { query: { process: name } } });
    entries = ((data as any)?.items ?? []) as any[];
    if (entries.length === 2) break;
    await new Promise((r) => setTimeout(r, 50));
  }
  expect(entries.length, "both instances parked").toBe(2);

  for (const e of entries) {
    // The code is gone from the input and named by the entry instead.
    expect(e.input?.code, "the bundle must not be carried in the task input").toBeUndefined();
    const listed = (e.objects ?? []).find(
      (o: any) => o.path.length === 2 && o.path[0] === "input" && o.path[1] === "code",
    );
    expect(listed, "the bundle is listed at input.code").toBeDefined();
    expect(listed.size).toBeGreaterThan(code.length - 100);
    // The per-instance half stays inline: externalizing the whole input would fold it in and
    // give every instance a different hash, which is the sharing this exists for, lost.
    expect(e.input?.input?.n).toBeGreaterThan(0);
  }

  // One object, two tasks: byte-identical code across instances is stored once.
  const refs = new Set(entries.map((e) => e.objects[0].ref));
  expect(refs.size, "both instances name the SAME object — the bundle is stored once").toBe(1);

  // And it is fetchable by that hash, which is how a worker gets it.
  const { data: obj } = await client.GET("/objects/{ref}", {
    params: { path: { ref: [...refs][0] } },
  });
  expect(JSON.parse((obj as any).data)).toBe(code);
}, 40_000);

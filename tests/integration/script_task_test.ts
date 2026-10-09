import { claimInProcess, parkedInProcess } from "../helpers/external.ts";
import { afterAll, beforeAll, expect, test } from "vitest";
import { spawn, type ChildProcess } from "child_process";
import { rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "path";
import { client, waitForInstance } from "../helpers/client.ts";
import { BASE_URL } from "../helpers/constants.ts";
import { evaluate } from "../../eval-node/eval.ts";

// A script task is an `external` task an evaluator claims; the failure KIND is the code on_error
// matches. Realm properties are asserted via evaluate() at the bottom (specs/script-tasks.md).

const ROOT = new URL("../../", import.meta.url).pathname;

let worker: ChildProcess;
// One process for every test the worker serves: a claim names its process, so the worker cannot
// follow per-test names. Each test registers its own version, and an instance runs the one it
// started on.
const PROCESS = `script_task_${crypto.randomUUID()}`;

beforeAll(async () => {
  worker = spawn("node", [join(ROOT, "eval-node/worker.ts")], {
    env: { ...process.env, GENROC_SERVER: BASE_URL, POLL_MS: "50", PROCESS, TASK: "run", WORKER_ID: `test-${process.pid}` },
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

// The closed set an evaluator may answer with: the failure kinds in eval-node/eval.ts. A
// definition declares the ones it handles; a code outside this is refused at submission.
const SCRIPT_ERROR = { type: "object", properties: { name: { type: "string" } }, required: ["name"] };
const ALL_KINDS = {
  threw: SCRIPT_ERROR,
  timeout: SCRIPT_ERROR,
  compile_error: SCRIPT_ERROR,
  nonserializable: SCRIPT_ERROR,
  exited: SCRIPT_ERROR,
};

/** A task handing `code` to an evaluator: an ES module whose default export the realm calls
 *  with `input`. Passed through verbatim — mind `$${`. */
function scriptTask(code: string, extra: Record<string, unknown> = {}) {
  return {
    id: "run",
    action: {
      type: "external" as const,
      input: { code, ...extra } as Record<string, unknown>,
      result_schema: {} as Record<string, unknown>,
      raises: ALL_KINDS as Record<string, unknown>,
      // The script's budget too: the worker runs it against this deadline.
      timeout: 20_000,
    },
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
  const { data } = await client.GET("/instances/{id}/detail", { params: { path: { id } } });
  return { status, data };
}

test("script task — the return value is self.result, typed by result_schema", async () => {
  const t = withInput(scriptTask("export default (input) => ({ fee: input.amount * 0.1 });"));
  t.action.result_schema = { type: "object", properties: { fee: { type: "number" } }, required: ["fee"] };

  const { status, data } = await run(
    PROCESS,
    [{ ...t, output: { fee: "$: self.result.fee" }, switch: [{ goto: "end" }] }],
    { amount: 250 },
    AMOUNT_SCHEMA,
  );

  expect(status).toBe("completed");
  expect((data?.state?.outputs as any)?.run).toEqual({ fee: 25 });
});

test("script task — a throw is caught by its own code, and the definition raises a named one", async () => {
  const t = withInput(
    scriptTask(
      [
        "export default function (input) {",
        "  if (input.amount > 100) {",
        "    const e = new Error('amount over the limit');",
        "    e.name = 'LimitExceeded';",
        "    throw e;",
        "  }",
        "  return { fee: input.amount * 0.1 };",
        "}",
      ].join("\n"),
    ),
  );

  const tasks = [
    { ...t, on_error: [{ code: ["threw"], goto: "$failed" }], switch: [{ goto: "end" }] },
    {
      id: "failed",
      switch: [
        {
          case: 'last_error.data.name == "LimitExceeded"',
          raise: { code: "limit_exceeded", message: "the script rejected the amount" },
        },
        { raise: { code: "script_failed", message: "the script failed" } },
      ],
    },
  ];

  const { status, data } = await run(PROCESS, tasks, { amount: 250 }, AMOUNT_SCHEMA);
  expect(status).toBe("raised");
  expect(data?.error_code).toBe("limit_exceeded");
});

test("script task — error.data.name is the error's class unless the script names it", async () => {
  for (const [label, thrown, want] of [
    ["a custom class", "class LimitExceeded extends Error {}; throw new LimitExceeded('x');", "LimitExceeded"],
    ["a set name over the class", "class A extends Error { name = 'Chosen' }; throw new A('x');", "Chosen"],
    ["a set name on a plain Error", "const e = new Error('x'); e.name = 'Tagged'; throw e;", "Tagged"],
    ["a plain Error", "throw new Error('x');", "Error"],
    ["a built-in", "null.x;", "TypeError"],
    ["a non-Error", "throw { code: 'x' };", "Thrown"],
  ] as const) {
    const tasks = [
      {
        ...scriptTask(`export default function () { ${thrown} }`),
        on_error: [{ code: ["threw"], goto: "$failed" }],
        switch: [{ goto: "end" }],
      },
      { id: "failed", output: { name: "$: last_error.data.name" }, switch: [{ goto: "end" }] },
    ];
    const { status, data } = await run(PROCESS, tasks);
    expect(status, label).toBe("completed");
    expect((data?.state?.outputs as any)?.failed?.name, `${label}: a caller matches on this name`).toBe(want);
  }
});

// No default export is compile_error too: nothing ran, and only editing the script helps.
test("script task — compile_error, nonserializable and exited are distinct codes", async () => {
  for (const [label, code, want] of [
    ["a syntax error", "export default () => {", "compile_error"],
    ["no default export", "export const fee = () => 1;", "compile_error"],
    ["a cyclic return", "export default () => { const a = {}; a.self = a; return a; };", "nonserializable"],
    ["ending its own realm", "export default () => process.exit(7);", "exited"],
  ] as const) {
    const t = scriptTask(code);
    const tasks = [
      { ...t, on_error: [{ code: ["compile_error", "nonserializable", "exited"], goto: "$broken" }], switch: [{ goto: "end" }] },
      { id: "broken", output: { code: "$: last_error.code" }, switch: [{ goto: "end" }] },
    ];
    const { status, data } = await run(PROCESS, tasks);
    expect(status, `${label} should complete via the handler`).toBe("completed");
    expect((data?.state?.outputs as any)?.broken, label).toEqual({ code: want });
  }
}, 60_000);

test("script task — a script over the task's timeout reports `timeout`, not external.timeout", async () => {
  const t = scriptTask("export default () => { while (true) {} };");
  // Below the evaluator's 5000 default: only a worker budgeting by the deadline answers in time.
  t.action.timeout = 1_500;
  const tasks = [
    { ...t, on_error: [{ code: ["timeout"], goto: "$slow" }], switch: [{ goto: "end" }] },
    { id: "slow", output: { code: "$: last_error.code" }, switch: [{ goto: "end" }] },
  ];
  const { status, data } = await run(PROCESS, tasks);
  expect(status).toBe("completed");
  expect((data?.state?.outputs as any)?.slow).toEqual({ code: "timeout" });
}, 30_000);

// The task input is a Shape, so a JS template literal's `${` must be escaped (specs/typed-values.md).
// The binding is the SCRIPT's own, not the task context's — the case that actually bites.
const TEMPLATE_SCRIPT = (dollars: string) =>
  [
    "export default function (input) {",
    "  const who = input.name;",
    "  return { greeting: `hi " + dollars + "{who}` };",
    "}",
  ].join("\n");

test("script task — a template literal in the code escapes ${ as $${", async () => {
  const t = withInput(scriptTask(TEMPLATE_SCRIPT("$$")));
  const { status, data } = await run(
    PROCESS,
    [{ ...t, output: "$: self.result", switch: [{ goto: "end" }] }],
    { name: "ada" },
    { type: "object", properties: { name: { type: "string" } }, required: ["name"] },
  );
  expect(status).toBe("completed");
  expect((data?.state?.outputs as any)?.run).toEqual({ greeting: "hi ada" });
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
  // `who` is a JS binding genroc cannot resolve.
  expect(JSON.stringify(error), "an unescaped ${ must be refused, not silently interpolated").toContain("who");
});

test("script task — a backlog drains", async () => {
  const name = PROCESS;
  const t = withInput(scriptTask("export default (input) => ({ doubled: input.n * 2 });"));
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
    const { data } = await client.GET("/instances/{id}/detail", { params: { path: { id } } });
    expect((data?.state?.outputs as any)?.run).toEqual({ doubled: n * 2 });
  }
}, 60_000);

// ── the realm ───────────────────────────────────────────────────────────────────
// eval.ts is kept free of genroc knowledge so it can be driven directly like this.

test("evaluator — a return value JSON cannot represent is the script's fault", async () => {
  const r = await evaluate({ code: "export default () => { const a = {}; a.self = a; return a; };" });
  // Serialising INSIDE the realm is what makes this a classified failure rather than an
  // exception thrown out of the answer path.
  expect(r.ok).toBe(false);
  expect(r.ok === false && r.failure.kind).toBe("nonserializable");
});

// eval.ts terminates the Worker as the reply lands, so the stdio pipe can lose what a script
// printed LAST. Asserted from a child process because stdout itself is under test.
test("output — everything a script prints survives the realm's termination", async () => {
  const script = [
    "export default () => {",
    "  console.log('CONSOLE-FIRST');",
    // Big enough that the pipe is still draining it when the thread is killed.
    "  console.log(new Array(200).fill({ k: 'v'.repeat(30) }));",
    "  process.stdout.write('DIRECT-BIG:' + 'x'.repeat(20000) + '\\n');",
    "  process.stderr.write('DIRECT-ERR\\n');",
    "  process.stdout.write('DIRECT-LAST\\n');",
    "  console.log('CONSOLE-LAST');",
    "  return 1;",
    "};",
  ].join("\n");
  const runner = [
    `import { evaluate } from ${JSON.stringify(new URL("../../eval-node/eval.ts", import.meta.url).href)};`,
    `const r = await evaluate({ code: ${JSON.stringify(script)} });`,
    "if (!r.ok) throw new Error('evaluate failed: ' + JSON.stringify(r));",
  ].join("\n");

  // A file rather than `-e`: --input-type is inherited by the Worker eval.ts starts, and a
  // Worker loading a URL refuses to run under it.
  const runnerPath = join(tmpdir(), `genroc-output-${process.pid}-${Date.now()}.mjs`);
  writeFileSync(runnerPath, runner);
  const { out, err } = await new Promise<{ out: string; err: string }>((resolve, reject) => {
    const child = spawn("node", [runnerPath], { stdio: ["ignore", "pipe", "pipe"] });
    let out = "";
    let err = "";
    child.stdout.on("data", (c: Buffer) => (out += c.toString()));
    child.stderr.on("data", (c: Buffer) => (err += c.toString()));
    child.on("error", reject);
    child.on("close", (code) => (code === 0 ? resolve({ out, err }) : reject(new Error(`runner exited ${code}: ${err}`))));
  }).finally(() => rmSync(runnerPath, { force: true }));

  expect(out, "the first write always landed — its survival was never the question").toContain("CONSOLE-FIRST");
  expect(out, "an object keeps console's own formatting").toContain("k: 'vvv");
  expect(out, "a direct stream write is the author's output too, not just console").toContain("DIRECT-BIG:");
  expect(out, "the last direct write is the one the pipe loses when the realm is killed").toContain("DIRECT-LAST");
  expect(out, "and the last console line with it").toContain("CONSOLE-LAST");
  expect(err, "stderr stays stderr rather than folding into stdout").toContain("DIRECT-ERR");
  // Both channels are the same stream, so a fix that rescued only one would reorder them.
  expect(out.indexOf("DIRECT-LAST"), "order is the stream's own").toBeLessThan(out.indexOf("CONSOLE-LAST"));
}, 30_000);

test("evaluator — Math and Date are the realm's own, and die with it", async () => {
  const patched = await evaluate({ code: "export default () => { Math.random = () => 0.5; return Math.random(); };" });
  expect(patched.ok, "a script owns its realm — patching a global in it is not a fault").toBe(true);
  expect(patched.ok === true && JSON.parse(patched.body)).toBe(0.5);

  const next = await evaluate({ code: "export default () => Math.random();" });
  const nextValue = next.ok === true ? JSON.parse(next.body) : null;
  expect(nextValue, "the patch must have died with the realm that made it").not.toBe(0.5);

  const again = await evaluate({ code: "export default () => Math.random();" });
  expect(again.ok === true && JSON.parse(again.body), "two executions must draw differently — the RNG is not seeded per request").not.toBe(nextValue);

  const clock = await evaluate({ code: "export default () => Date.now();" });
  expect(Math.abs((clock.ok === true ? JSON.parse(clock.body) : 0) - Date.now()), "the script reads the wall clock").toBeLessThan(5_000);
});

// The script is imported as a module, not compiled from a string, so there is no wrapper
// preamble offset to correct for (eval-node/realm.ts).
test("stack — a throw reports the author's line, and the function that threw", async () => {
  const code = [
    "const rate = 0.1;", //             1
    "function fee(amount) {", //        2
    "  throw new Error('nope');", //    3
    "}", //                             4
    "export default () => fee(10);", // 5
  ].join("\n");

  const r = await evaluate({ code });
  const stack = (r.ok === false && r.failure.stack) || "";
  expect(stack, `the throw is on line 3 of what the author wrote:\n${stack}`).toContain("at fee (script:main:3:");
  expect(stack, `the call is on line 5:\n${stack}`).toContain("(script:main:5:");
});

test("stack — the runner's own frames and file path stay out of it", async () => {
  const r = await evaluate({ code: "\n\nexport default () => { throw new Error('boom'); };" });
  const stack = (r.ok === false && r.failure.stack) || "";
  expect(stack, `line 3, and nothing above it:\n${stack}`).toContain("(script:main:3:");
  expect(stack, `a script's author cannot act on the runner's plumbing:\n${stack}`).not.toMatch(
    /realm\.ts|node:internal|MessagePort|evaluator\//,
  );
});

test("realm — a synchronous busy loop is bounded and the evaluator keeps working", async () => {
  const t0 = Date.now();
  const r = await evaluate({ code: "export default () => { while (true) {} };", timeout_ms: 400 });
  const elapsed = Date.now() - t0;

  expect(r.ok, `a busy loop must fault, not hang (took ${elapsed}ms)`).toBe(false);
  expect(r.ok === false && r.failure.kind).toBe("timeout");
  expect(elapsed, "the budget must be enforced, not merely reported").toBeLessThan(3_000);

  const next = await evaluate({ code: "export default () => ({ alive: true });" });
  expect(next.ok, "the killed thread must not have taken the process with it").toBe(true);
});

test("realm — a script that ends its own realm faults instead of hanging", async () => {
  const r = await evaluate({ code: "export default () => process.exit(7);", timeout_ms: 5_000 });
  // Relies on the Worker's close event; without it the caller waits out the full budget.
  expect(r.ok).toBe(false);
  expect(r.ok === false && r.failure.kind).toBe("exited");
});

test("realm — one execution cannot leave state behind for the next", async () => {
  const wrote = await evaluate({ code: "export default () => { globalThis.__leak = 'poison'; return { wrote: true }; };" });
  expect(wrote.ok, "the write itself is allowed — it is the realm's own global").toBe(true);

  const read = await evaluate({ code: "export default () => ({ leak: globalThis.__leak ?? null });" });
  expect(read.ok === true && JSON.parse(read.body), "a fresh realm per execution is what stops one script configuring another").toEqual({ leak: null });
});

test("realm — a script can import a node builtin", async () => {
  // The bundler externalises builtins, so the realm must resolve them as ordinary imports.
  const r = await evaluate({
    code: 'import { platform } from "node:os";\nexport default () => ({ platform: platform() });',
  });
  expect(r.ok, JSON.stringify(r)).toBe(true);
  expect(typeof (r.ok === true && JSON.parse(r.body).platform)).toBe("string");
});

test("script task — a large script is shared, not copied, and still runs", async () => {
  const name = PROCESS;
  // Past the 2 KiB cutoff, and identical across instances so it is stored as one object.
  const pad = Array.from({ length: 400 }, (_, i) => `const pad_${i} = "${"x".repeat(64)}";`).join("\n");
  const t = withInput(scriptTask(`${pad}\nexport default (input) => ({ doubled: input.n * 2 });`));

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
    const { data } = await client.GET("/instances/{id}/detail", { params: { path: { id } } });
    expect((data?.state?.outputs as any)?.run).toEqual({ doubled: n * 2 });
  }
}, 60_000);

// The test above passes even if the bundle stays inline; this pins that it LEFT the instance.
// The task id is deliberately not "run", so this file's worker (TASK=run) leaves it parked.
test("script task — a large script leaves the instance and is listed, not carried", async () => {
  const name = `script_shared_${crypto.randomUUID()}`;
  const pad = Array.from({ length: 400 }, (_, i) => `const pad_${i} = "${"x".repeat(64)}";`).join("\n");
  const code = `${pad}\nexport default () => ({ ok: true });`;

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
    if ((await parkedInProcess(name, client)).length === 2) {
      // The externalized-value list rides on the claim, not on discovery.
      entries = await claimInProcess(name, "parked");
      if (entries.length === 2) break;
    }
    await new Promise((r) => setTimeout(r, 50));
  }
  expect(entries.length, "both instances parked").toBe(2);

  for (const e of entries) {
    expect(e.external_input?.code, "the bundle must not be carried in the task input").toBeUndefined();
    const listed = (e.objects ?? []).find(
      (o: any) => o.path.length === 2 && o.path[0] === "external_input" && o.path[1] === "code",
    );
    expect(listed, "the bundle is listed at external_input.code").toBeDefined();
    expect(listed.size).toBeGreaterThan(code.length - 100);
    // The per-instance half stays inline: externalizing the whole input would fold it in and
    // give every instance a different hash, which is the sharing this exists for, lost.
    expect(e.external_input?.input?.n).toBeGreaterThan(0);
  }

  const refs = new Set(entries.map((e) => e.objects[0].ref));
  expect(refs.size, "both instances name the SAME object — the bundle is stored once").toBe(1);

  // And it is fetchable by that hash, which is how a worker gets it.
  const { data: obj } = await client.GET("/objects/{ref}", {
    params: { path: { ref: [...refs][0] } },
  });
  expect(JSON.parse((obj as any).data)).toBe(code);
}, 40_000);

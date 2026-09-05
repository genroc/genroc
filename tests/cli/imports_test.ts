import { mkdirSync, mkdtempSync, readFileSync, writeFileSync, existsSync } from "fs";
import { spawn, type ChildProcess } from "child_process";
import { tmpdir } from "os";
import { join, relative } from "path";
import { afterAll, beforeAll, expect, test } from "vitest";
import { buildGenctlBinary, runCli } from "../helpers/cli.ts";
import { waitForInstance } from "../helpers/client.ts";
import { BASE_URL } from "../helpers/constants.ts";
import { startedID, uid } from "../helpers/genctl.ts";

// Source resolution: a `$<resolver>: <path>` leaf is replaced by a string a registered
// binary produces, before anything reaches the server. specs/source-resolution.md.
//
// The fake resolver below returns file contents verbatim — the mechanism is
// language-agnostic, and a plain text template exercises every part of it except tsc.
// The evaluator's importer gets its own tests at the bottom.

let bin: string;
beforeAll(() => {
  bin = buildGenctlBinary();
}, 60_000);

// The evaluator, for the two tests that RUN an imported script rather than only applying it.
// Started lazily: every other test here stops at the apply and needs no worker. It claims the
// script tasks off the shared test server's queue — nothing listens, so there is no port.
let runner: ChildProcess | undefined;
let runnerReady: Promise<void>;
async function startRunner(): Promise<void> {
  if (runner) return runnerReady;
  runner = spawn("node", [join(REPO, "eval-node/worker.ts")], {
    // TASK scopes this worker to the script tasks below; an unfiltered one would claim every
    // parked external task on the shared test server.
    env: { ...process.env, GENROC_SERVER: BASE_URL, POLL_MS: "50", TASK: "price", WORKER_ID: `imports-${process.pid}` },
    stdio: ["ignore", "pipe", "inherit"],
  });
  runnerReady = new Promise<void>((resolve, reject) => {
    const timer = setTimeout(() => reject(new Error("evaluator worker did not start within 10s")), 10_000);
    runner!.stdout!.on("data", (chunk: Buffer) => {
      if (chunk.toString().includes("polling")) {
        clearTimeout(timer);
        resolve();
      }
    });
    runner!.on("error", reject);
  });
  return runnerReady;
}
afterAll(() => runner?.kill());

const REPO = new URL("../../", import.meta.url).pathname;

// `schema` infers locally; nothing here needs the server.
const OFFLINE = { GENROC_SERVER: "http://127.0.0.1:1" };

/** A resolver that echoes each site's file and dumps the manifest for inspection. It JOINS the
 *  argument to its process's directory, because genctl passes the argument verbatim. */
const ECHO_RESOLVER = `
import { readFileSync, writeFileSync } from "node:fs";
import { resolve } from "node:path";
const chunks = [];
for await (const c of process.stdin) chunks.push(c);
const m = JSON.parse(Buffer.concat(chunks).toString("utf8"));
writeFileSync(new URL("./manifest.json", import.meta.url), JSON.stringify(m, null, 2));
if (m.mode === "types") process.exit(0);
const code = [];
for (const p of m.processes)
  for (const s of p.sites) code.push(readFileSync(resolve(p.dir, s.argument), "utf8"));
process.stdout.write(JSON.stringify({ code }));
`;

type Project = { dir: string; write: (name: string, body: string) => string; manifest: () => any };

function project(resolvers: string): Project {
  const dir = mkdtempSync(join(tmpdir(), "genroc_import_"));
  writeFileSync(join(dir, ".genroc"), resolvers, "utf8");
  writeFileSync(join(dir, "echo.mjs"), ECHO_RESOLVER, "utf8");
  return {
    dir,
    write: (name, body) => {
      const path = join(dir, name);
      mkdirSync(join(path, ".."), { recursive: true });
      writeFileSync(path, body, "utf8");
      return path;
    },
    manifest: () => JSON.parse(readFileSync(join(dir, "manifest.json"), "utf8")),
  };
}

function echoProject(): Project {
  // No `types`: this resolver splices text and wants nothing typed, which is most of them.
  return project(`resolvers:\n  import: { phase: code, command: [node, echo.mjs] }\n`);
}

// A resolver that DOES want types names them, by `genctl schema type` address relative to the
// task the directive sits in — `input` is the whole action input, `input.amount` one field of
// it, `result` what the task hands back.
function typedProject(): Project {
  return project(
    [
      "resolvers:",
      "  import:",
      "    phase: code",
      "    command: [node, echo.mjs]",
      "    types: { Action: task.action.body, Amount: task.action.body.amount, Output: task.action.result }",
      "",
    ].join("\n"),
  );
}

// ── the resolution pass ─────────────────────────────────────────────────────────

test("apply — an imported file becomes the slot's value, and $ survives it verbatim", async () => {
  const p = echoProject();
  const name = uid("import");
  // Every character the template layer has an opinion about. If genctl did not double the
  // `$` on splice, `${world}` would be read by genroc as an interpolation of an unknown
  // identifier and this apply would fail instead of storing the text.
  const snippet = 'hello ${world} — $$ — $: not an expression — plain $5.00\n';
  p.write("snippet.txt", snippet);
  const def = p.write(
    "proc.yaml",
    [
      `name: ${name}`,
      "tasks:",
      "  - id: t",
      "    output:",
      '      code: "$import: ./snippet.txt"',
      "    switch: [{ goto: end }]",
      'output: "$: outputs.t.code"',
      "",
    ].join("\n"),
  );

  expect(runCli(bin, ["apply", "-f", def]).stdout).toContain(`saved: ${name}@v1`);

  const id = startedID(runCli(bin, ["run", name]).stdout);
  expect(await waitForInstance(id)).toBe("completed");
  const instance = JSON.parse(runCli(bin, ["get", id, "--json"]).stdout);
  expect(instance.state.output).toBe(snippet);
});

test("apply — the manifest carries the inferred input type and the declared output type", () => {
  const p = typedProject();
  const name = uid("import");
  p.write("body.txt", "x");
  const def = p.write(
    "proc.yaml",
    [
      `name: ${name}`,
      "input_schema:",
      "  type: object",
      "  properties: { amount: { type: number } }",
      "  required: [amount]",
      "tasks:",
      "  - id: call",
      "    action:",
      "      type: fetch",
      "      url: http://localhost:9999/x",
      "      body:",
      '        code: "$import: ./body.txt"',
      '        amount: "$: input.amount"',
      "      responses:",
      "        200: { type: object, properties: { ok: { type: boolean } }, required: [ok] }",
      "    timeout: 5s",
      "    switch: [{ goto: end }]",
      "",
    ].join("\n"),
  );

  expect(runCli(bin, ["apply", "--check-only", "-f", def]).ok).toBe(true);

  const m = p.manifest();
  expect(m.mode).toBe("build");
  expect(m.root, "the cwd genctl runs a resolver in says it; the manifest need not").toBeUndefined();
  expect(m.processes).toHaveLength(1);
  expect(m.processes[0].name).toBe(name);
  expect(m.processes[0].dir).toBe(p.dir);
  expect(m.processes[0].file).toBe("proc.yaml");
  expect(m.processes[0].sites).toHaveLength(1);

  const site = m.processes[0].sites[0];
  expect(site.task).toBe("call");
  // Shaped like the definition: the task by ID, then the document's own keys. What KIND of
  // action it is rides beside the address as a field, not inside it.
  expect(site.pointer).toEqual(["tasks", "call", "action", "body", "code"]);
  expect(site.level, "which namespace it sits in").toBe("action");
  expect(site.action, "what the site IS, beside where it is").toBe("fetch");
  expect(site.argument, "verbatim: genctl does not read it as a path").toBe("./body.txt");

  // Each fragment is what the resolver ASKED for, resolved at this site. `Action` is inferred —
  // `amount` is typed from the process input schema, which only validation knows — and `Amount`
  // is one field of it, which genctl reaches because the request is an address rather than a
  // fixed pair. `Output` is DECLARED: responses.200 verbatim.
  const defs = m.processes[0].$defs;
  const action = site.types.Action.$ref
    ? defs[site.types.Action.$ref.replace("#/$defs/", "")]
    : site.types.Action;
  expect(action.properties.amount).toEqual({ type: "number" });
  expect(action.properties.code).toEqual({ type: "string" });
  expect(site.types.Amount, "an address reaches INTO the action input").toEqual({
    type: "number",
  });
  expect(site.types.Output).toEqual({
    type: "object",
    properties: { ok: { type: "boolean" } },
    required: ["ok"],
  });
});

test("types — writes declarations and applies nothing", () => {
  const p = echoProject();
  const name = uid("import");
  p.write("body.txt", "x");
  const def = p.write(
    "proc.yaml",
    [
      `name: ${name}`,
      "tasks:",
      "  - id: t",
      "    output:",
      '      code: "$import: ./body.txt"',
      "    switch: [{ goto: end }]",
      "",
    ].join("\n"),
  );

  expect(runCli(bin, ["types", "-f", def]).stdout).toContain("generated types for 1 import");
  expect(p.manifest().mode).toBe("types");
  // Nothing was stored: types is the editor's command, not a write.
  const rows = JSON.parse(runCli(bin, ["definitions", "--json"]).stdout) as { name: string }[];
  expect(rows.some((r) => r.name === name)).toBe(false);
});

// The reason the types are inferred in genctl rather than fetched: `genctl types` runs on
// every edit, and an editor loop that stops working when the server is down is a worse
// property than a genctl that links the inference. specs/source-resolution.md.
test("types — needs no server, and the types are still inferred", () => {
  const p = typedProject();
  p.write("body.txt", "x");
  const def = p.write(
    "proc.yaml",
    [
      `name: ${uid("import")}`,
      "input_schema:",
      "  type: object",
      "  properties: { amount: { type: number } }",
      "  required: [amount]",
      "tasks:",
      "  - id: t",
      "    action:",
      "      type: fetch",
      "      url: https://example.test/x",
      "      method: POST",
      "      body:",
      '        code: "$import: ./body.txt"',
      '        amount: "$: input.amount"',
      "      responses:",
      "        200: { type: object, properties: { ok: { type: boolean } }, required: [ok] }",
      "    switch: [{ goto: end }]",
      "",
    ].join("\n"),
  );

  // Nothing listens on port 1, so any roundtrip here fails the command.
  const r = runCli(bin, ["types", "-f", def], { GENROC_SERVER: "http://127.0.0.1:1" });
  expect(r.ok, `types must not need a server:\n${r.stdout}${r.stderr}`).toBe(true);

  // …and the manifest still carries INFERRED fragments, which is the half a roundtrip used to
  // buy: `amount` is typed from the process input schema, not from the source text.
  const m = p.manifest();
  const site = m.processes[0].sites[0];
  const defs = m.processes[0].$defs ?? {};
  const action = site.types.Action.$ref
    ? defs[site.types.Action.$ref.replace("#/$defs/", "")]
    : site.types.Action;
  expect(action.properties.amount).toEqual({ type: "number" });
  expect(site.types.Amount).toEqual({ type: "number" });
});

// genctl computes the types; the server decides validity. A definition carrying no directive
// is never inferred at all, so one broken file cannot stop the generation for every other
// script in a project-wide `genctl types` — and the apply still refuses it.
test("types — a definition with no directive is the server's to judge, not genctl's", () => {
  const p = echoProject();
  p.write("body.txt", "x");
  const good = p.write(
    "good.yaml",
    [
      `name: ${uid("import")}`,
      "tasks:",
      "  - id: t",
      "    output:",
      '      code: "$import: ./body.txt"',
      "    switch: [{ goto: end }]",
      "",
    ].join("\n"),
  );
  // Invalid — a task must route somewhere — and it imports nothing.
  const bad = p.write("bad.yaml", `name: ${uid("import")}\ntasks:\n  - id: t\n`);

  const types = runCli(bin, ["types", "-f", good, bad]);
  expect(types.ok, `a broken sibling must not stop type generation:\n${types.stdout}${types.stderr}`).toBe(true);
  expect(types.stdout).toContain("generated types for 1 import");

  // …and the judgement genctl skipped still happens, with the server's own message.
  const applied = runCli(bin, ["apply", "-f", good, bad]);
  expect(applied.ok).toBe(false);
  expect(applied.stderr).toContain("switch");
});

test("types — says so when a definition imports nothing", () => {
  const p = echoProject();
  const def = p.write(
    "proc.yaml",
    `name: ${uid("import")}\ntasks:\n  - id: t\n    switch: [{ goto: end }]\n`,
  );
  expect(runCli(bin, ["types", "-f", def]).stdout).toContain("no imports found");
});

test("apply — a relative -f path still leaves the resolver a base it can join", async () => {
  const p = echoProject();
  const name = uid("import");
  p.write("snippet.txt", "relative\n");
  const def = p.write(
    "proc.yaml",
    [
      `name: ${name}`,
      "tasks:",
      "  - id: t",
      "    output:",
      '      code: "$import: ./snippet.txt"',
      "    switch: [{ goto: end }]",
      'output: "$: outputs.t.code"',
      "",
    ].join("\n"),
  );

  // The argument travels verbatim and the DIRECTORY it is relative to travels with the
  // definition — absolute, because the resolver's cwd is the project root rather than the
  // directory -f was relative to, and joining against the wrong base finds nothing.
  expect(runCli(bin, ["apply", "-f", relative(process.cwd(), def)]).ok).toBe(true);
  const proc = p.manifest().processes[0];
  expect(proc.sites[0].argument).toBe("./snippet.txt");
  expect(proc.dir).toBe(p.dir);
  expect(proc.file).toBe("proc.yaml");

  const id = startedID(runCli(bin, ["run", name]).stdout);
  expect(await waitForInstance(id)).toBe("completed");
  const instance = JSON.parse(runCli(bin, ["get", id, "--json"]).stdout);
  expect(instance.state.output).toBe("relative\n");
});

test("compat — the document is resolved before it is compared", () => {
  const p = echoProject();
  const name = uid("import");
  p.write("snippet.txt", "the body\n");
  const def = p.write(
    "proc.yaml",
    [
      `name: ${name}`,
      "tasks:",
      "  - id: t",
      "    output:",
      '      code: "$import: ./snippet.txt"',
      "    switch: [{ goto: end }]",
      'output: "$: outputs.t.code"',
      "",
    ].join("\n"),
  );
  expect(runCli(bin, ["apply", "-f", def]).stdout).toContain(`saved: ${name}@v1`);

  // Unresolved, the slot holds the literal `$import: ./snippet.txt` beside the text v1
  // stores, so every imported site compares changed and no document can ever read unchanged
  // — the same file apply just deduped.
  const same = runCli(bin, ["compat", "-f", def, "--from", `${name}@1`]);
  expect(same.ok, same.stderr).toBe(true);
  expect(same.stdout).toMatch(new RegExp(`${name}\\s+v1\\s+unchanged`));

  // The yaml is untouched here: only the IMPORTED file changed, so a report that notices is
  // one that ran the resolver.
  p.write("snippet.txt", "another body\n");
  const edited = runCli(bin, ["compat", "-f", def, "--from", `${name}@1`]);
  expect(edited.stdout).toMatch(new RegExp(`${name}\\s+v1 → \\(new\\)`));
});

// ── refusals ────────────────────────────────────────────────────────────────────

test("apply — an unregistered resolver names itself and stores nothing", () => {
  const p = echoProject();
  const name = uid("import");
  const def = p.write(
    "proc.yaml",
    [
      `name: ${name}`,
      "tasks:",
      "  - id: t",
      "    output:",
      '      code: "$nosuch: ./body.txt"',
      "    switch: [{ goto: end }]",
      "",
    ].join("\n"),
  );

  const r = runCli(bin, ["apply", "-f", def]);
  expect(r.ok).toBe(false);
  expect(r.stderr).toContain('no resolver named "nosuch"');
  expect(r.stderr).toContain(".genroc");
});

// genctl does not know an argument names a file, so it cannot refuse one that is missing — the
// resolver that reads it is the one that can, and it still fails the apply before anything is
// stored. What genctl checks is the ASSERTION the config made: `ext`.
test("apply — a missing file is the resolver's to refuse", () => {
  const p = echoProject();
  const name = uid("import");
  const def = p.write(
    "proc.yaml",
    [
      `name: ${name}`,
      "tasks:",
      "  - id: t",
      "    output:",
      '      code: "$import: ./gone.txt"',
      "    switch: [{ goto: end }]",
      "",
    ].join("\n"),
  );
  const r = runCli(bin, ["apply", "-f", def]);
  expect(r.ok).toBe(false);
  expect(r.stderr).toContain("gone.txt");
  // The manifest WAS written: the resolver ran, and refused. Nothing reached the server.
  expect(existsSync(join(p.dir, "manifest.json"))).toBe(true);
  const rows = JSON.parse(runCli(bin, ["definitions", "--json"]).stdout) as { name: string }[];
  expect(rows.some((r) => r.name === name)).toBe(false);
});

test("apply — a resolver's exit code aborts the apply with its stderr", () => {
  const p = project(`resolvers:\n  import: { phase: code, command: [node, fail.mjs] }\n`);
  p.write("fail.mjs", 'console.error("summarize.ts(3,7): error TS2322: nope");\nprocess.exit(1);\n');
  p.write("body.txt", "x");
  const name = uid("import");
  const def = p.write(
    "proc.yaml",
    [
      `name: ${name}`,
      "tasks:",
      "  - id: t",
      "    output:",
      '      code: "$import: ./body.txt"',
      "    switch: [{ goto: end }]",
      "",
    ].join("\n"),
  );

  const r = runCli(bin, ["apply", "-f", def]);
  expect(r.ok).toBe(false);
  expect(r.stderr).toContain("error TS2322");
  // The ordering property: a failed resolution never produces the string, so a stored
  // definition cannot contain code that failed to typecheck.
  const rows = JSON.parse(runCli(bin, ["definitions", "--json"]).stdout) as { name: string }[];
  expect(rows.some((r2) => r2.name === name)).toBe(false);
});

test("apply — an ext mismatch is refused by name rather than inside the toolchain", () => {
  const p = project(
    `resolvers:\n  import: { phase: code, ext: .ts, command: [node, echo.mjs] }\n`,
  );
  writeFileSync(join(p.dir, "echo.mjs"), ECHO_RESOLVER, "utf8");
  p.write("body.txt", "x");
  const def = p.write(
    "proc.yaml",
    [
      `name: ${uid("import")}`,
      "tasks:",
      "  - id: t",
      "    output:",
      '      code: "$import: ./body.txt"',
      "    switch: [{ goto: end }]",
      "",
    ].join("\n"),
  );
  const r = runCli(bin, ["apply", "-f", def]);
  expect(r.ok).toBe(false);
  expect(r.stderr).toContain("accepts .ts files");
});

test("apply — $$ escapes the directive, leaving a literal string", async () => {
  const p = echoProject();
  const name = uid("import");
  const def = p.write(
    "proc.yaml",
    [
      `name: ${name}`,
      "tasks:",
      "  - id: t",
      "    output:",
      '      code: "$$import: ./body.txt"',
      "    switch: [{ goto: end }]",
      'output: "$: outputs.t.code"',
      "",
    ].join("\n"),
  );

  expect(runCli(bin, ["apply", "-f", def]).ok).toBe(true);
  // No resolver ran: the leaf never was a directive.
  expect(existsSync(join(p.dir, "manifest.json"))).toBe(false);

  const id = startedID(runCli(bin, ["run", name]).stdout);
  expect(await waitForInstance(id)).toBe("completed");
  const instance = JSON.parse(runCli(bin, ["get", id, "--json"]).stdout);
  expect(instance.state.output).toBe("$import: ./body.txt");
});

test("apply — a definition with no directives spends no resolver and no extra roundtrip", () => {
  // The resolver command does not exist, so running it at all would fail the apply.
  const p = project(`resolvers:\n  import: { phase: code, command: [/nonexistent/binary] }\n`);
  const name = uid("import");
  const def = p.write(
    "proc.yaml",
    `name: ${name}\ntasks:\n  - id: t\n    switch: [{ goto: end }]\n`,
  );
  expect(runCli(bin, ["apply", "-f", def]).stdout).toContain(`saved: ${name}@v1`);
});

// What a site can answer varies: a resolver asks for the same addresses everywhere, and a task
// that types no result simply has none. The key is NULL rather than missing — it was asked for,
// and nothing is there, which is a different fact from not being asked for — and the apply is
// not refused, because a script that takes no argument is a legal definition.
test("a requested type that is not at this site comes back null", () => {
  const p = typedProject();
  const name = uid("import");
  p.write("body.txt", "x");
  const def = p.write(
    "proc.yaml",
    [
      `name: ${name}`,
      "input_schema:",
      "  type: object",
      "  properties: { amount: { type: number } }",
      "  required: [amount]",
      "tasks:",
      "  - id: t",
      "    action:",
      "      type: fetch",
      "      url: https://example.test/x",
      "      method: POST",
      '      body: { code: "$import: ./body.txt", amount: "$: input.amount" }',
      "    switch: [{ goto: end }]",
      "",
    ].join("\n"),
  );

  // No `responses`, so the task types no result — but the input fragments are there.
  const r = runCli(bin, ["types", "-f", def]);
  expect(r.ok, `${r.stdout}${r.stderr}`).toBe(true);

  const site = p.manifest().processes[0].sites[0];
  expect(Object.keys(site.types).sort(), "every name the resolver asked for is answered").toEqual([
    "Action",
    "Amount",
    "Output",
  ]);
  expect(site.types.Output, "asked for, and nothing is there").toBeNull();
  expect(site.types.Amount).toEqual({ type: "number" });
});

// The same collapse `schema type` prints, on the path that ships to a resolver: a def that is
// only a `$ref` becomes a generated type alias per task, naming nothing.
test("a definition that only names another never reaches the manifest", () => {
  const p = project(
    [
      "resolvers:",
      "  import:",
      "    phase: code",
      "    command: [node, echo.mjs]",
      "    types: { Output: task.output }",
      "",
    ].join("\n"),
  );
  const name = uid("import");
  p.write("body.txt", "x");
  const def = p.write(
    "proc.yaml",
    [
      `name: ${name}`,
      "tasks:",
      "  - id: t",
      "    action:",
      "      type: fetch",
      "      url: https://example.test/x",
      "      method: POST",
      '      body: { code: "$import: ./body.txt" }',
      '      responses: { 200: { $ref: "#/$defs/quote" } }',
      "    output: \"$: self.result\"",
      "    switch: [{ goto: end }]",
      "$defs:",
      "  quote: { type: object, properties: { fee: { type: number } }, required: [fee] }",
      "",
    ].join("\n"),
  );

  const r = runCli(bin, ["types", "-f", def]);
  expect(r.ok, `${r.stdout}${r.stderr}`).toBe(true);

  const proc = p.manifest().processes[0];
  expect(proc.sites[0].types.Output, "the definition itself, not a name for it").toEqual({
    $ref: "#/$defs/quote",
  });
  expect(Object.keys(proc.$defs), "and no def that only forwards travels beside it").toEqual([
    "quote",
  ]);
});

// An address names the frame it is relative to, because a directive can sit inside a task or
// outside one and both frames have an `input`. Without the frame, a `task`-relative request
// silently answered with the PROCESS input at a site in the output map — a plausible-looking
// type from an unrelated schema, which is worse than no answer.
test("a task-relative type is null outside a task, and process-relative still answers", () => {
  const p = project(
    [
      "resolvers:",
      "  import:",
      "    phase: code",
      "    command: [node, echo.mjs]",
      "    types: { Input: task.action.input.input, Output: task.action.result, Whole: process.input }",
      "",
    ].join("\n"),
  );
  const name = uid("import");
  p.write("body.txt", "x");
  const def = p.write(
    "proc.yaml",
    [
      `name: ${name}`,
      "input_schema:",
      "  type: object",
      "  properties: { amount: { type: number } }",
      "  required: [amount]",
      "tasks:",
      "  - id: t",
      "    action: { type: external, result_schema: { type: object } }",
      "    switch: [{ goto: end }]",
      "output:",
      '  note: "$import: ./body.txt"',
      "",
    ].join("\n"),
  );

  expect(runCli(bin, ["types", "-f", def]).ok).toBe(true);
  const site = p.manifest().processes[0].sites[0];
  expect(site.pointer, "the directive is in the process output, not in a task").toEqual([
    "output",
    "note",
  ]);
  expect(site.task).toBeUndefined();
  expect(site.types.Input, "no task here, so a task-relative address answers null").toBeNull();
  expect(site.types.Output).toBeNull();
  expect(site.types.Whole, "the process frame is there at every site").toEqual({
    $ref: "#/$defs/input",
  });
});

// A frame that is not one is a typo, and it is refused where it is written rather than resolving
// somewhere unintended.
test("an address with no frame is refused at the config", () => {
  const p = project(
    `resolvers:\n  import: { phase: code, command: [node, echo.mjs], types: { X: input.input } }\n`,
  );
  p.write("body.txt", "x");
  const def = p.write(
    "proc.yaml",
    [`name: ${uid("import")}`, "tasks:", "  - id: t", "    output:", '      code: "$import: ./body.txt"', "    switch: [{ goto: end }]", ""].join("\n"),
  );
  const r = runCli(bin, ["types", "-f", def]);
  expect(r.ok).toBe(false);
  expect(r.stderr).toContain('"input.input" names no frame');
});

// ── pointers are type addresses ─────────────────────────────────────────────────

/** A pointer as the address it is — the rendering `schema` reads back, brackets and all. */
function address(pointer: (string | number)[]): string {
  return pointer
    .map((seg) =>
      typeof seg === "string" && /^[A-Za-z_][A-Za-z0-9_]*$/.test(seg)
        ? `.${seg}`
        : `[${JSON.stringify(seg)}]`,
    )
    .join("")
    .replace(/^\./, "");
}

// Both spaces name a slot the way the DEFINITION does, so what the manifest says about where a
// directive is can be handed to `schema type` unchanged. This feeds the pointers straight back
// rather than re-deriving them: a rename on either side breaks it here.
// A site's level is which namespace it is in, and the action's type is reported only where the
// directive is actually IN the action: a switch case is the task's, and calling it a delay site
// would describe the wrong thing.
test("a site says which level it is at, and only an action site names the action", () => {
  const p = echoProject();
  const name = uid("import");
  p.write("f.txt", "x");
  const def = p.write(
    "proc.yaml",
    [
      `name: ${name}`,
      "tasks:",
      "  - id: wait",
      "    action: { type: delay, for: 5s }",
      '    switch: [{ case: "$import: ./f.txt", goto: end }]',
      'output: { done: "$import: ./f.txt" }',
      "",
    ].join("\n"),
  );

  expect(runCli(bin, ["types", "-f", def]).ok).toBe(true);
  const sites = p.manifest().processes[0].sites as Record<string, unknown>[];
  const at = (level: string) => sites.find((s) => s.level === level)!;

  expect(at("task").task).toBe("wait");
  expect(at("task").action, "a switch case is not in the action").toBeUndefined();
  expect(at("process").task, "and the process output is in no task at all").toBeUndefined();
});

test("a manifest pointer is an address `schema type` answers", () => {
  const p = echoProject();
  const name = uid("import");
  p.write("body.txt", "x");
  const def = p.write(
    "proc.yaml",
    [
      `name: ${name}`,
      "input_schema: { type: object, properties: { n: { type: integer } }, required: [n] }",
      "tasks:",
      // A space in the id, so the rendering has to quote it and the address still resolves.
      '  - id: "my call"',
      "    action:",
      "      type: fetch",
      '      url: "$import: ./body.txt"',
      "      method: POST",
      '      body: { code: "$import: ./body.txt" }',
      "      responses:",
      "        200: { type: object, properties: { ok: { type: boolean } }, required: [ok] }",
      '    output: { note: "$import: ./body.txt" }',
      "    switch: [{ goto: end }]",
      'output: { done: "$import: ./body.txt" }',
      "",
    ].join("\n"),
  );

  expect(runCli(bin, ["types", "-f", def]).ok).toBe(true);
  const pointers = p.manifest().processes[0].sites.map((s: { pointer: (string | number)[] }) =>
    address(s.pointer),
  );
  expect(pointers.sort()).toEqual([
    'tasks["my call"].action.body.code',
    'tasks["my call"].action.url',
    'tasks["my call"].output.note',
    "output.done",
  ].sort());

  // A directive is a string leaf, so where the slot has a type the answer is what it splices to.
  const TYPED = new Set([
    'tasks["my call"].action.body.code',
    'tasks["my call"].output.note',
    "output.done",
  ]);
  for (const at of pointers) {
    const r = runCli(bin, ["schema", "type", name, at, "--json", "-f", def], OFFLINE);
    if (TYPED.has(at)) {
      expect(r.ok, `${at}: ${r.stderr}`).toBe(true);
      expect(JSON.parse(r.stdout), at).toEqual({ type: "string" });
      continue;
    }
    // `url` holds a template, not a contract boundary: a pointer promises a location, and only
    // a location. The refusal names what the action does have rather than inventing a type.
    expect(r.ok, `${at} names no type`).toBe(false);
    expect(r.stderr).toContain("which holds: body, result");
  }
});

// ── the evaluator's importer ────────────────────────────────────────────────────

function tsProject(): Project {
  const p = project(
    [
      "resolvers:",
      "  import:",
      "    phase: code",
      "    ext: .ts",
      `    command: [node, ${join(REPO, "eval-node/import.ts")}]`,
      "    types: { Input: task.action.input.input, Output: task.action.result }",
      "",
    ].join("\n"),
  );
  return p;
}

/** A definition whose script task carries the import, shaped like the playground's. */
function scriptDef(name: string, script: string): string {
  return [
    `name: ${name}`,
    "input_schema:",
    "  type: object",
    "  properties: { amount: { type: number } }",
    "  required: [amount]",
    "tasks:",
    "  - id: price",
    "    action:",
    "      type: external",
    "      input:",
    `        code: "$import: ${script}"`,
    '        input: "$: input"',
    "      result_schema: { type: object, properties: { fee: { type: number } }, required: [fee] }",
    "    timeout: 5s",
    "    switch: [{ goto: end }]",
    "",
  ].join("\n");
}

test("evaluator importer — generates declarations keyed by the script's path", () => {
  const p = tsProject();
  p.write(
    "fee.ts",
    [
      'import type { Input, Output } from "./fee.genroc";',
      "",
      "export default async function (input: Input): Promise<Output> {",
      "  return { fee: input.amount * 0.1 };",
      "}",
      "",
    ].join("\n"),
  );
  const def = p.write("proc.yaml", scriptDef(uid("script"), "./fee.ts"));

  expect(runCli(bin, ["types", "-f", def]).ok).toBe(true);

  // Named for the script, not the task: renaming `price` must not break the import line.
  const decls = readFileSync(join(p.dir, "fee.genroc.d.ts"), "utf8");
  expect(decls).toContain("export type Input =");
  expect(decls).toContain("amount");
  expect(decls).toContain("export type Output =");
  expect(decls).toContain("fee: number");
}, 60_000);

// genctl is agnostic about what a script is for; that an evaluation request carries its module
// in `code` is the EVALUATOR's contract, so the evaluator is what enforces it. Its two shapes are
// a child call to a process that forwards to it (what the scaffold generates) and an external
// task making the same request directly.
test("evaluator importer — a directive outside an evaluation request is refused", () => {
  const p = tsProject();
  p.write("fee.ts", "export default () => 1;\n");
  const def = p.write(
    "proc.yaml",
    [
      `name: ${uid("import")}`,
      "tasks:",
      "  - id: t",
      "    action:",
      "      type: fetch",
      "      url: http://x",
      "      method: POST",
      '      body: { code: "$import: ./fee.ts" }',
      "      responses: { 200: { type: object } }",
      "    switch: [{ goto: end }]",
      "",
    ].join("\n"),
  );

  const r = runCli(bin, ["types", "-f", def]);
  expect(r.ok).toBe(false);
  expect(r.stderr).toContain("belongs in the `code` field of a child or external task's input");
  expect(r.stderr, "and it says which slot it landed in").toContain("tasks.t.action.body.code");
});

test("evaluator importer — a type error is a failed import, so nothing is stored", () => {
  const p = tsProject();
  p.write(
    "bad.ts",
    [
      'import type { Input, Output } from "./bad.genroc";',
      "",
      "export default async function (input: Input): Promise<Output> {",
      // `fee` is a number in the declared output; a string cannot be assigned to it.
      '  return { fee: "not a number" };',
      "}",
      "",
    ].join("\n"),
  );
  const name = uid("script");
  const def = p.write("proc.yaml", scriptDef(name, "./bad.ts"));

  const r = runCli(bin, ["apply", "-f", def]);
  expect(r.ok).toBe(false);
  expect(r.stderr).toContain("bad.ts");
  expect(r.stderr).toMatch(/error TS\d+/);

  const rows = JSON.parse(runCli(bin, ["definitions", "--json"]).stdout) as { name: string }[];
  expect(rows.some((row) => row.name === name)).toBe(false);
}, 60_000);

test("evaluator importer — a checked script applies as a self-contained module", () => {
  const p = tsProject();
  p.write("rate.ts", "export const RATE = 0.1;\n");
  p.write(
    "fee.ts",
    [
      'import type { Input, Output } from "./fee.genroc";',
      'import { RATE } from "./rate";',
      "",
      "export default async function (input: Input): Promise<Output> {",
      // A template literal: `${` is genroc's interpolation marker, and this reaching the
      // server unescaped is the failure the import directive exists to remove.
      "  const label = `fee for ${input.amount}`;",
      "  return { fee: input.amount * RATE, label };",
      "}",
      "",
    ].join("\n"),
  );
  const name = uid("script");
  const def = p.write(
    "proc.yaml",
    scriptDef(name, "./fee.ts").replace(
      "properties: { fee: { type: number } }, required: [fee] }",
      "properties: { fee: { type: number }, label: { type: string } }, required: [fee, label] }",
    ),
  );

  expect(runCli(bin, ["apply", "-f", def]).stdout).toContain(`saved: ${name}@v1`);
}, 60_000);

// The one thing the typecheck cannot see: `Input`/`Output` say nothing about HOW the module
// exports, and the evaluator has only the file's own default export to call. Refused here, where
// the path names a file on this machine, rather than against a running instance.
test("evaluator importer — a script with no default export is a failed apply", () => {
  const p = tsProject();
  p.write(
    "fee.ts",
    [
      'import type { Input, Output } from "./fee.genroc";',
      "",
      "export async function fee(input: Input): Promise<Output> {",
      "  return { fee: input.amount * 0.1 };",
      "}",
      "",
    ].join("\n"),
  );
  const name = uid("script");
  const def = p.write("proc.yaml", scriptDef(name, "./fee.ts"));

  const r = runCli(bin, ["apply", "-f", def]);
  expect(r.ok, `a module with nothing to call must not be stored:\n${r.stdout}${r.stderr}`).toBe(false);
  expect(r.stderr).toContain("fee.ts");
  expect(r.stderr).toContain("export default");

  const rows = JSON.parse(runCli(bin, ["definitions", "--json"]).stdout) as { name: string }[];
  expect(rows.some((row) => row.name === name)).toBe(false);
}, 60_000);

test("evaluator importer — the sandbox is a worker realm, not the host one", () => {
  const p = tsProject();
  p.write(
    "fee.ts",
    [
      'import type { Input, Output } from "./fee.genroc";',
      "",
      "export default async function (input: Input): Promise<Output> {",
      "  const res = await fetch(new URL(String(input.amount), 'http://localhost:9999/'));",
      "  console.log(res.status);",
      "  return { fee: input.amount * 0.1 };",
      "}",
      "",
    ].join("\n"),
  );
  const name = uid("script");
  const def = p.write("proc.yaml", scriptDef(name, "./fee.ts"));

  // A script that reads an HTTP source is the ordinary case; under a host-realm fence
  // (`lib: [esnext]` alone) `fetch` and `console` do not resolve and this apply fails.
  expect(runCli(bin, ["apply", "-f", def]).stdout).toContain(`saved: ${name}@v1`);

  p.write(
    "host.ts",
    [
      'import type { Input, Output } from "./host.genroc";',
      "",
      "export default async function (input: Input): Promise<Output> {",
      "  return { fee: input.amount * Number(process.env.RATE) };",
      "}",
      "",
    ].join("\n"),
  );
  const hostDef = p.write("host.yaml", scriptDef(uid("script"), "./host.ts"));

  // …and the fence that stays: the host's globals are not the worker's, so reaching for
  // one is a failed apply even though the evaluator's realm would have answered.
  const r = runCli(bin, ["apply", "-f", hostDef]);
  expect(r.ok, `process.env must not typecheck:\n${r.stdout}${r.stderr}`).toBe(false);
  expect(r.stderr).toContain("Cannot find name 'process'");
}, 60_000);

test("evaluator importer — a script is checked against the nearest tsconfig above it", () => {
  const p = tsProject();
  // An alias declared at the project root says nothing about a script that lives in `sub/`:
  // the author's editor reads sub/tsconfig.json, and so must the apply.
  p.write("tsconfig.json", JSON.stringify({ compilerOptions: {} }));
  p.write(
    "sub/tsconfig.json",
    JSON.stringify({ compilerOptions: { paths: { "#lib/*": ["./lib/*"] } } }),
  );
  p.write("sub/lib/rate.ts", "export const RATE = 0.1;\n");
  const script = [
    'import type { Input, Output } from "./fee.genroc";',
    'import { RATE } from "#lib/rate";',
    "",
    "export default async function (input: Input): Promise<Output> {",
    "  return { fee: input.amount * RATE };",
    "}",
    "",
  ].join("\n");
  p.write("sub/fee.ts", script);
  const name = uid("script");
  const def = p.write("proc.yaml", scriptDef(name, "./sub/fee.ts"));

  expect(runCli(bin, ["apply", "-f", def]).stdout).toContain(`saved: ${name}@v1`);

  // The same script one directory up, where only the root config applies and the alias is
  // undeclared. Its failure is what makes the apply above evidence of the walk.
  p.write("fee.ts", script);
  p.write("lib/rate.ts", "export const RATE = 0.1;\n");
  const bare = p.write("bare.yaml", scriptDef(uid("script"), "./fee.ts"));
  const r = runCli(bin, ["apply", "-f", bare]);
  expect(r.ok, `the root tsconfig declares no alias:\n${r.stdout}${r.stderr}`).toBe(false);
  expect(r.stderr).toContain("#lib/rate");
}, 60_000);

test("evaluator importer — the author's tsconfig cannot widen the sandbox or the program", () => {
  const p = tsProject();
  // Both halves of what `extends` must not let through: a `lib` that reopens the realm, and
  // an `include` that would drag the author's own tree in to be checked as a script.
  p.write(
    "tsconfig.json",
    JSON.stringify({ compilerOptions: { lib: ["esnext", "dom"] }, include: ["**/*.ts"] }),
  );
  p.write("stray.ts", "const oops: number = 'not a number';\n");
  p.write(
    "fee.ts",
    [
      'import type { Input, Output } from "./fee.genroc";',
      "",
      "export default async function (input: Input): Promise<Output> {",
      "  document.title = String(input.amount);",
      "  return { fee: input.amount * 0.1 };",
      "}",
      "",
    ].join("\n"),
  );
  const def = p.write("proc.yaml", scriptDef(uid("script"), "./fee.ts"));

  const r = runCli(bin, ["apply", "-f", def]);
  expect(r.ok).toBe(false);
  expect(r.stderr).toContain("Cannot find name 'document'");
  expect(r.stderr, "stray.ts is the author's file, not this program's to check").not.toContain(
    "stray.ts",
  );
}, 60_000);

test("evaluator importer — a data file imported as JSON is inlined and reaches the realm", async () => {
  await startRunner();
  const p = tsProject();
  // A `.json` import is a build-time data file, not JavaScript: whichever bundler is
  // underneath has to be told to parse it, and one that is not hands JSON to the JS parser.
  // Running it is what proves the VALUE was inlined — the realm has no file to read.
  p.write("rates.json", JSON.stringify({ rate: 0.25 }));
  p.write(
    "fee.ts",
    [
      'import type { Input, Output } from "./fee.genroc";',
      'import rates from "./rates.json";',
      "",
      "export default async function (input: Input): Promise<Output> {",
      "  return { fee: input.amount * rates.rate };",
      "}",
      "",
    ].join("\n"),
  );
  const name = uid("script");
  const def = p.write(
    "proc.yaml",
    [
      `name: ${name}`,
      "input_schema:",
      "  type: object",
      "  properties: { amount: { type: number } }",
      "  required: [amount]",
      "tasks:",
      "  - id: price",
      "    action:",
      "      type: external",
      "      input:",
      '        code: "$import: ./fee.ts"',
      '        input: "$: input"',
      "      result_schema: { type: object, properties: { fee: { type: number } }, required: [fee] }",
      "    timeout: 10s",
      '    output: "$: self.result"',
      "    switch: [{ goto: end }]",
      'output: "$: outputs.price"',
      "",
    ].join("\n"),
  );

  const applied = runCli(bin, ["apply", "-f", def]);
  expect(applied.stdout, `a json import must bundle:\n${applied.stdout}${applied.stderr}`).toContain(
    `saved: ${name}@v1`,
  );

  const started = runCli(bin, ["run", name, "--input", JSON.stringify({ amount: 100 })]);
  const id = startedID(`${started.stdout}${started.stderr}`);
  expect(await waitForInstance(id)).toBe("completed");
  const instance = JSON.parse(runCli(bin, ["get", id, "--json"]).stdout);
  expect(instance.state.output.fee, "0.25 must have been baked into the bundle").toBe(25);
}, 60_000);

test("evaluator importer — an import that resolves to nothing is a failed apply", () => {
  const p = tsProject();
  // The bundler's default for an unresolved import is to leave it as a require of a module
  // that will not be there: it bundles clean and fails inside the realm, where the author
  // cannot see it. Typecheck catches this one first; the refusal must hold either way.
  p.write(
    "fee.ts",
    [
      'import type { Input, Output } from "./fee.genroc";',
      '// @ts-expect-error — the point is a module that is not installed',
      'import { RATE } from "not-installed-anywhere";',
      "",
      "export default async function (input: Input): Promise<Output> {",
      "  return { fee: input.amount * RATE };",
      "}",
      "",
    ].join("\n"),
  );
  const def = p.write("proc.yaml", scriptDef(uid("script"), "./fee.ts"));

  const r = runCli(bin, ["apply", "-f", def]);
  expect(r.ok, `an unresolvable import must not produce a stored definition:\n${r.stdout}${r.stderr}`).toBe(false);
  expect(r.stderr).toContain("not-installed-anywhere");
}, 60_000);

test("evaluator importer — a package dependency resolves and is bundled in", () => {
  const p = tsProject();
  // A package, not a relative file. The worker realm fences GLOBALS, not module resolution:
  // an author who cannot reach their dependencies has no use for the toolchain.
  p.write(
    "node_modules/rate-lib/package.json",
    JSON.stringify({ name: "rate-lib", version: "1.0.0", main: "index.js", types: "index.d.ts" }),
  );
  p.write("node_modules/rate-lib/index.js", "exports.RATE = 0.1;\n");
  p.write("node_modules/rate-lib/index.d.ts", "export declare const RATE: number;\n");
  p.write(
    "fee.ts",
    [
      'import type { Input, Output } from "./fee.genroc";',
      'import { RATE } from "rate-lib";',
      "",
      "export default async function (input: Input): Promise<Output> {",
      "  return { fee: input.amount * RATE };",
      "}",
      "",
    ].join("\n"),
  );
  const name = uid("script");
  const def = p.write("proc.yaml", scriptDef(name, "./fee.ts"));

  const r = runCli(bin, ["apply", "-f", def]);
  expect(r.stdout, `a dependency must typecheck and bundle:\n${r.stdout}${r.stderr}`).toContain(
    `saved: ${name}@v1`,
  );
}, 60_000);

test("evaluator importer — a node builtin survives the bundle and runs in the realm", async () => {
  await startRunner();
  const p = tsProject();
  // The author declares which globals their scripts get; the generated config no longer
  // dictates `types`, so this is the opt-in. A stub package keeps the test off the repo's
  // own @types/node.
  p.write("tsconfig.json", JSON.stringify({ compilerOptions: { types: ["node"] } }));
  p.write(
    "node_modules/@types/node/package.json",
    JSON.stringify({ name: "@types/node", version: "1.0.0", types: "index.d.ts" }),
  );
  p.write(
    "node_modules/@types/node/index.d.ts",
    'declare module "node:fs" { export function readFileSync(p: string, enc: string): string; }\n',
  );
  p.write(
    "host.ts",
    [
      'import type { Input, Output } from "./host.genroc";',
      'import { readFileSync } from "node:fs";',
      "",
      "export default async function (input: Input): Promise<Output> {",
      "  return { fee: (input.amount ?? 250) * 0.1, host: readFileSync(input.path ?? '', 'utf8').trim() };",
      "}",
      "",
    ].join("\n"),
  );
  const secret = p.write("secret.txt", "the realm reaches the host\n");
  const name = uid("script");
  const def = p.write(
    "proc.yaml",
    [
      `name: ${name}`,
      "input_schema:",
      "  type: object",
      "  properties: { amount: { type: number, default: 250 }, path: { type: string } }",
      "tasks:",
      "  - id: price",
      "    action:",
      "      type: external",
      "      input:",
      '        code: "$import: ./host.ts"',
      '        input: "$: input"',
      "      result_schema:",
      "        type: object",
      "        properties: { fee: { type: number }, host: { type: string } }",
      "        required: [fee, host]",
      "    timeout: 10s",
      '    output: "$: self.result"',
      "    switch: [{ goto: end }]",
      'output: "$: outputs.price"',
      "",
    ].join("\n"),
  );

  const applied = runCli(bin, ["apply", "-f", def]);
  expect(applied.stdout, `${applied.stdout}${applied.stderr}`).toContain(`saved: ${name}@v1`);

  const started = runCli(bin, ["run", name, "--input", JSON.stringify({ amount: 250, path: secret })]);
  const id = startedID(`${started.stdout}${started.stderr}`);
  expect(await waitForInstance(id)).toBe("completed");
  const instance = JSON.parse(runCli(bin, ["get", id, "--json"]).stdout);

  // Only a real builtin can answer this: under the browser target `node:fs` bundles to `{}`
  // and `readFileSync` is undefined, so the script reaches the realm and throws.
  expect(instance.state.output.fee).toBe(25);
  expect(instance.state.output.host, "the script must have read the real filesystem").toBe(
    "the realm reaches the host",
  );
}, 60_000);

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

/** A resolver that echoes each site's file and dumps the manifest for inspection. */
const ECHO_RESOLVER = `
import { readFileSync, writeFileSync } from "node:fs";
const chunks = [];
for await (const c of process.stdin) chunks.push(c);
const m = JSON.parse(Buffer.concat(chunks).toString("utf8"));
writeFileSync(new URL("./manifest.json", import.meta.url), JSON.stringify(m, null, 2));
if (m.mode === "types") process.exit(0);
const code = [];
for (const s of m.sites) code.push(readFileSync(s.path, "utf8"));
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
  return project(`resolvers:\n  import: { phase: code, command: [node, echo.mjs] }\n`);
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
  const p = echoProject();
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

  expect(runCli(bin, ["validate", "-f", def]).ok).toBe(true);

  const m = p.manifest();
  expect(m.mode).toBe("build");
  expect(m.root).toBe(p.dir);
  expect(m.sites).toHaveLength(1);

  const site = m.sites[0];
  expect(site.resolver).toBe("import");
  expect(site.process).toBe(name);
  expect(site.task).toBe("call");
  expect(site.pointer).toBe("/tasks/0/action/body/code");
  expect(site.path).toBe(join(p.dir, "body.txt"));

  // The input type is INFERRED — `amount` is typed from the process input schema, which
  // only validation knows. The output type is DECLARED: responses.200 verbatim.
  const defs = m.schemas[name].$defs;
  const input = site.input.$ref ? defs[site.input.$ref.replace("#/$defs/", "")] : site.input;
  expect(input.properties.amount).toEqual({ type: "number" });
  expect(input.properties.code).toEqual({ type: "string" });
  expect(site.output).toEqual({
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

test("types — says so when a definition imports nothing", () => {
  const p = echoProject();
  const def = p.write(
    "proc.yaml",
    `name: ${uid("import")}\ntasks:\n  - id: t\n    switch: [{ goto: end }]\n`,
  );
  expect(runCli(bin, ["types", "-f", def]).stdout).toContain("no imports found");
});

test("apply — a relative -f path still resolves the site absolutely", async () => {
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

  // The resolver's cwd is the project root, not the directory -f was relative to. A site
  // path left relative resolved against the wrong place and the resolver wrote its output
  // into a mirrored subtree instead of beside the script.
  expect(runCli(bin, ["apply", "-f", relative(process.cwd(), def)]).ok).toBe(true);
  expect(p.manifest().sites[0].path).toBe(join(p.dir, "snippet.txt"));

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

test("apply — a missing file is refused before anything is sent", () => {
  const p = echoProject();
  const def = p.write(
    "proc.yaml",
    [
      `name: ${uid("import")}`,
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
  expect(existsSync(join(p.dir, "manifest.json"))).toBe(false);
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

// ── the evaluator's importer ────────────────────────────────────────────────────

function tsProject(): Project {
  const p = project(
    `resolvers:\n  import: { phase: code, ext: .ts, command: [node, ${join(REPO, "eval-node/import.ts")}] }\n`,
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

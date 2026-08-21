import { mkdirSync, mkdtempSync, readFileSync, writeFileSync, existsSync } from "fs";
import { tmpdir } from "os";
import { join, relative } from "path";
import { beforeAll, expect, test } from "vitest";
import { buildGenctlBinary, runCli } from "../helpers/cli.ts";
import { waitForInstance } from "../helpers/client.ts";
import { startedID, uid } from "../helpers/genctl.ts";

// Source resolution: a `$<resolver>: <path>` leaf is replaced by a string a registered
// binary produces, before anything reaches the server. specs/source-resolution.md.
//
// The fake resolver below returns file contents verbatim — the mechanism is
// language-agnostic, and a plain text template exercises every part of it except tsc.
// The bun-runtime importer gets its own tests at the bottom.

let bin: string;
beforeAll(() => {
  bin = buildGenctlBinary();
}, 60_000);

const REPO = new URL("../../", import.meta.url).pathname;

/** A resolver that echoes each site's file and dumps the manifest for inspection. */
const ECHO_RESOLVER = `
const m = await Bun.stdin.json();
await Bun.write(new URL("./manifest.json", import.meta.url).pathname, JSON.stringify(m, null, 2));
if (m.mode === "types") process.exit(0);
const code = [];
for (const s of m.sites) code.push(await Bun.file(s.path).text());
process.stdout.write(JSON.stringify({ code }));
`;

type Project = { dir: string; write: (name: string, body: string) => string; manifest: () => any };

function project(resolvers: string): Project {
  const dir = mkdtempSync(join(tmpdir(), "genroc_import_"));
  writeFileSync(join(dir, "genroc.yaml"), resolvers, "utf8");
  writeFileSync(join(dir, "echo.ts"), ECHO_RESOLVER, "utf8");
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
  return project(`resolvers:\n  import: { phase: code, command: [bun, run, echo.ts] }\n`);
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
  expect(instance.context.output).toBe(snippet);
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
  expect(instance.context.output).toBe("relative\n");
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
  expect(r.stderr).toContain("genroc.yaml");
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
  const p = project(`resolvers:\n  import: { phase: code, command: [bun, run, fail.ts] }\n`);
  p.write("fail.ts", 'console.error("summarize.ts(3,7): error TS2322: nope");\nprocess.exit(1);\n');
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
    `resolvers:\n  import: { phase: code, ext: .ts, command: [bun, run, echo.ts] }\n`,
  );
  writeFileSync(join(p.dir, "echo.ts"), ECHO_RESOLVER, "utf8");
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
  expect(instance.context.output).toBe("$import: ./body.txt");
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

// ── the bun-runtime importer ────────────────────────────────────────────────────

function tsProject(): Project {
  const p = project(
    `resolvers:\n  import: { phase: code, ext: .ts, command: [bun, run, ${join(REPO, "bun-runtime/import.ts")}] }\n`,
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
    "      type: fetch",
    "      url: http://localhost:9999/eval",
    "      body:",
    `        code: "$import: ${script}"`,
    '        input: "$: input"',
    "      responses:",
    "        200: { type: object, properties: { fee: { type: number } }, required: [fee] }",
    "    timeout: 5s",
    "    switch: [{ goto: end }]",
    "",
  ].join("\n");
}

test("bun-runtime importer — generates declarations keyed by the script's path", () => {
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

test("bun-runtime importer — a type error is a failed import, so nothing is stored", () => {
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

test("bun-runtime importer — a checked script applies as a self-contained function body", () => {
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

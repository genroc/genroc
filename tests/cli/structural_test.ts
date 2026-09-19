import { mkdtempSync, readFileSync, writeFileSync } from "fs";
import { tmpdir } from "os";
import { join } from "path";
import { beforeAll, expect, test } from "vitest";
import { buildGenctlBinary, runCli } from "../helpers/cli.ts";

// A REGISTERED structural resolver: the same manifest the code phase sends, minus types (it runs
// before inference), answered with one value per site instead of one string. `$process` is the
// built-in instance; this is any other. specs/source-resolution.md §Registered structural resolvers.

let bin: string;
beforeAll(() => {
  bin = buildGenctlBinary();
}, 60_000);

const OFFLINE = { GENROC_SERVER: "http://127.0.0.1:1" };

/** An inferred type is stored as a `$ref` into its own `$defs`; this is what it points at. */
// eslint-disable-next-line @typescript-eslint/no-explicit-any
function resolved(t: any): any {
  const name = typeof t?.$ref === "string" ? t.$ref.replace("#/$defs/", "") : "";
  return t?.$defs?.[name] ?? t;
}

/** A fragment loader: each site's argument names a JSON file beside the definition. The file's
 *  text is spliced into the reply VERBATIM, since JSON.parse would round a big number here, in
 *  the resolver, before genctl ever saw it. */
const FRAG_RESOLVER = `
import { readFileSync, writeFileSync } from "node:fs";
import { resolve } from "node:path";
const chunks = [];
for await (const c of process.stdin) chunks.push(c);
const m = JSON.parse(Buffer.concat(chunks).toString("utf8"));
writeFileSync(new URL("./manifest.json", import.meta.url), JSON.stringify(m, null, 2));
const values = [];
for (const p of m.processes)
  for (const s of p.sites) values.push(readFileSync(resolve(p.dir, s.argument), "utf8").trim());
process.stdout.write('{"values":[' + values.join(",") + "]}");
`;

const REGISTRY = "resolvers:\n  - { name: frag, phase: structural, ext: [.json], command: [node, frag.mjs] }\n";

type Project = { dir: string; write: (name: string, body: string) => string; manifest: () => any };

function project(registry = REGISTRY): Project {
  const dir = mkdtempSync(join(tmpdir(), "genroc_structural_"));
  writeFileSync(join(dir, ".genroc"), registry, "utf8");
  writeFileSync(join(dir, "frag.mjs"), FRAG_RESOLVER, "utf8");
  return {
    dir,
    write: (name, body) => {
      const path = join(dir, name);
      writeFileSync(path, body, "utf8");
      return path;
    },
    manifest: () => JSON.parse(readFileSync(join(dir, "manifest.json"), "utf8")),
  };
}

// Written as text, not stringified: the default is past float64, and it must reach the type
// view exact the way any authored number does.
const INPUT = '{"type":"object","properties":{"n":{"type":"number","default":12345678901234567890}}}';

test("a slot directive is filled with the value the resolver answers", () => {
  const p = project();
  p.write("input.json", INPUT);
  const def = p.write(
    "proc.genroc.yaml",
    ['name: frag-slot', 'input_schema: "$frag: ./input.json"', "tasks: []", "output: { ok: true }", ""].join("\n"),
  );
  const r = runCli(bin, ["schema", "type", "frag-slot", "input", "--json", "-f", def], OFFLINE);
  expect(r.stderr).toBe("");
  expect(resolved(JSON.parse(r.stdout))).toMatchObject({ properties: { n: { type: "number" } } });
  expect(r.stdout, "a number in a fragment is spliced exact, not through float64").toContain(
    "12345678901234567890",
  );
});

test("the manifest is the code phase's, in structural mode and with no types", () => {
  const p = project();
  p.write("input.json", INPUT);
  const def = p.write(
    "proc.genroc.yaml",
    ['name: frag-manifest', 'input_schema: "$frag: ./input.json"', "tasks: []", "output: { ok: true }", ""].join("\n"),
  );
  expect(runCli(bin, ["schema", "type", "frag-manifest", "input", "-f", def], OFFLINE).ok).toBe(true);
  const m = p.manifest();
  expect(m.mode).toBe("structural");
  expect(m.processes).toHaveLength(1);
  expect(m.processes[0]).toMatchObject({ name: "frag-manifest", dir: p.dir, file: "proc.genroc.yaml" });
  expect(m.processes[0].sites).toEqual([
    { level: "process", pointer: ["input_schema"], argument: "./input.json" },
  ]);
  expect(m.processes[0], "there is nothing to type before inference has run").not.toHaveProperty("$defs");
});

test("a spread directive pre-fills the mapping around it", () => {
  const p = project();
  p.write(
    "call.json",
    JSON.stringify({
      result_schema: { type: "object", properties: { label: { type: "string" } }, required: ["label"] },
    }),
  );
  const def = p.write(
    "proc.genroc.yaml",
    [
      "name: frag-spread",
      "tasks:",
      "  - id: call",
      "    action:",
      "      type: external",
      '      <<: "$frag: ./call.json"',
      "      input: { n: 1 }",
      "      timeout: 5s",
      "    switch: [{ goto: end }]",
      "output: { ok: true }",
      "",
    ].join("\n"),
  );
  const r = runCli(bin, ["schema", "type", "frag-spread", "tasks.call.action.result", "--json", "-f", def], OFFLINE);
  expect(r.stderr).toBe("");
  expect(JSON.parse(r.stdout).properties).toHaveProperty("label");
});

test("a spread answered with anything but a mapping is refused by name", () => {
  const p = project();
  p.write("call.json", "[1, 2]");
  const def = p.write(
    "proc.genroc.yaml",
    [
      "name: frag-bad-spread",
      "tasks:",
      "  - id: call",
      "    action:",
      "      type: external",
      '      <<: "$frag: ./call.json"',
      "      input: { n: 1 }",
      "      timeout: 5s",
      "    switch: [{ goto: end }]",
      "output: { ok: true }",
      "",
    ].join("\n"),
  );
  const r = runCli(bin, ["schema", "type", "frag-bad-spread", "output", "-f", def], OFFLINE);
  expect(r.ok).toBe(false);
  expect(r.stderr).toContain("spreads a mapping");
  expect(r.stderr).toContain("a list");
  expect(r.stderr).toContain("tasks.call.action");
});

test("one call carries every site that named the resolver, and each is filled", () => {
  const p = project();
  p.write("input.json", INPUT);
  p.write("config.json", JSON.stringify({ type: "object", properties: { region: { type: "string" } } }));
  const def = p.write(
    "proc.genroc.yaml",
    [
      "name: frag-batch",
      'input_schema: "$frag: ./input.json"',
      'config_schema: "$frag: ./config.json"',
      "tasks: []",
      "output: { ok: true }",
      "",
    ].join("\n"),
  );
  const r = runCli(bin, ["schema", "type", "frag-batch", "input", "--json", "-f", def], OFFLINE);
  expect(r.stderr).toBe("");
  expect(resolved(JSON.parse(r.stdout)).properties).toHaveProperty("n");
  const sites = p.manifest().processes[0].sites;
  expect(sites).toHaveLength(2);
  expect(sites.map((s: { argument: string }) => s.argument).sort()).toEqual(["./config.json", "./input.json"]);
});

test("an answer with the wrong number of values is refused, not spliced by position", () => {
  const p = project("resolvers:\n  - { name: frag, phase: structural, ext: [.json], command: [node, short.mjs] }\n");
  p.write("short.mjs", 'process.stdout.write(JSON.stringify({ values: [] }));\n');
  p.write("input.json", INPUT);
  const def = p.write(
    "proc.genroc.yaml",
    ['name: frag-short', 'input_schema: "$frag: ./input.json"', "tasks: []", "output: { ok: true }", ""].join("\n"),
  );
  const r = runCli(bin, ["schema", "type", "frag-short", "input", "-f", def], OFFLINE);
  expect(r.ok).toBe(false);
  expect(r.stderr).toContain("returned 0 values for 1 sites");
});

test("a structural entry asking for types is refused where it is written", () => {
  const p = project(
    "resolvers:\n  - { name: frag, phase: structural, ext: [.json], command: [node, frag.mjs], types: { X: process.input } }\n",
  );
  p.write("input.json", INPUT);
  const def = p.write(
    "proc.genroc.yaml",
    ['name: frag-typed', 'input_schema: "$frag: ./input.json"', "tasks: []", "output: { ok: true }", ""].join("\n"),
  );
  const r = runCli(bin, ["schema", "type", "frag-typed", "input", "-f", def], OFFLINE);
  expect(r.ok).toBe(false);
  expect(r.stderr).toContain("cannot be handed types");
  expect(r.stderr).toContain(".genroc");
});

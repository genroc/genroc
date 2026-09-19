import { mkdtempSync, readFileSync, readdirSync } from "fs";
import { tmpdir } from "os";
import { join } from "path";
import { beforeAll, afterAll, expect, test } from "vitest";
import { buildGenctlBinary, runCli } from "../helpers/cli.ts";
import { at, Doc, Lsp, useWorkspace } from "./helpers.ts";

// What `genctl init` writes, read the way a person first reads it: opened in an editor. This is
// the check `tests/cli/init_scaffold_test.ts` cannot make — `genctl schema` answers about ONE
// slot and says nothing about the others, so a scaffold can typecheck there and still open
// covered in red. It did: the `$process` spread wrote an inferred `raises` payload whose
// property was both required and defaulted, which is not a valid schema document.

let lsp: Lsp;
beforeAll(async () => {
  useWorkspace();
  lsp = await Lsp.start();
}, 60_000);
afterAll(async () => lsp?.stop());

function scaffold(...flags: string[]): string {
  const dir = mkdtempSync(join(tmpdir(), "genroc_scaffold_lsp_"));
  const r = runCli(buildGenctlBinary(), ["init", dir, "-y", ...flags], {
    GENROC_SERVER: "http://127.0.0.1:1",
  });
  expect(r.exitCode, r.stderr).toBe(0);
  return join(dir, "definitions");
}

async function diagnose(defs: string): Promise<Record<string, string[]>> {
  const out: Record<string, string[]> = {};
  for (const f of readdirSync(defs).filter((n) => n.endsWith(".genroc.yaml"))) {
    const path = join(defs, f);
    const doc: Doc = { uri: `file://${path}`, text: readFileSync(path, "utf8") };
    out[f] = await lsp.diagnostics(doc);
  }
  return out;
}

test("the base scaffold opens with nothing underlined", async () => {
  expect(await diagnose(scaffold())).toEqual({ "hello.genroc.yaml": [] });
});

test("the eval-node scaffold opens with nothing underlined", async () => {
  expect(await diagnose(scaffold("--eval-node"))).toEqual({
    "hello.genroc.yaml": [],
    "script-node.genroc.yaml": [],
  });
});

test("the scaffold's $process directive shows the child it spreads in", async () => {
  const defs = scaffold("--eval-node");
  const path = join(defs, "hello.genroc.yaml");
  const doc: Doc = { uri: `file://${path}`, text: readFileSync(path, "utf8") };
  const md = await lsp.hover(at('      <<: "$process: <^./script-node.genroc.yaml>"', doc));
  expect(md).toBe(
    [
      "```yaml",
      "name: script-node",
      "input_schema:",
      "  type: object",
      "  properties:",
      "    code: {type: string}",
      "    input:",
      "      description: what the script is called with; the caller types it",
      "    timeout_ms: {type: integer, default: 5000}",
      "  required: [code]",
      // The scaffold narrows `result_schema` itself, and the top type it would have taken says so.
      "result_schema: {} # the key written here wins",
      "raises:",
      "  script_threw:",
      "    type: object",
      "    properties:",
      "      name: {type: string}",
      '      stack: {type: ["null", string]}',
      "    required: [name, stack]",
      "  script_timeout:",
      "    type: object",
      "    properties:",
      "      budget_ms: {type: integer}",
      "    required: [budget_ms]",
      '  script_unknown: {type: "null"}',
      "```",
    ].join("\n"),
  );
});

// The code phase is never run by the editor, and the `$import` line says so rather than
// reproducing `genctl types` under a hover.
test("the scaffold's $import directive says it resolves at apply", async () => {
  const defs = scaffold("--eval-node");
  const path = join(defs, "hello.genroc.yaml");
  const doc: Doc = { uri: `file://${path}`, text: readFileSync(path, "utf8") };
  expect(await lsp.hover(at('        code: "$import: <^./greet.ts>"', doc))).toBe(
    "`import` runs at apply, in the code phase, and fills this slot with a string.",
  );
});

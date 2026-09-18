import { mkdtempSync, readFileSync, readdirSync } from "fs";
import { tmpdir } from "os";
import { join } from "path";
import { beforeAll, afterAll, expect, test } from "vitest";
import { buildGenctlBinary, runCli } from "../helpers/cli.ts";
import { Doc, Lsp, useWorkspace } from "./helpers.ts";

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

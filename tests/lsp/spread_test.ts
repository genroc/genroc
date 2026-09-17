import { mkdtempSync, writeFileSync } from "fs";
import { tmpdir } from "os";
import { join } from "path";
import { afterAll, beforeAll, expect, test } from "vitest";
import { Lsp, useWorkspace } from "./helpers.ts";

// The structural phase in the editor. A `<<` spread fills `name`, `result_schema` and `raises`
// from the child definition, so a server that skips resolution reports a document nobody
// applies — `unknown field "<<"` first, then every key the spread would have supplied.
// specs/source-resolution.md, specs/language-server.md §4.

let lsp: Lsp;
beforeAll(async () => {
  useWorkspace();
  lsp = await Lsp.start();
}, 60_000);
afterAll(async () => lsp?.stop());

const CHILD = [
  "name: spread-child",
  "input_schema:",
  "  type: object",
  "  properties: { n: { type: number } }",
  "  required: [n]",
  "tasks:",
  "  - id: only",
  "    switch:",
  "      - case: 'input.n < 0'",
  "        raise: { code: negative, message: 'n is negative' }",
  "      - goto: end",
  "    output: { doubled: '$: input.n * 2' }",
  "output: { doubled: '$: outputs.only.doubled' }",
  "",
].join("\n");

/** A parent whose child call is filled by the spread, plus whatever lines the test adds. */
function parent(...extra: string[]): string {
  return [
    "name: spread-parent",
    "input_schema:",
    "  type: object",
    "  properties: { n: { type: number } }",
    "tasks:",
    "  - id: call",
    "    action:",
    "      type: child",
    '      <<: "$process: ./child.genroc.yaml"',
    "      input: { n: '$: input.n' }",
    // Without this the task exposes nothing as `outputs.call`, spread or no spread.
    "    output: \"$: self.result\"",
    ...extra,
    "    switch: [{ goto: end }]",
    "output: { r: '$: outputs.call.doubled' }",
    "",
  ].join("\n");
}

/** A project on disk: the spread's argument is a path relative to the file holding it. */
function project(text: string): { uri: string; text: string } {
  const dir = mkdtempSync(join(tmpdir(), "genroc-lsp-spread-"));
  writeFileSync(join(dir, "child.genroc.yaml"), CHILD);
  const p = join(dir, "parent.genroc.yaml");
  writeFileSync(p, text);
  return { uri: `file://${p}`, text };
}

test("a spread underlines nothing, and its result types the reader downstream", async () => {
  expect(
    await lsp.diagnostics(project(parent())),
    "`<<` is an unknown field until the structural phase runs, and `doubled` is not there either",
  ).toEqual([]);
});

test("a rule may name a code the spread brought across", async () => {
  // `negative` is nowhere in this file: it comes from the child's raise clause through `raises`.
  expect(
    await lsp.diagnostics(project(parent("    on_error: [{ code: [negative], goto: end }]"))),
    "the raise set arrived with the spread, so the rule names a code the call declares",
  ).toEqual([]);
});

test("a spread naming a file that is not there is reported, not swallowed", async () => {
  const broken = project(parent()).text.replace("./child.genroc.yaml", "./missing.genroc.yaml");
  const ds = await lsp.diagnostics(project(broken));
  expect(ds.length, `expected one diagnostic, got ${JSON.stringify(ds)}`).toBe(1);
  expect(ds[0]).toContain("missing.genroc.yaml");
});

test("a mistake the author DID write is still underlined through a spread", async () => {
  // The expression is wrong, not the spread: resolution must not swallow the verdict with it.
  const ds = await lsp.diagnostics(project(parent().replace("input.n * 2", "input.n * 2")
    .replace("outputs.call.doubled", "outputs.call.nope")));
  expect(ds.length, `expected one diagnostic, got ${JSON.stringify(ds)}`).toBe(1);
  expect(ds[0]).toContain("nope");
});

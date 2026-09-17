import { mkdtempSync, writeFileSync } from "fs";
import { tmpdir } from "os";
import { join } from "path";
import { afterAll, beforeAll, expect, test } from "vitest";
import { at, Lsp, useWorkspace } from "./helpers.ts";

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

// Diagnostics are not the only answer the spread changes: `result_schema` arrives with it, so
// the type of `self.result` — and everything a reader walks out of it — exists only once the
// structural phase has run.
test("hover reads a type the spread supplied", async () => {
  const doc = project(parent());
  expect(
    await lsp.hover(at('    output: "$: self.<^result>"', doc)),
    "self.result is typed by the result_schema the spread filled in",
  ).toBe("`self.result` → `object{doubled}`");
});

test("hover walks INTO a spread-supplied type", async () => {
  const doc = project(parent().replace('"$: self.result"', '"$: self.result.doubled"'));
  expect(await lsp.hover(at('    output: "$: self.result.<^doubled>"', doc))).toBe(
    "`self.result.doubled` → `number`",
  );
});

test("completion offers the members the spread brought across", async () => {
  const doc = project(parent().replace('"$: self.result"', '"$: self.result."'));
  expect(
    await lsp.completions(at('    output: "$: self.result.<|>"', doc)),
    "without the spread the slot recovers as {} and offers nothing",
  ).toContain("doubled");
});

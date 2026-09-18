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
    // Required, because the parent forwards `n` straight into a child that requires it: an
    // optional property reads as nullable, and the call site's declared input_schema (spread
    // in from the child) refuses a null where the child declares a number.
    "  required: [n]",
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

// `raises` arrives with the spread too, and an `on_error` rule's `code` is a closed set the
// server offers. It is a THIRD path: `completeAt` dispatches to `errorCodeValues` before it
// ever reaches the expression scope, and that one reads the action out of the document.
test("completion offers a raise code the spread brought across", async () => {
  const doc = project(parent("    on_error:", "      - code: []"));
  expect(
    await lsp.completions(at("      - code: [<|>]", doc)),
    "`negative` is declared by the child's raise clause and arrives through `raises`",
  ).toContain("negative");
});

// `input_schema` is the fourth thing the spread fills, and the only one that is a COPY rather
// than an inference — the child's author wrote it. It is what makes a child call checkable with
// no server, which is what the input check has never been.
test("the spread brings the child's input_schema across, so a bad input is caught here", async () => {
  const misspelled = parent().replace("input: { n: '$: input.n' }", "input: { m: '$: input.n' }");
  const ds = await lsp.diagnostics(project(misspelled));
  expect(ds.length, `expected one diagnostic, got ${JSON.stringify(ds)}`).toBe(1);
  // The KEY the child does not declare. Nothing read the child out of a database to know that.
  expect(ds[0]).toContain("m");
});

test("a spread that fills input_schema still leaves an explicit one alone", async () => {
  // An explicit key beats the spread, as everywhere — so a hand-written declaration that the
  // input does satisfy reports nothing, even though the child's own would have refused it.
  const own = parent().replace(
    "      input: { n: '$: input.n' }",
    [
      "      input: { n: '$: input.n', extra: 1 }",
      "      input_schema:",
      "        type: object",
      "        properties: { n: { type: number }, extra: { type: number } }",
    ].join("\n"),
  );
  const ds = await lsp.diagnostics(project(own));
  expect(ds).toEqual([]);
});

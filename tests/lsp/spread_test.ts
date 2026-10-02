import { mkdtempSync, writeFileSync } from "fs";
import { tmpdir } from "os";
import { join } from "path";
import { afterAll, beforeAll, expect, test } from "vitest";
import { at, Lsp, useWorkspace } from "./helpers.ts";

// The structural phase in the editor: skipped, a `<<` spread reads as `unknown field "<<"` plus
// every key it supplies. specs/source-resolution.md, specs/language-server.md §4.

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
  "  properties: { n: { type: number, description: 'the number to double' } }",
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
    // Required: optional reads as nullable, and the input_schema spread from the child refuses null.
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

// A separate path: `completeAt` dispatches to `errorCodeValues` before the expression scope.
test("completion offers a raise code the spread brought across", async () => {
  const doc = project(parent("    on_error:", "      - code: []"));
  expect(
    await lsp.completions(at("      - code: [<|>]", doc)),
    "`negative` is declared by the child's raise clause and arrives through `raises`",
  ).toContain("negative");
});

// The one key the spread COPIES rather than infers, so a child call is checkable with no server.
test("the spread brings the child's input_schema across, so a bad input is caught here", async () => {
  const misspelled = parent().replace("input: { n: '$: input.n' }", "input: { m: '$: input.n' }");
  const ds = await lsp.diagnostics(project(misspelled));
  expect(ds.length, `expected one diagnostic, got ${JSON.stringify(ds)}`).toBe(1);
  // The KEY the child does not declare.
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

// The one hover that is not a line: the mapping as YAML, from the call the structural phase makes.
test("hover on the directive shows what the spread fills in, as YAML", async () => {
  const doc = project(parent());
  expect(await lsp.hover(at('      <<: "$process: <^./child.genroc.yaml>"', doc))).toBe(
    [
      "```yaml",
      "name: spread-child",
      "input_schema:",
      "  type: object",
      "  properties:",
      // `n` is quoted because a bare one is YAML's `no`; the prose is the child author's,
      // which the copy keeps.
      "    n: {description: the number to double, type: number}",
      '  required: ["n"]',
      "result_schema:",
      "  type: object",
      "  properties:",
      "    doubled: {type: number}",
      "  required: [doubled]",
      "raises:",
      '  negative: {type: "null"}',
      "```",
    ].join("\n"),
  );
});

// The copy is worth having for its prose, and Canonicalize drops `description` — so the key
// hover in the CALLER is where a lost one would show.
test("a key in the caller's input carries the child author's description", async () => {
  const doc = project(parent());
  expect(await lsp.hover(at("      input: { <^n>: '$: input.n' }", doc))).toContain(
    "the number to double",
  );
});

test("a key written beside the spread is marked as the one that wins", async () => {
  const own = parent().replace(
    "      input: { n: '$: input.n' }",
    [
      "      input: { n: '$: input.n' }",
      "      result_schema: { type: object, properties: { doubled: { type: number } } }",
    ].join("\n"),
  );
  const md = await lsp.hover(at('      <<: "$process: <^./child.genroc.yaml>"', project(own)));
  expect(md).toContain("result_schema: # the key written here wins");
  expect(md, "the other keys are taken, and say nothing").not.toContain("name: spread-child #");
  // The note is on the KEY only where the value opens a block; a `{}` or a scalar would carry it
  // on the value, since yaml.v3 otherwise prints it on the next line (scaffold_test pins that).
});

test("a directive that does not resolve shows no structure", async () => {
  const broken = parent().replace("./child.genroc.yaml", "./missing.genroc.yaml");
  const md = await lsp.hover(at('      <<: "$process: <^./missing.genroc.yaml>"', project(broken)));
  expect(md).not.toContain("```");
});

// A `<<` whose value defdoc merges itself has no index node, so without an answer of its own the
// cursor resolves to the enclosing mapping.
test("hover on the `<<` key says what a merge does, whatever its value", async () => {
  const explains = (md: string) => {
    expect(md).toContain("Merges a mapping into this one");
    expect(md).toContain("A key written beside it wins");
    expect(md, "the key is not the value: no structure here").not.toContain("```");
  };
  explains(await lsp.hover(at('      <^<<>: "$process: ./child.genroc.yaml"', project(parent()))));

  // The same keys the spread would have brought, written by hand under a nested mapping.
  const nested = parent().replace(
    '      <<: "$process: ./child.genroc.yaml"',
    [
      "      <<:",
      "        name: spread-child",
      "        result_schema: { type: object, properties: { doubled: { type: number } }, required: [doubled] }",
    ].join("\n"),
  );
  const doc = project(nested);
  const ds = await lsp.diagnostics(doc);
  expect(ds, `a nested mapping is a merge defdoc performs: ${JSON.stringify(ds)}`).toEqual([]);
  explains(await lsp.hover(at("      <^<<>:", doc)));
});

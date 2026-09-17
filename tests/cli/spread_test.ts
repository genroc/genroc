import { mkdtempSync, writeFileSync } from "fs";
import { tmpdir } from "os";
import { join } from "path";
import { beforeAll, expect, test } from "vitest";
import { buildGenctlBinary, runCli } from "../helpers/cli.ts";

// The spread form: `<<: "$<resolver>: <params>"` pre-fills the mapping it sits in, where the
// same directive in a VALUE fills that slot. `$process` is the built-in that answers it with
// another definition's call-site types. specs/source-resolution.md §The spread form, §`$process`.

let bin: string;
beforeAll(() => {
  bin = buildGenctlBinary();
}, 60_000);

const OFFLINE = { GENROC_SERVER: "http://127.0.0.1:1" };

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
  "    output: { doubled: '$: input.n * 2', label: 'x' }",
  "output: { doubled: '$: outputs.only.doubled', label: '$: outputs.only.label' }",
  "",
].join("\n");

function parent(body: string[]): string {
  return [
    "name: spread-parent",
    "input_schema:",
    "  type: object",
    "  properties: { n: { type: number } }",
    "tasks:",
    "  - id: call",
    "    action:",
    "      type: child",
    ...body,
    "    switch: [{ goto: end }]",
    "output: { r: '$: outputs.call' }",
    "",
  ].join("\n");
}

/** A project holding the child, plus whatever parent the test writes. */
function project(parentBody: string[], config?: string): { dir: string; parent: string } {
  const dir = mkdtempSync(join(tmpdir(), "genroc_spread_"));
  if (config !== undefined) writeFileSync(join(dir, ".genroc"), config, "utf8");
  writeFileSync(join(dir, "child.genroc.yaml"), CHILD, "utf8");
  const p = join(dir, "parent.genroc.yaml");
  writeFileSync(p, parent(parentBody), "utf8");
  return { dir, parent: p };
}

const SPREAD = '      <<: "$process: ./child.genroc.yaml"';

test("a spread fills name, result_schema and raises from the child definition", () => {
  const { parent: path } = project([SPREAD, "      input: { n: '$: input.n' }"]);

  const r = runCli(bin, ["schema", "type", "spread-parent", "tasks.call.action.result", "--json", "-f", path], OFFLINE);
  expect(r.stderr).toBe("");
  // The child's OUTPUT is the parent's result_schema -- a rename, not a copy.
  expect(JSON.parse(r.stdout)).toMatchObject({
    type: "object",
    properties: { doubled: { type: "number" }, label: { type: "string" } },
  });

  // `raises` came across too: the child's raise set, keyed by code. Without the spread the
  // rule below names a code the call never declared.
  const withRule = project([
    SPREAD,
    "      input: { n: '$: input.n' }",
    "    on_error: [{ code: [negative], goto: end }]",
  ]);
  const r2 = runCli(bin, ["schema", "type", "spread-parent", "tasks.call.action.result", "--json", "-f", withRule.parent], OFFLINE);
  expect(r2.stderr).toBe("");
});

test("an explicit key beats the spread, and only that key", () => {
  const { parent: path } = project([
    SPREAD,
    "      input: { n: '$: input.n' }",
    "      result_schema: { type: object, properties: { doubled: { type: number } } }",
  ]);
  const r = runCli(bin, ["schema", "type", "spread-parent", "tasks.call.action.result", "--json", "-f", path], OFFLINE);
  expect(r.stderr).toBe("");
  const got = JSON.parse(r.stdout);
  // The hand-written schema won outright -- shallow, so `label` is gone rather than merged in.
  expect(got.properties).toHaveProperty("doubled");
  expect(got.properties).not.toHaveProperty("label");
  // ...while `name`, which the mapping does not carry, still came from the spread.
  const named = runCli(bin, ["schema", "type", "spread-parent", "tasks.call", "--json", "-f", path], OFFLINE);
  expect(named.stderr).toBe("");
});

test("a spread cycle is refused by path, though a recursive CALL is ordinary", () => {
  const dir = mkdtempSync(join(tmpdir(), "genroc_spread_"));
  const self = join(dir, "self.genroc.yaml");
  writeFileSync(
    self,
    [
      "name: spread-self",
      "tasks:",
      "  - id: again",
      "    action:",
      "      type: child",
      '      <<: "$process: ./self.genroc.yaml"',
      "      input: {}",
      "    switch: [{ goto: end }]",
      "output: { done: true }",
      "",
    ].join("\n"),
    "utf8",
  );
  const r = runCli(bin, ["schema", "type", "spread-self", "output", "--json", "-f", self], OFFLINE);
  expect(r.ok).toBe(false);
  expect(r.stderr).toContain("spread cycle");
  expect(r.stderr).toContain("self.genroc.yaml");
});

test("`<<` with a value that is not a directive stays the parse error it always was", () => {
  const { parent: path } = project(["      <<: nope", "      name: spread-child"]);
  const r = runCli(bin, ["schema", "type", "spread-parent", "output", "--json", "-f", path], OFFLINE);
  expect(r.ok).toBe(false);
  expect(r.stderr).toContain("`<<`");
});

test("a .genroc entry of the same name is matched before the built-in", () => {
  // The local entry claims the name AND the suffix, so first-match stops at it and never
  // reaches the built-in -- which is visible because it is an external resolver, not the one
  // genctl answers itself.
  const { parent: path } = project(
    [SPREAD, "      input: { n: '$: input.n' }"],
    "resolvers:\n  - { name: process, phase: structural, ext: [.genroc.yaml], command: [node, x.mjs] }\n",
  );
  const r = runCli(bin, ["schema", "type", "spread-parent", "output", "--json", "-f", path], OFFLINE);
  expect(r.ok).toBe(false);
  expect(r.stderr).toContain("not implemented");
});

test("an override is per suffix: a local entry claiming another one falls through", () => {
  // The honest reading of a first-match table, and the reason there is no shadowing rule:
  // the local entry does not accept this argument, so the built-in still answers it.
  const { parent: path } = project(
    [SPREAD, "      input: { n: '$: input.n' }"],
    "resolvers:\n  - { name: process, phase: structural, ext: [.genroc.json], command: [node, x.mjs] }\n",
  );
  const r = runCli(bin, ["schema", "type", "spread-parent", "tasks.call.action.result", "--json", "-f", path], OFFLINE);
  expect(r.stderr).toBe("");
  expect(JSON.parse(r.stdout).properties).toHaveProperty("label");
});

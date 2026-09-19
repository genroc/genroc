import { mkdtempSync, writeFileSync } from "fs";
import { tmpdir } from "os";
import { join } from "path";
import { afterAll, beforeAll, expect, test } from "vitest";
import { at, Doc, Lsp, useWorkspace } from "./helpers.ts";

// Hover over a `$<resolver>:` directive, for every kind of resolver. A STRUCTURAL one shows what
// it yields, from the same call the pass makes — so a registered fragment loader answers like
// the built-in `$process` does. A CODE one is never run by the editor, so it says only that it
// fills the slot with a string at apply. specs/source-resolution.md.

let lsp: Lsp;
beforeAll(async () => {
  useWorkspace();
  lsp = await Lsp.start();
}, 60_000);
afterAll(async () => lsp?.stop());

const FRAG_RESOLVER = `
import { readFileSync } from "node:fs";
import { resolve } from "node:path";
const chunks = [];
for await (const c of process.stdin) chunks.push(c);
const m = JSON.parse(Buffer.concat(chunks).toString("utf8"));
const values = [];
for (const p of m.processes)
  for (const s of p.sites) values.push(JSON.parse(readFileSync(resolve(p.dir, s.argument), "utf8")));
process.stdout.write(JSON.stringify({ values }));
`;

const REGISTRY = [
  "resolvers:",
  "  - { name: frag, phase: structural, ext: [.json], command: [node, frag.mjs] }",
  "  - name: import",
  "    phase: code",
  "    ext: [.ts]",
  "    command: ['true']",
  "    types: { Input: task.action.input.input, Output: task.action.result }",
  "  - { name: raw, phase: code, command: ['true'] }",
  "",
].join("\n");

const PARENT = [
  "name: hover-parent",
  'input_schema: "$frag: ./input.json"',
  "tasks:",
  "  - id: call",
  "    action:",
  "      type: external",
  '      <<: "$frag: ./call.json"',
  "      input:",
  '        code: "$import: ./x.ts"',
  "        input: { n: '$: input.n' }",
  '        note: "$raw: ./notes.md"',
  "      timeout: 5s",
  '    output: "$: self.result"',
  "    switch: [{ goto: end }]",
  'output: "$: outputs.call"',
  "",
].join("\n");

/** A project on disk: the resolver is a file beside `.genroc`, and the fragments sit beside the definition. */
function project(text = PARENT): Doc {
  const dir = mkdtempSync(join(tmpdir(), "genroc-lsp-directive-"));
  writeFileSync(join(dir, ".genroc"), REGISTRY);
  writeFileSync(join(dir, "frag.mjs"), FRAG_RESOLVER);
  writeFileSync(
    join(dir, "input.json"),
    JSON.stringify({ type: "object", properties: { n: { type: "number" } }, required: ["n"] }),
  );
  writeFileSync(
    join(dir, "call.json"),
    JSON.stringify({
      result_schema: { type: "object", properties: { label: { type: "string" } }, required: ["label"] },
    }),
  );
  const p = join(dir, "parent.genroc.yaml");
  writeFileSync(p, text);
  return { uri: `file://${p}`, text };
}

test("a registered structural resolver runs in the editor, so nothing is underlined", async () => {
  expect(await lsp.diagnostics(project())).toEqual([]);
});

test("hover on a slot directive shows the value it is filled with", async () => {
  expect(await lsp.hover(at('input_schema: "$frag: <^./input.json>"', project()))).toBe(
    ["```yaml", "type: object", "properties:", "  n: {type: number}", 'required: ["n"]', "```"].join("\n"),
  );
});

test("hover on a spread directive shows the mapping it fills in", async () => {
  const md = await lsp.hover(at('      <<: "$frag: <^./call.json>"', project()));
  expect(md).toBe(
    [
      "```yaml",
      "result_schema:",
      "  type: object",
      "  properties:",
      "    label: {type: string}",
      "  required: [label]",
      "```",
    ].join("\n"),
  );
});

test("hover on a code directive says it resolves at apply, and computes nothing", async () => {
  const doc = project();
  const line = (name: string) => `\`${name}\` runs at apply, in the code phase, and fills this slot with a string.`;
  expect(await lsp.hover(at('        code: "$import: <^./x.ts>"', doc))).toBe(line("import"));
  // With or without `types` in its entry: the editor is not `genctl types`.
  expect(await lsp.hover(at('        note: "$raw: <^./notes.md>"', doc))).toBe(line("raw"));
});

test("a structural directive that fails shows no value; the diagnostic says why", async () => {
  const doc = project(PARENT.replace("./input.json", "./missing.json"));
  expect(await lsp.hover(at('input_schema: "$frag: <^./missing.json>"', doc))).not.toContain("```");
  const ds = await lsp.diagnostics(doc);
  expect(ds.length, JSON.stringify(ds)).toBe(1);
  expect(ds[0]).toContain("missing.json");
});

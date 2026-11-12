import { afterAll, beforeAll, expect, test } from "vitest";
import { at, Doc, edit, Lsp, orders, useWorkspace } from "./helpers.ts";

// `x-` keys at a set structure are the author's, never read (specs/extension-keys.md §1). Beside
// them, two keys that only START with `x-` and are data: a header, and a property name.

let lsp: Lsp;
let doc: Doc;
beforeAll(async () => {
  useWorkspace();
  lsp = await Lsp.start();
  doc = extended();
}, 60_000);
afterAll(async () => lsp?.stop());

const extended = () => edit(orders, {
  "name: orders\n": 'name: orders\nx-builder:\n  zoom: 2\n  note: "$: input.customer_id"\n',
  "  - id: price\n": '  - id: price\n    x-pos: { x: 10, y: 20 }\n    x-calc: "$: self.result.total"\n',
  "    customer_id: { type: string }": "    customer_id: { type: string, x-order: 1 }\n    x-id: { type: string }",
  '        X-Currency: "${ input.currency }"': '        X-Currency: "${ input.currency }"\n        x-api-key: "${ input.currency }"',
});

test("the fixture registers", async () => {
  expect(await lsp.diagnostics(doc)).toEqual([]);
});

test("hover on an x- key says it is the author's and never read", async () => {
  for (const [cursor, key] of [
    ["<^x-builder>:", "x-builder"],
    ["<^x-pos>: { x: 10", "x-pos"],
    ["<^x-order>: 1", "x-order"],
  ] as const) {
    expect(await lsp.hover(at(cursor, doc)), `${key}: a hover always has a line`).toContain(
      `\`${key}\` is an extension key`,
    );
  }
});

test("hover inside an x- value names the key it is in, and types nothing", async () => {
  expect(await lsp.hover(at("  <^zoom>: 2", doc))).toContain("`x-builder` is an extension key");
  const inExpression = await lsp.hover(at('x-calc: "$: self.result.<^total>"', doc));
  expect(inExpression, "it looks like an expression and is never evaluated").toContain(
    "`x-calc` is an extension key",
  );
  expect(inExpression).not.toContain("number");
});

test("a key that only starts with x- where values are data is not an extension key", async () => {
  for (const cursor of ["<^x-api-key>: ", "<^x-id>: { type: string }"]) {
    expect(await lsp.hover(at(cursor, doc)), cursor).not.toContain("extension key");
  }
});

test("completion offers nothing inside an x- value, and never an x- key beside one", async () => {
  expect(await lsp.completions(at("  zoom: 2\n  <|>note", doc))).toEqual([]);
  const beside = await lsp.completions(at("    x-calc: \"$: self.result.total\"\n    <|>action:", doc));
  expect(beside, "the task's own keys are still offered").toContain("only_once");
  expect(beside.filter((k) => k.startsWith("x-"))).toEqual([]);
});

test("a $: string inside an x- value is not marked as an expression", async () => {
  const lines = doc.text.split("\n");
  const xLines = new Set(lines.flatMap((l, i) => (/^\s*(note|x-calc):/.test(l) ? [i] : [])));
  expect(xLines.size).toBe(2);
  const marked = (await lsp.semanticTokens(doc)).filter((t) => xLines.has(t.line));
  expect(marked, "an x- value is data, so painting it as an expression says it runs").toEqual([]);
});

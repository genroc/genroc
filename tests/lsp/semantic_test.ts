import { afterAll, beforeAll, expect, test } from "vitest";
import { Lsp, edit, orders, useWorkspace } from "./helpers.ts";

// Highlighting the real binary produces, over the same fixture every other suite reads.
//
// The grammar cannot answer this: it sees `"$: tick"` and not the slot holding it. These tests
// are about that difference — the same characters in two slots, marked in one and not the other.

let lsp: Lsp;

beforeAll(async () => {
  useWorkspace();
  lsp = await Lsp.start();
});
afterAll(async () => lsp?.stop());

const kindOf = (toks: { text: string; kind: string }[], text: string) =>
  toks.find((t) => t.text === text)?.kind ?? "";

test("a $: leaf is marked and lexed", async () => {
  const toks = await lsp.semanticTokens(orders);
  expect(kindOf(toks, "$:"), "nothing else says the scalar computes").toBe("keyword");
  expect(kindOf(toks, "self")).toBe("variable");
  expect(kindOf(toks, "??")).toBe("operator");
  expect(kindOf(toks, "1000")).toBe("number");
});

test("a ${ } interpolation is marked inside the string it renders into", async () => {
  const toks = await lsp.semanticTokens(orders);
  expect(kindOf(toks, "${")).toBe("keyword");
  expect(kindOf(toks, "}")).toBe("keyword");
  expect(kindOf(toks, "customer_id")).toBe("variable");
  // The literal half of the same scalar is not marked at all.
  expect(toks.some((t) => t.text.includes("api.example.com"))).toBe(false);
});

// The report this whole change came from: `id` holds text, so a `$:` written there is the
// literal string `$: tick`. A grammar paints it anyway; the server knows the slot.
test("the same marker in a literal slot is left alone", async () => {
  const doc = edit(orders, { "  - id: price": `  - id: "$: price"` });
  const toks = await lsp.semanticTokens(doc);
  const idLine = doc.text.split("\n").findIndex((l) => l.includes(`id: "$: price"`));
  expect(idLine, "the edit did not apply").toBeGreaterThan(0);
  expect(
    toks.filter((t) => t.line === idLine),
    "id is a plain string; a $: written there is text",
  ).toEqual([]);
  expect(kindOf(toks, "$:"), "no marker was found anywhere, so this proves nothing").toBe("keyword");
});

// One referent, one kind. Two kinds paint a path in two colours in any editor that maps them
// apart, which is what was reported twice against the grammar.
test("a member path is one kind from root to leaf", async () => {
  const toks = await lsp.semanticTokens(orders);
  const path = ["self", "result", "total"].map((seg) => kindOf(toks, seg));
  expect(path.every((k) => k !== ""), `unmarked segment in ${path.join("/")}`).toBe(true);
  expect(new Set(path).size, `self.result.total is ${path.join("/")}`).toBe(1);
});

// A `case` carries no marker at all: only the slot says it is an expression.
test("a bare case is lexed", async () => {
  const toks = await lsp.semanticTokens(orders);
  expect(kindOf(toks, ">")).toBe("operator");
  expect(kindOf(toks, "charged")).toBe("variable");
});

test("every routing target is one kind", async () => {
  const toks = await lsp.semanticTokens(orders);
  const targets = toks.filter((t) => t.text === "$review" || t.text === "$fulfil" || t.text === "end");
  expect(targets.length, "routing targets were not marked").toBeGreaterThan(2);
  expect(new Set(targets.map((t) => t.kind)).size, "one slot must not render in two colours").toBe(1);
});

// A buffer being typed in is the normal input. A token past the end of a line corrupts every
// token after it, and the editor paints the wrong ranges rather than showing nothing.
test("a document mid-edit still yields placeable ranges", async () => {
  const changes: Record<string, string>[] = [
    { '      charged: "$: self.result.total - (self.result.discount ?? 0)"': '      charged: "$: self.result.' },
    { '      - goto: "$fulfil"': "      - goto: $" },
    { '      - case: "self.output.charged > 1000"': '      - case: "self.output.' },
  ];
  for (const change of changes) {
    const doc = edit(orders, change);
    const lines = doc.text.split("\n");
    for (const t of await lsp.semanticTokens(doc)) {
      expect(lines[t.line], `token on line ${t.line}, past the end of the document`).toBeDefined();
      expect(lines[t.line]).toContain(t.text);
    }
  }
});

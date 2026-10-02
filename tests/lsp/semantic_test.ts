import { afterAll, beforeAll, expect, test } from "vitest";
import { Lsp, edit, orders, useWorkspace } from "./helpers.ts";

// The grammar sees `"$: tick"`, not the slot holding it; the server marks the same characters in
// one slot and not in another.

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

// A token past the end of a line corrupts every token after it.
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

import { beforeAll, afterAll, expect, test } from "vitest";
import { at, edit, Lsp, orders, useWorkspace } from "./helpers.ts";

// What the server says about the thing under the cursor. `<^text>` puts the cursor inside
// `text`, which stays — this is reading, not writing.
//
// A hover is ONE line: the type of what you are pointing at. The scope a slot carries is a
// different question, and `genctl schema context` is where it is asked.

let lsp: Lsp;
beforeAll(async () => {
  useWorkspace();
  lsp = await Lsp.start();
}, 60_000);
afterAll(async () => lsp?.stop());

// The reason to build hover: the type an author is otherwise guessing at.
test("a member types as itself, not as the expression it sits in", async () => {
  expect(
    await lsp.hover(at(`      charged: "$: self.result.<^total> - (self.result.discount ?? 0)"`)),
  ).toBe("`self.result.total` → **number**");
});

// `discount` is optional, so its own type is where the `?? 0` beside it comes from — the
// expression's `number` never shows that.
test("an optional member is nullable, which the whole expression's type hides", async () => {
  expect(
    await lsp.hover(at(`      charged: "$: self.result.total - (self.result.<^discount> ?? 0)"`)),
  ).toBe("`self.result.discount` → **number|null**");
});

// The path is truncated AT the segment hovered, so walking it shows each level's own type.
test("an intermediate segment types the path up to it", async () => {
  expect(
    await lsp.hover(at(`      charged: "$: self.<^result>.total - (self.result.discount ?? 0)"`)),
  ).toBe("`self.result` → **object{discount?, total}**");
});

// A `$ref` behind a null arm used to block resolution and read `unknown` — which is what a
// looping task's `self.previous` said, being exactly the value someone hovers to find out.
test("a nullable object still describes what it holds", async () => {
  const looping = edit(orders, {
    '      charged: "$: self.result.total - (self.result.discount ?? 0)"':
      '      charged: "$: (self.previous.charged ?? 0) + self.result.total"',
    '      - goto: "$fulfil"': '      - goto: "$price"',
  });
  expect(
    await lsp.hover(at(`      charged: "$: (self.<^previous>.charged ?? 0) + self.result.total"`, looping)),
  ).toBe("`self.previous` → **object{charged}|null**");
});

// No symbol under the cursor: the expression it sits in is the answer.
test("on an operator, the whole expression is the answer", async () => {
  expect(
    await lsp.hover(at(`      charged: "$: self.result.total <|>- (self.result.discount ?? 0)"`)),
  ).toBe("`self.result.total - (self.result.discount ?? 0)` → **number**");
});

// A `${ }` inside a longer string types as the string it renders into, so the leaf says
// nothing — but the interpolation being written has a type of its own, and a URL is where most
// expressions in a definition live.
test("an interpolation inside a url is typed on its own", async () => {
  expect(
    await lsp.hover(
      at(`      url: "https://api.example.com/price?customer=\${ input.<^customer_id> }"`),
    ),
  ).toBe("`input.customer_id` → **string**");
});

test("a slot reports its own type", async () => {
  expect(await lsp.hover(at(`    <^output>:`))).toContain("**tasks.price.output** — object{charged}");
});

// An expression that does not type is left to the DIAGNOSTIC: the editor puts it at the top of
// the same popup, and hover repeating it is what a reader sees twice.
test("an expression that does not type is left to the diagnostic", async () => {
  expect(
    await lsp.hover(at(`      charged: "$: self.result.total <|>- (self.result.discount)"`,
      edit(orders, { " ?? 0": "" }))),
  ).toBe("");
});

// The symbol inside it still types, and THAT is what the diagnostic does not say — it is how a
// reader finds out the `?? 0` was load-bearing.
test("a symbol inside a broken expression is still typed", async () => {
  expect(
    await lsp.hover(at(`      charged: "$: self.result.total - (self.result.<^discount>)"`,
      edit(orders, { " ?? 0": "" }))),
  ).toBe("`self.result.discount` → **number|null**");
});

// The scan reads raw text, so it cannot tell a member path from a word inside a string
// literal. A symbol that does not type is dropped, and the expression's answer stands.
test("a word inside a string literal reports no error of its own", async () => {
  const literal = edit(orders, {
    'X-Currency: "\${ input.currency }"': `X-Currency: "$: 'EUR'"`,
  });
  expect(await lsp.hover(at(`        X-Currency: "$: '<^EUR>'"`, literal))).toBe(
    "`'EUR'` → **string**",
  );
});

// A name the author chose means nothing to the definition language, so it is the one thing
// with no answer.
test("there is nothing to say about a name the author chose", async () => {
  expect(await lsp.hover(at(`    <^customer_id>: { type: string }`))).toBe("");
});

// ── keys ─────────────────────────────────────────────────────────────────────────

// Hover answered NOTHING on most of a file once the scope line was dropped — every key, every
// literal. A key means something, and the schema already carries the prose.
test("a key says what it means", async () => {
  expect(await lsp.hover(at(`  - <^id>: price`))).toContain("Task identifier");
  expect(await lsp.hover(at(`      <^method>: get`))).toContain("HTTP method");
});

// The discriminator carries no prose of its own — `{"const": "fetch"}` says nothing. What a
// reader is pointing at is the variant it selects.
test("an action's type describes the action it selects", async () => {
  expect(await lsp.hover(at(`      type: <^fetch>`))).toContain("HTTP call");
  const child = await lsp.hover(at(`      type: <^child>`));
  expect(child).toContain("child");
  expect(child).not.toContain("HTTP call");
});

// Every line of a valid definition has something to say. A hover that is silent nine times out
// of ten reads as a hover that does not work, which is how this was reported.
test("every written line of the fixture answers", async () => {
  const lines = orders.text.split("\n");
  const silent: string[] = [];
  for (let i = 0; i < lines.length; i++) {
    const text = lines[i];
    if (text.trim() === "") continue;
    const character = text.length - text.trimStart().length + 2;
    const md = await lsp.hover({ ...orders, line: i, character, quoted: text });
    if (md === "") silent.push(`${i + 1}: ${text}`);
  }
  // What is left is exactly the author's own vocabulary — the property names in their schemas
  // and the status pattern they chose. Everything the definition language owns has an answer.
  expect(silent).toEqual([
    "6:     customer_id: { type: string }",
    "7:     amount: { type: number }",
    "8:     currency: { type: string }",
    '20:         "200":',
    "23:             total: { type: number }",
    "24:             discount: { type: number }",
    "43:           approved: { type: boolean }",
  ]);
});

// A `case` is an expression written BARE — an expression slot, not a Shape — so there is no
// `$:` to recognise it by, and hover answered with what the `case` key means instead of what
// the expression evaluates to.
test("a switch case's expression is typed, not described as a key", async () => {
  expect(await lsp.hover(at(`      - case: "self.output.<^charged> > 1000"`))).toBe(
    "`self.output.charged` → **number**",
  );
});

test("an on_error rule's case is the same kind of slot", async () => {
  // `error` is the failure THIS rule caught — the scope an on_error case is written in.
  const withCase = edit(orders, {
    "      - code: [http.500]\n": '      - code: [http.500]\n        case: "error.code == \'x\'"\n',
  });
  expect(await lsp.hover(at(`        case: "<^error>.code == 'x'"`, withCase))).toContain(
    "`error` →",
  );
});

// A `default` is filled in when the value is absent, so the property is always there — which
// is what reading it already typed as. The structured summary said `?` beside it anyway.
test("a defaulted property is not marked absent, matching the type reading it gives", async () => {
  const defaulted = edit(orders, {
    "    currency: { type: string }": '    currency: { type: string, default: "EUR" }',
    "  required: [customer_id, amount, currency]": "  required: [customer_id, amount]",
  });
  // The member list and the type of reading it must agree.
  expect(await lsp.hover(at(`      url: "https://api.example.com/price?customer=\${ <^input>.customer_id }"`, defaulted)))
    .toBe("`input` → **object{amount, currency, customer_id}**");
  expect(await lsp.hover(at(`        X-Currency: "\${ input.<^currency> }"`, defaulted)))
    .toBe("`input.currency` → **string**");
});

// Without a default it stays optional, and both halves say so.
test("an optional property with no default is marked absent and types nullable", async () => {
  const optional = edit(orders, {
    "  required: [customer_id, amount, currency]": "  required: [customer_id, amount]",
  });
  expect(await lsp.hover(at(`      url: "https://api.example.com/price?customer=\${ <^input>.customer_id }"`, optional)))
    .toBe("`input` → **object{amount, currency?, customer_id}**");
  expect(await lsp.hover(at(`        X-Currency: "\${ input.<^currency> }"`, optional)))
    .toBe("`input.currency` → **string|null**");
});

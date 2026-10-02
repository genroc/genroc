import { beforeAll, afterAll, expect, test } from "vitest";
import { at, edit, fanout, Lsp, orders, useWorkspace } from "./helpers.ts";

// A hover is ONE line, the type under the cursor; a slot's scope is `genctl schema context`'s question.

let lsp: Lsp;
beforeAll(async () => {
  useWorkspace();
  lsp = await Lsp.start();
}, 60_000);
afterAll(async () => lsp?.stop());

test("a member types as itself, not as the expression it sits in", async () => {
  expect(
    await lsp.hover(at(`      charged: "$: self.result.<^total> - (self.result.discount ?? 0)"`)),
  ).toBe("`self.result.total` → `number`");
});

test("an optional member is nullable, which the whole expression's type hides", async () => {
  expect(
    await lsp.hover(at(`      charged: "$: self.result.total - (self.result.<^discount> ?? 0)"`)),
  ).toBe("`self.result.discount` → `number|null`");
});

test("an intermediate segment types the path up to it", async () => {
  expect(
    await lsp.hover(at(`      charged: "$: self.<^result>.total - (self.result.discount ?? 0)"`)),
  ).toBe("`self.result` → `object{discount?, total}`");
});

// A `$ref` behind a null arm must still resolve, not read `unknown`.
test("a nullable object still describes what it holds", async () => {
  const looping = edit(orders, {
    '      charged: "$: self.result.total - (self.result.discount ?? 0)"':
      '      charged: "$: (self.previous.charged ?? 0) + self.result.total"',
    '      - goto: "$fulfil"': '      - goto: "$price"',
  });
  expect(
    await lsp.hover(at(`      charged: "$: (self.<^previous>.charged ?? 0) + self.result.total"`, looping)),
  ).toBe("`self.previous` → `object{charged}|null`");
});

test("on an operator, the whole expression is the answer", async () => {
  expect(
    await lsp.hover(at(`      charged: "$: self.result.total <|>- (self.result.discount ?? 0)"`)),
  ).toBe("`self.result.total - (self.result.discount ?? 0)` → `number`");
});

// The leaf types as the string it renders into; the interpolation has a type of its own.
test("an interpolation inside a url is typed on its own", async () => {
  expect(
    await lsp.hover(
      at(`      url: "https://api.example.com/price?customer=\${ input.<^customer_id> }"`),
    ),
  ).toBe("`input.customer_id` → `string`");
});

// The span must come off the source, not the FOLDED value, whose newlines differ.
test("an expression written as a block scalar types like an inline one", async () => {
  const folded = edit(orders, {
    '      charged: "$: self.result.total - (self.result.discount ?? 0)"':
      "      charged: >\n        $: self.result.total - (self.result.discount ?? 0)",
  });
  expect(
    await lsp.hover(at(`        $: self.result.<^total> - (self.result.discount ?? 0)`, folded)),
  ).toBe("`self.result.total` → `number`");
});

test("a slot reports its own type", async () => {
  expect(await lsp.hover(at(`    <^output>:`))).toContain("**tasks.price.output** — `object{charged}`");
});

// The editor already shows the diagnostic at the top of the same popup.
test("an expression that does not type is left to the diagnostic", async () => {
  expect(
    await lsp.hover(at(`      charged: "$: self.result.total <|>- (self.result.discount)"`,
      edit(orders, { " ?? 0": "" }))),
  ).toBe("");
});

test("a symbol inside a broken expression is still typed", async () => {
  expect(
    await lsp.hover(at(`      charged: "$: self.result.total - (self.result.<^discount>)"`,
      edit(orders, { " ?? 0": "" }))),
  ).toBe("`self.result.discount` → `number|null`");
});

// The scan reads raw text, so it cannot tell a member path from a word inside a string
// literal. A symbol that does not type is dropped, and the expression's answer stands.
test("a word inside a string literal reports no error of its own", async () => {
  const literal = edit(orders, {
    'X-Currency: "\${ input.currency }"': `X-Currency: "$: 'EUR'"`,
  });
  expect(await lsp.hover(at(`        X-Currency: "$: '<^EUR>'"`, literal))).toBe(
    "`'EUR'` → `string`",
  );
});

test("there is nothing to say about a name the author chose", async () => {
  expect(await lsp.hover(at(`    <^customer_id>: { type: string }`))).toBe("");
});

// ── keys ─────────────────────────────────────────────────────────────────────────

test("a key says what it means", async () => {
  expect(await lsp.hover(at(`  - <^id>: price`))).toContain("Task identifier");
  expect(await lsp.hover(at(`      <^method>: get`))).toContain("HTTP method");
});

// The discriminator's own schema (`{"const": "fetch"}`) has no prose; the variant it selects does.
test("an action's type describes the action it selects", async () => {
  expect(await lsp.hover(at(`      type: <^fetch>`))).toContain("HTTP call");
  const child = await lsp.hover(at(`      type: <^child>`));
  expect(child).toContain("child");
  expect(child).not.toContain("HTTP call");
});

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
  // What is left is the author's own vocabulary: their property names and status pattern.
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

// A `case` is written BARE: no `$:` to recognise it by, only the slot.
test("a switch case's expression is typed, not described as a key", async () => {
  expect(await lsp.hover(at(`      - case: "self.output.<^charged> > 1000"`))).toBe(
    "`self.output.charged` → `number`",
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

// A `default` means the property is always there, which is how reading it already types.
test("a defaulted property is not marked absent, matching the type reading it gives", async () => {
  const defaulted = edit(orders, {
    "    currency: { type: string }": '    currency: { type: string, default: "EUR" }',
    "  required: [customer_id, amount, currency]": "  required: [customer_id, amount]",
  });
  // The member list and the type of reading it must agree.
  expect(await lsp.hover(at(`      url: "https://api.example.com/price?customer=\${ <^input>.customer_id }"`, defaulted)))
    .toBe("`input` → `object{amount, currency, customer_id}`");
  expect(await lsp.hover(at(`        X-Currency: "\${ input.<^currency> }"`, defaulted)))
    .toBe("`input.currency` → `string`");
});

// Without a default it stays optional, and both halves say so.
test("an optional property with no default is marked absent and types nullable", async () => {
  const optional = edit(orders, {
    "  required: [customer_id, amount, currency]": "  required: [customer_id, amount]",
  });
  expect(await lsp.hover(at(`      url: "https://api.example.com/price?customer=\${ <^input>.customer_id }"`, optional)))
    .toBe("`input` → `object{amount, currency?, customer_id}`");
  expect(await lsp.hover(at(`        X-Currency: "\${ input.<^currency> }"`, optional)))
    .toBe("`input.currency` → `string|null`");
});

// ── a name the expression binds ──────────────────────────────────────────────────

// The expression binds it, so no slot scope has it.
test("a lambda parameter types as the element it is bound to", async () => {
  expect(
    await lsp.hover(at(`      over: "$: map(input.lines, (<^line>) => { order: line.sku })"`, fanout)),
  ).toBe("`line` → `object{qty, sku}`");
});

// It is a path root like any other once bound, so the segments after it must walk.
test("a member of a lambda parameter types through it", async () => {
  expect(
    await lsp.hover(at(`      over: "$: map(input.lines, (line) => { order: line.<^sku> })"`, fanout)),
  ).toBe("`line.sku` → `string`");
});

// The map's SOURCE is in the slot's scope, not the lambda's.
test("the map's source is unaffected by what the lambda binds", async () => {
  expect(
    await lsp.hover(at(`      over: "$: map(input.<^lines>, (line) => { order: line.sku })"`, fanout)),
  ).toBe("`input.lines` → `array<object{qty, sku}>`");
});

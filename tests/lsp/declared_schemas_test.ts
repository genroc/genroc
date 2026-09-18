import { beforeAll, afterAll, expect, test } from "vitest";
import { at, Doc, Lsp, orders, useWorkspace } from "./helpers.ts";

// Declared slot schemas, checked with NO SERVER. specs/declared-slot-schemas.md.
//
// That is the claim this file exists for rather than a convenience: the child input check has
// never been runnable here, because it reads the child definition out of the database. A
// declaration is in the document, so an editor can check the call against it.

let lsp: Lsp;
beforeAll(async () => {
  useWorkspace();
  lsp = await Lsp.start();
}, 60_000);
afterAll(async () => lsp?.stop());

const dir = () => orders.uri.slice("file://".length).replace(/\/[^/]+$/, "");
let n = 0;
function doc(lines: string[]): Doc {
  const name = `declared_${n++}`;
  return {
    uri: `file://${dir()}/${name}.genroc.yaml`,
    text: [`name: ${name}`, ...lines].join("\n") + "\n",
  };
}

const FETCH = (body: string, schema: string[]) =>
  doc([
    "input_schema:",
    "  type: object",
    "  properties: { n: { type: number } }",
    "tasks:",
    "  - id: call",
    "    action:",
    "      type: fetch",
    "      url: http://example.invalid/x",
    "      method: post",
    `      body: ${body}`,
    ...schema,
    "    switch: [{ goto: end }]",
    "output: { ok: true }",
  ]);

const AMOUNT = [
  "      body_schema:",
  "        type: object",
  "        properties: { amount: { type: number } }",
];

test("a misspelled body key is named", async () => {
  const ds = await lsp.diagnostics(FETCH("{ amont: '$: input.n' }", AMOUNT));
  expect(ds).toHaveLength(1);
  // The KEY, not a type mismatch — a reader who mistyped needs to be told that.
  expect(ds[0]).toContain("amont");
  expect(ds[0]).toContain("not declared");
});

test("the same body passes once the key is spelled right", async () => {
  expect(await lsp.diagnostics(FETCH("{ amount: '$: input.n' }", AMOUNT))).toEqual([]);
});

test("a declared property the body never sets is fine while it is optional", async () => {
  const ds = await lsp.diagnostics(
    FETCH("{ amount: '$: input.n' }", [
      "      body_schema:",
      "        type: object",
      "        properties: { amount: { type: number }, note: { type: string } }",
    ]),
  );
  expect(ds).toEqual([]);
});

test("a declared property the body never sets is refused once it is required", async () => {
  const ds = await lsp.diagnostics(
    FETCH("{ amount: '$: input.n' }", [
      "      body_schema:",
      "        type: object",
      "        properties: { amount: { type: number }, note: { type: string } }",
      "        required: [amount, note]",
    ]),
  );
  expect(ds).toHaveLength(1);
  expect(ds[0]).toContain("note");
});

// The one that needs no child definition anywhere, which is the whole point.
test("a child input_schema catches a misspelled key with the child nowhere in sight", async () => {
  const ds = await lsp.diagnostics(
    doc([
      "input_schema:",
      "  type: object",
      "  properties: { n: { type: number } }",
      "tasks:",
      "  - id: call",
      "    action:",
      "      type: child",
      "      name: no_such_process",
      "      input: { cont: '$: input.n' }",
      "      input_schema:",
      "        type: object",
      "        properties: { count: { type: number } }",
      "    switch: [{ goto: end }]",
      "output: { ok: true }",
    ]),
  );
  expect(ds).toHaveLength(1);
  expect(ds[0]).toContain("cont");
});

test("a child_map entry declares its own input, and is checked per entry", async () => {
  const ds = await lsp.diagnostics(
    doc([
      "tasks:",
      "  - id: call",
      "    action:",
      "      type: child_map",
      "      children:",
      "        a:",
      "          name: no_such_process",
      "          input: { wrng: 1 }",
      "          input_schema:",
      "            type: object",
      "            properties: { right: { type: number } }",
      "    switch: [{ goto: end }]",
      "output: { ok: true }",
    ]),
  );
  expect(ds).toHaveLength(1);
  expect(ds[0]).toContain("wrng");
});

test("a process output_schema is refused when a required key is never set", async () => {
  const ds = await lsp.diagnostics(
    doc([
      "tasks:",
      "  - id: only",
      "    switch: [{ goto: end }]",
      "    output: { a: 1 }",
      "output: { a: '$: outputs.only.a' }",
      "output_schema:",
      "  type: object",
      "  properties: { a: { type: number }, b: { type: string } }",
      "  required: [a, b]",
    ]),
  );
  expect(ds).toHaveLength(1);
  expect(ds[0]).toContain("b");
});

test("a task output_schema is checked the same way", async () => {
  const ds = await lsp.diagnostics(
    doc([
      "tasks:",
      "  - id: only",
      "    switch: [{ goto: end }]",
      "    output: { a: 1, extra: 2 }",
      "    output_schema:",
      "      type: object",
      "      properties: { a: { type: number } }",
      "output: { ok: true }",
    ]),
  );
  expect(ds).toHaveLength(1);
  expect(ds[0]).toContain("extra");
});

test("additionalProperties in a declared schema is refused by name", async () => {
  const ds = await lsp.diagnostics(
    doc([
      "tasks:",
      "  - id: only",
      "    switch: [{ goto: end }]",
      "    output: { a: 1 }",
      "output: { a: '$: outputs.only.a' }",
      "output_schema:",
      "  type: object",
      "  properties: { a: { type: number } }",
      "  additionalProperties: { type: string }",
    ]),
  );
  expect(ds.join("\n")).toContain("additionalProperties");
});

test("body_schema is refused on an action that makes no request", async () => {
  const ds = await lsp.diagnostics(
    doc([
      "tasks:",
      "  - id: wait",
      "    action: { type: delay, for: 1s, body_schema: { type: object } }",
      "    switch: [{ goto: end }]",
      "output: { ok: true }",
    ]),
  );
  expect(ds.join("\n")).toContain("body_schema");
});

test("input_schema on a fetch names body_schema rather than being ignored", async () => {
  const ds = await lsp.diagnostics(
    doc([
      "tasks:",
      "  - id: call",
      "    action:",
      "      type: fetch",
      "      url: http://example.invalid/x",
      "      method: post",
      "      input_schema: { type: object }",
      "    switch: [{ goto: end }]",
      "output: { ok: true }",
    ]),
  );
  expect(ds.join("\n")).toContain("body_schema");
});

// A null in an optional non-nullable slot is the case the runtime REPAIRS, so it must not be
// refused here. Without this the feature is unreachable: the author would have to write around
// exactly the nullability the repair exists to absorb.
test("an optional non-nullable property fed a nullable expression is accepted", async () => {
  const ds = await lsp.diagnostics(
    doc([
      "input_schema:",
      "  type: object",
      "  properties: { n: { type: [number, 'null'] } }",
      "tasks:",
      "  - id: only",
      "    switch: [{ goto: end }]",
      "    output: { discount: '$: input.n' }",
      "    output_schema:",
      "      type: object",
      "      properties: { discount: { type: number } }",
      "output: { ok: true }",
    ]),
  );
  expect(ds).toEqual([]);
});

// The same nullability where absence is NOT valid has no repair, so it must still be refused.
test("a REQUIRED non-nullable property fed a nullable expression is refused", async () => {
  const ds = await lsp.diagnostics(
    doc([
      "input_schema:",
      "  type: object",
      "  properties: { n: { type: [number, 'null'] } }",
      "tasks:",
      "  - id: only",
      "    switch: [{ goto: end }]",
      "    output: { discount: '$: input.n' }",
      "    output_schema:",
      "      type: object",
      "      properties: { discount: { type: number } }",
      "      required: [discount]",
      "output: { ok: true }",
    ]),
  );
  expect(ds).toHaveLength(1);
  expect(ds[0]).toContain("discount");
});

// A declared schema's VALUE is the author's own JSON Schema, so the editor must treat it the
// way it treats `result_schema`. Both halves of that are silent when broken, which is why they
// each get a test rather than being assumed from the feature working.

test("completion inside a declared schema offers JSON Schema keywords", async () => {
  const d = doc([
    "tasks:",
    "  - id: call",
    "    action:",
    "      type: fetch",
    "      url: http://example.invalid/x",
    "      method: post",
    "      body: { a: 1 }",
    "      body_schema:",
    "        type: object",
    "        ",
    "    switch: [{ goto: end }]",
    "output: { ok: true }",
  ]);
  const labels = await lsp.completions(at("        <|>\n    switch:", d));
  // Without pointAtUserSchema the template's permissive object offers nothing at all here.
  expect(labels).toContain("properties");
  expect(labels).toContain("required");
});

test("a `default` inside a declared schema is not painted as an expression", async () => {
  const d = doc([
    "input_schema:",
    "  type: object",
    "  properties: { s: { type: string } }",
    "  required: [s]",
    "tasks:",
    "  - id: call",
    "    action:",
    "      type: fetch",
    "      url: http://example.invalid/x",
    "      method: post",
    // A real expression beside it, so the action phase is live and the contrast is the slot
    // rather than the document having nothing to paint.
    "      body: { a: '$: input.s' }",
    "      body_schema:",
    "        type: object",
    "        properties: { a: { type: string, default: '$: input.s' } }",
    "    switch: [{ goto: end }]",
    "output: { ok: true }",
  ]);
  const toks = await lsp.semanticTokens(d);
  const schemaLine = d.text.split("\n").findIndex((l) => l.includes("default:"));
  expect(schemaLine, "the fixture changed").toBeGreaterThan(0);
  expect(
    toks.filter((t) => t.line === schemaLine),
    "a default is data about a type; a $: written there is text",
  ).toEqual([]);
  // Without this the assertion above passes on a document where nothing was marked at all.
  expect(toks.some((t) => t.text === "$:"), "no marker was found anywhere").toBe(true);
});

// §6: a declared schema is an author's type in a KEY position, which is the one thing key
// completion has never had. Without it these mappings are open maps of the author's own names
// and the editor offers nothing at all inside them.

test("typing inside a declared body offers the fields the schema names", async () => {
  const d = doc([
    "tasks:",
    "  - id: call",
    "    action:",
    "      type: fetch",
    "      url: http://example.invalid/x",
    "      method: post",
    "      body:",
    "        ",
    "      body_schema:",
    "        type: object",
    "        properties:",
    "          amount: { type: number, description: minor units }",
    "          currency: { type: string }",
    "        required: [amount]",
    "    switch: [{ goto: end }]",
    "output: { ok: true }",
  ]);
  const keys = await lsp.completionDetails(at("        <|>\n      body_schema:", d));
  expect(Object.keys(keys).sort()).toEqual(["amount", "currency"]);
  // The prose an imported schema carries is the reason to import one.
  expect(keys["amount"].documentation).toContain("minor units");
  expect(keys["amount"].detail).toContain("required");
});

test("a key already written is not offered a second time", async () => {
  const d = doc([
    "tasks:",
    "  - id: call",
    "    action:",
    "      type: fetch",
    "      url: http://example.invalid/x",
    "      method: post",
    "      body:",
    "        amount: 1",
    "        ",
    "      body_schema:",
    "        type: object",
    "        properties: { amount: { type: number }, currency: { type: string } }",
    "    switch: [{ goto: end }]",
    "output: { ok: true }",
  ]);
  const labels = await lsp.completions(at("        <|>\n      body_schema:", d));
  expect(labels).toEqual(["currency"]);
});

test("a nested object inside a declared body offers its own fields", async () => {
  const d = doc([
    "tasks:",
    "  - id: call",
    "    action:",
    "      type: fetch",
    "      url: http://example.invalid/x",
    "      method: post",
    "      body:",
    "        who:",
    "          ",
    "      body_schema:",
    "        type: object",
    "        properties:",
    "          who: { type: object, properties: { name: { type: string } } }",
    "    switch: [{ goto: end }]",
    "output: { ok: true }",
  ]);
  const labels = await lsp.completions(at("          <|>\n      body_schema:", d));
  expect(labels).toEqual(["name"]);
});

test("a child input offers the declared keys, with no child definition anywhere", async () => {
  const d = doc([
    "tasks:",
    "  - id: call",
    "    action:",
    "      type: child",
    "      name: no_such_process",
    "      input:",
    "        ",
    "      input_schema:",
    "        type: object",
    "        properties: { count: { type: number } }",
    "    switch: [{ goto: end }]",
    "output: { ok: true }",
  ]);
  const labels = await lsp.completions(at("        <|>\n      input_schema:", d));
  expect(labels).toEqual(["count"]);
});

test("a slot with no declaration still offers the definition language's own keys", async () => {
  const d = doc([
    "tasks:",
    "  - id: call",
    "    action:",
    "      type: fetch",
    "      url: http://example.invalid/x",
    "      method: post",
    "      body: { a: 1 }",
    "    ",
    "    switch: [{ goto: end }]",
    "output: { ok: true }",
  ]);
  // The control: the second source must not shadow the first where nothing is declared.
  const labels = await lsp.completions(at("    <|>\n    switch:", d));
  expect(labels).toContain("on_error");
  expect(labels).toContain("output_schema");
});

// child_list has no `input` slot: each ELEMENT of `over` is one child's input, so that is what
// its declaration types — the same rule `result_schema` follows there. Checked against an
// absent `input` instead, the declaration compares against an empty object and accepts anything.

const OVER = (items: string, schema: string[]) =>
  doc([
    "input_schema:",
    "  type: object",
    "  properties:",
    `    rows: { type: array, items: ${items} }`,
    "  required: [rows]",
    "tasks:",
    "  - id: fan",
    "    action:",
    "      type: child_list",
    "      name: no_such_process",
    "      over: '$: input.rows'",
    ...schema,
    "    switch: [{ goto: end }]",
    "output: { ok: true }",
  ]);

const RIGHT = [
  "      input_schema:",
  "        type: object",
  "        properties: { right: { type: number } }",
];

test("child_list: an element key the declaration does not name is refused", async () => {
  const ds = await lsp.diagnostics(
    OVER("{ type: object, properties: { wrng: { type: number } } }", RIGHT),
  );
  expect(ds).toHaveLength(1);
  expect(ds[0]).toContain("wrng");
});

test("child_list: a matching element type passes", async () => {
  const ds = await lsp.diagnostics(
    OVER("{ type: object, properties: { right: { type: number } } }", RIGHT),
  );
  expect(ds).toEqual([]);
});

test("child_list: an element missing a required key is refused", async () => {
  const ds = await lsp.diagnostics(
    OVER("{ type: object, properties: { right: { type: number } } }", [
      "      input_schema:",
      "        type: object",
      "        properties: { right: { type: number }, also: { type: string } }",
      "        required: [also]",
    ]),
  );
  expect(ds).toHaveLength(1);
  expect(ds[0]).toContain("also");
});

test("child_list: an untyped element array says so rather than accepting anything", async () => {
  const ds = await lsp.diagnostics(
    doc([
      "input_schema:",
      "  type: object",
      "  properties: { rows: { type: array } }",
      "  required: [rows]",
      "tasks:",
      "  - id: fan",
      "    action:",
      "      type: child_list",
      "      name: no_such_process",
      "      over: '$: input.rows'",
      ...RIGHT,
      "    switch: [{ goto: end }]",
      "output: { ok: true }",
    ]),
  );
  expect(ds).toHaveLength(1);
  expect(ds[0]).toContain("element type");
});

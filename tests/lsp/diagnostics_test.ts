import { beforeAll, afterAll, expect, test } from "vitest";
import { edit, Lsp, orders, useWorkspace } from "./helpers.ts";

// What the editor underlines. The fixture is valid, so every case here writes a mistake into a
// copy of it with `edit()` — which shows both halves of the mistake in the test.

let lsp: Lsp;
beforeAll(async () => {
  useWorkspace();
  lsp = await Lsp.start();
}, 60_000);
afterAll(async () => lsp?.stop());

test("a definition that registers underlines nothing", async () => {
  expect(await lsp.diagnostics(orders)).toEqual([]);
});

// The typo the published JSON Schema used to accept and the server always refused.
test("a misspelled key is named, and underlined on the key", async () => {
  expect(await lsp.diagnostics(edit(orders, { "on_error:": "on_eror:" }))).toEqual([
    `32: unknown field "on_eror"`,
  ]);
});

// `discount` is optional, so the `?? 0` is what makes the output type at all.
test("an expression that does not type is underlined where it is written", async () => {
  expect(await lsp.diagnostics(edit(orders, { " ?? 0": "" }))).toEqual([
    `27: task "price" output.charged: operator requires non-nullable operands`,
  ]);
});

// The switch two lines below reads `self.output.charged`, which the failure above left
// untyped. Reporting that too would be the server explaining its own recovery back to the
// author — one mistake is one diagnostic.
test("a slot that fails does not also report every slot that reads it", async () => {
  const ds = await lsp.diagnostics(edit(orders, { " ?? 0": "" }));
  expect(ds.every((d) => !d.includes("unknown"))).toBe(true);
});

// Inference used to stop at the first failure, so the second mistake stayed hidden until the
// first was fixed. Both are reported in one pass, each on its own line.
test("two unrelated mistakes are two diagnostics, not two round trips", async () => {
  expect(
    await lsp.diagnostics(
      edit(orders, {
        "input.customer_id }": "input.nope }",
        "self.result.total": "self.result.nope",
      }),
    ),
  ).toEqual([
    `15: task "price" url: template expression " input.nope ": field "nope" not found in schema`,
    `27: task "price" output.charged: field "nope" not found in schema`,
  ]);
});

// A definition that does not parse is still worth underlining, and the line it names is the
// line the parser stopped on rather than the top of the file.
test("a syntax error is reported on its own line", async () => {
  const ds = await lsp.diagnostics(edit(orders, { "  - id: review": "  - id review" }));
  expect(ds).toHaveLength(1);
  expect(ds[0]).toMatch(/^3[0-9]: /);
});

// A switch is a list of routing clauses, and only one of them is wrong. Underlining the whole
// slot squiggles the `goto`s below it, which are fine — reported from an editor.
test("a broken switch case underlines that case, not the clauses around it", async () => {
  const ds = await lsp.diagnostics(
    edit(orders, { '- case: "self.output.charged > 1000"': '- case: "self.output.nope > 1000"' }),
  );
  expect(ds).toEqual([
    `29: task "price" switch case "self.output.nope > 1000": field "nope" not found in schema`,
  ]);
});

// The rule index is already in the address; `case` is the field within it. The case is put on
// the line BELOW the rule's first key, so underlining the rule and underlining the case are
// different answers — otherwise both start on line 33 and the test proves nothing.
test("a broken on_error case underlines that rule's case", async () => {
  const ds = await lsp.diagnostics(
    edit(orders, {
      "      - code: [http.500]\n": '      - code: [http.500]\n        case: "self.nope"\n',
    }),
  );
  expect(ds).toHaveLength(1);
  expect(ds[0]).toMatch(/^34: /);
});

// A rule that reports prose carries no path, and falling back to the document root painted
// every line of the file red for one bad word — while the reader was still typing it.
test("an error with no path underlines the value it names, not the file", async () => {
  const ds = await lsp.diagnostics(
    edit(orders, { '      - goto: "$fulfil"': '      - goto: "$nope"' }),
  );
  expect(ds).toEqual([`31: task "price" switch: goto "$nope" is not a known task`]);
});

// A rule that names the task, the clause and three key names in its prose used to be placed by
// guessing which quoted word was a value somewhere — and `"tick"` won, so the error landed on
// the task's id. The rules carry their slot now.
test("a rule about one switch case underlines that case", async () => {
  // Two `goto: "$review"` lines exist; the case above disambiguates which is removed.
  const ds = await lsp.diagnostics(
    edit(orders, {
      '      - case: "self.output.charged > 1000"\n        goto: "$review"\n':
        '      - case: "self.output.charged > 1000"\n',
    }),
  );
  expect(ds).toEqual([
    `29: task "price" switch case 0: set exactly one of "goto", "raise", "panic"`,
  ]);
});

// A schema document's failures used to reach the editor as prose with no path, so they landed
// on the first line — the furthest possible place from a property nested three levels down.
test("a malformed property in input_schema underlines that property", async () => {
  const ds = await lsp.diagnostics(
    edit(orders, { "    customer_id: { type: string }": "    customer_id: { type: strng }" }),
  );
  expect(ds).toEqual([
    `6: input_schema is not a valid JSON Schema: customer_id: unsupported schema type "strng"`,
  ]);
});

test("a null sub-schema underlines the key that has no schema", async () => {
  const ds = await lsp.diagnostics(edit(orders, { "    amount: { type: number }": "    amount:" }));
  expect(ds).toEqual([`7: input_schema is not a valid JSON Schema: property "amount" is null`]);
});

// A response schema is a user schema too, and it sits under an action rather than at the root.
test("a malformed response schema underlines it, not the file", async () => {
  const ds = await lsp.diagnostics(edit(orders, { "            total: { type: number }": "            total: { type: nmber }" }));
  expect(ds).toEqual([
    `23: task "price" action.responses["200"] is not a valid JSON Schema: total: unsupported schema type "nmber"`,
  ]);
});

// A schema is decoded before anything validates it, and the decoder's own error named the
// outermost slot it was inside ("input_schema.properties") in the words of a Go type — so a
// mistake in one property underlined the whole schema, and usually the first line of the file.
test("a property written as something other than a schema underlines that property", async () => {
  const ds = await lsp.diagnostics(
    edit(orders, { "    customer_id: { type: string }": "    customer_id: string" }),
  );
  expect(ds).toEqual([`6: customer_id: a schema must be an object, not a string`]);
});

test("a keyword the subset does not have is named where it is written", async () => {
  const ds = await lsp.diagnostics(
    edit(orders, {
      "            total: { type: number }": '            total: { type: number, pattern: "^d" }',
    }),
  );
  expect(ds).toEqual([`23: total: unsupported schema keyword "pattern"`]);
});

// The one failure the schema cannot place itself: the slot IS the mistake, so there is no path
// inside the schema to report. The server finds the slot by asking which one is not an object.
test("a schema slot holding a scalar underlines that slot", async () => {
  const ds = await lsp.diagnostics(
    edit(orders, {
      "      result_schema:\n        type: object\n        properties:\n          approved: { type: boolean }\n        required: [approved]":
        "      result_schema: object",
    }),
  );
  expect(ds).toEqual([`40: a schema must be an object, not a string`]);
});

// The decoder's own prose names a Go type and a field stack that skips the list index, so this
// read as "cannot unmarshal array into Go struct field ... of type string" against `tasks:`.
test("a field given the wrong kind of value says what it takes, where it is written", async () => {
  const ds = await lsp.diagnostics(edit(orders, { "      method: GET": "      method: [GET]" }));
  expect(ds).toEqual([`16: method must be a string, not a list`]);
});

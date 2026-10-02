import { beforeAll, afterAll, expect, test } from "vitest";
import { edit, Lsp, orders, useWorkspace } from "./helpers.ts";

// The fixture is valid, so every case writes its mistake into a copy with `edit()`.

let lsp: Lsp;
beforeAll(async () => {
  useWorkspace();
  lsp = await Lsp.start();
}, 60_000);
afterAll(async () => lsp?.stop());

test("a definition that registers underlines nothing", async () => {
  expect(await lsp.diagnostics(orders)).toEqual([]);
});

test("a misspelled key is named, and underlined on the key", async () => {
  // Both tasks have an `on_error`, so the rule under it is what says which one is misspelled.
  const typo = edit(orders, {
    "on_error:\n      - code: [http.500]": "on_eror:\n      - code: [http.500]",
  });
  expect(await lsp.diagnostics(typo)).toEqual([`32: unknown field "on_eror"`]);
});

// `discount` is optional, so the `?? 0` is what makes the output type at all.
test("an expression that does not type is underlined where it is written", async () => {
  expect(await lsp.diagnostics(edit(orders, { " ?? 0": "" }))).toEqual([
    `27: task "price" output.charged: operator requires non-nullable operands`,
  ]);
});

// The switch below reads the now-untyped `self.output.charged`: one mistake, one diagnostic.
test("a slot that fails does not also report every slot that reads it", async () => {
  const ds = await lsp.diagnostics(edit(orders, { " ?? 0": "" }));
  expect(ds.every((d) => !d.includes("unknown"))).toBe(true);
});

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

test("a syntax error is reported on its own line", async () => {
  const ds = await lsp.diagnostics(edit(orders, { "  - id: review": "  - id review" }));
  expect(ds).toHaveLength(1);
  expect(ds[0]).toMatch(/^3[0-9]: /);
});

test("a broken switch case underlines that case, not the clauses around it", async () => {
  const ds = await lsp.diagnostics(
    edit(orders, { '- case: "self.output.charged > 1000"': '- case: "self.output.nope > 1000"' }),
  );
  expect(ds).toEqual([
    `29: task "price" switch case "self.output.nope > 1000": field "nope" not found in schema`,
  ]);
});

// The case sits on the line BELOW the rule's first key, or rule and case would both start on 33.
test("a broken on_error case underlines that rule's case", async () => {
  const ds = await lsp.diagnostics(
    edit(orders, {
      "      - code: [http.500]\n": '      - code: [http.500]\n        case: "self.nope"\n',
    }),
  );
  expect(ds).toHaveLength(1);
  expect(ds[0]).toMatch(/^34: /);
});

test("an error with no path underlines the value it names, not the file", async () => {
  const ds = await lsp.diagnostics(
    edit(orders, { '      - goto: "$fulfil"': '      - goto: "$nope"' }),
  );
  expect(ds).toEqual([`31: task "price" switch: goto "$nope" is not a known task`]);
});

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

// The slot IS the mistake, so the schema has no inner path to report.
test("a schema slot holding a scalar underlines that slot", async () => {
  const ds = await lsp.diagnostics(
    edit(orders, {
      "      result_schema:\n        type: object\n        properties:\n          approved: { type: boolean }\n        required: [approved]":
        "      result_schema: object",
    }),
  );
  expect(ds).toEqual([`40: a schema must be an object, not a string`]);
});

test("a field given the wrong kind of value says what it takes, where it is written", async () => {
  const ds = await lsp.diagnostics(edit(orders, { "      method: get": "      method: [get]" }));
  expect(ds).toEqual([`16: method must be a string, not a list`]);
});

// A bare `-`, which every new task passes through, decodes to a nil task.
test("an empty list entry is underlined, and does not crash the server", async () => {
  const ds = await lsp.diagnostics(edit(orders, { "  - id: review": "  -\n\n  - id: review" }));
  expect(ds).toEqual([`37: tasks[1] is empty`]);
});

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

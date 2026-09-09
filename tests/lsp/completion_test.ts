import { beforeAll, afterAll, expect, test } from "vitest";
import { at, edit, Lsp, orders, shipment, useWorkspace } from "./helpers.ts";

// What `genctl lsp` offers, at the places someone actually pauses while writing a definition.
// Each test quotes the line it is about; `<|text>` is the cursor with `text` not yet typed.

let lsp: Lsp;
beforeAll(async () => {
  useWorkspace();
  lsp = await Lsp.start();
}, 60_000);
afterAll(async () => lsp?.stop());

// ── inside an expression: the scope, which no JSON Schema can describe ────────────

test("after `input.` — the process input's own fields", async () => {
  expect(
    await lsp.completions(
      at(`      url: "https://api.example.com/price?customer=\${ input.<|customer_id> }"`),
    ),
  ).toEqual(["amount", "currency", "customer_id"]);
});

test("after `self.result.` — the shape the action declared it returns", async () => {
  expect(
    await lsp.completions(
      at(`      charged: "$: self.result.<|total> - (self.result.discount ?? 0)"`),
    ),
  ).toEqual(["discount", "total"]);
});

// The scope is not one thing: it depends on the slot. An action runs before its own result
// exists, so `self` is not there to read — which is the whole reason completion asks the
// context view rather than the schema.
test("a bare expression in an OUTPUT sees self; the same in an ACTION does not", async () => {
  const inOutput = await lsp.completions(
    at(`      charged: "$: <|self>.result.total - (self.result.discount ?? 0)"`),
  );
  expect(inOutput).toContain("self");
  expect(inOutput).toContain("input");

  const inAction = await lsp.completions(
    at(`        X-Currency: "\${ <|input>.currency }"`),
  );
  expect(inAction).not.toContain("self");
  expect(inAction).toContain("input");
});

// This is the state a buffer is in while someone types. Nothing about it parses.
test("an unclosed interpolation still answers", async () => {
  expect(
    await lsp.completions(at(`        X-Currency: "\${ input.<|currency> }"`)),
  ).toEqual(["amount", "currency", "customer_id"]);
});

// ── keys: the schema, discriminated by the action's own type ──────────────────────

// `discriminator` is an OpenAPI keyword a JSON Schema validator ignores, which is why
// yaml-language-server offers the union of every action shape here. This one reads `type`.
test("inside a `fetch` action — fetch's keys, and no other action's", async () => {
  const keys = await lsp.completions(at(`      method: <^GET>`));
  expect(keys).toContain("body");
  expect(keys).toContain("query");
  expect(keys).not.toContain("name"); // child's
  expect(keys).not.toContain("children"); // child_map's
  expect(keys).not.toContain("over"); // child_list's
});

test("inside a `child` action — child's keys, and not fetch's", async () => {
  const keys = await lsp.completions(at(`      name: <^shipment>`));
  expect(keys).toContain("version");
  expect(keys).toContain("raises");
  expect(keys).not.toContain("url");
});

test("keys already written are not offered again", async () => {
  const keys = await lsp.completions(at(`    switch: <^end>`));
  expect(keys).not.toContain("switch");
  expect(keys).not.toContain("id");
  expect(keys).toContain("on_error");
});

// ── a user-supplied JSON Schema ──────────────────────────────────────────────────

// `input_schema` reflected to an opaque object, so the editor had nothing to offer and fell
// back to whatever mapping SPANNED the line — the document root. Eliding the first key puts
// the cursor on the empty line under `input_schema:`, which is where it was reported.
test("inside input_schema — the schema keywords, not the document's", async () => {
  const keys = await lsp.completions(at(`input_schema:\n  <|type: object>`));
  // `type`, `properties` and `required` are already written, so what is left is the rest of
  // the vocabulary.
  expect(keys).toContain("type"); // elided by the marker, so offered again
  expect(keys).toContain("description");
  expect(keys).toContain("additionalProperties");
  expect(keys).not.toContain("required"); // already written
  // What it used to answer with.
  expect(keys).not.toContain("tasks");
  expect(keys).not.toContain("config_schema");
});

// The same allowlist `UnmarshalJSON` enforces, so the editor cannot offer a keyword the
// server would refuse. `allOf` is deliberately absent from both.
test("only the keywords genroc actually accepts are offered", async () => {
  const keys = await lsp.completions(at(`input_schema:\n  <|type: object>`));
  expect(keys).not.toContain("allOf");
  expect(keys).not.toContain("patternProperties");
  expect(keys).not.toContain("$schema");
});

test("a schema keyword carries what it means", async () => {
  const keys = await lsp.completionDetails(at(`input_schema:\n  <|type: object>`));
  expect(keys["additionalProperties"].documentation).toContain("open map");
});

// A completion list is a guessing game without them, and the prose is already on the struct
// tags — the schema carries it through.
test("a key completion carries the prose the struct tag already wrote", async () => {
  // `price` has everything but these two, so these two are what is left to offer.
  const keys = await lsp.completionDetails(at(`  - id: <^price>`));
  expect(Object.keys(keys).sort()).toEqual(["only_once", "timeout"]);
  expect(keys["only_once"].documentation).toContain("at-most-once");
  expect(keys["timeout"].documentation).toContain("Maximum execution time");
});

// The fixture is valid, so nothing required is ever missing from it — the marker takes a line
// back out to ask what the editor would say before it was written.
test("a key that is required says so", async () => {
  const keys = await lsp.completionDetails(at(`    <|switch: end>`, shipment));
  expect(keys["switch"].detail).toBe("required");
  expect(keys["timeout"].detail).toBe("");
});

// ── a union with no discriminator ────────────────────────────────────────────────

// A `switch` is either a scalar shorthand or a list of cases, and nothing in the schema says
// which. The INDEX in the path does: writing `switch.0` is writing a case.
test("inside a switch case — the routing clause's keys", async () => {
  const keys = await lsp.completions(at(`      - case: "self.output.charged > 1000"\n        <^goto>: "$review"`));
  expect(keys).toContain("raise");
  expect(keys).toContain("panic");
  expect(keys).not.toContain("id"); // the task's
  expect(keys).not.toContain("code"); // on_error's
});

test("inside an on_error rule — that rule's keys, which are not a switch case's", async () => {
  const keys = await lsp.completions(at(`      - code: [http.500]\n        <^retry>: { attempts: 3, delay: 2s }`));
  // `code` and `retry` are already written, so what is left is the rest of a rule's vocabulary.
  expect(keys).toContain("not_reached");
  expect(keys).toContain("case");
  expect(keys).not.toContain("id"); // the task's
});

// ── inside a user-supplied schema, at depth ──────────────────────────────────────

// A user schema nests user schemas. The published document leaves those positions permissive
// (openapi-typescript rejects a self-$ref), so the server puts the recursion back.
test("inside a nested property's schema — the schema keywords again", async () => {
  const keys = await lsp.completions(at(`    customer_id: { <^type>: string }`));
  // The cursor is ON `type`, so what is offered is what can be written BESIDE it.
  expect(keys).toContain("minLength");
  expect(keys).toContain("enum");
  expect(keys).not.toContain("tasks");
});

// `responses` values are user schemas too, written as a permissive object in the hand-written
// action template — so the server points them at the schema type as well.
test("inside a fetch response's schema — the schema keywords", async () => {
  const keys = await lsp.completions(at(`          <^type>: object\n          properties:`));
  // `required` is already written two lines down, so it is not offered again.
  expect(keys).toContain("description");
  expect(keys).toContain("additionalProperties");
  expect(keys).not.toContain("required");
});

// ── expressions written without a `$:` ───────────────────────────────────────────

// A `case` is an expression slot: the scan for `$:` finds nothing, so the cursor read as
// sitting on a key and the switch clause's own keys were offered inside the expression.
test("inside a switch case's expression — the scope, not the clause's keys", async () => {
  const members = await lsp.completions(at(`      - case: "self.<|output>.charged > 1000"`));
  expect(members).toContain("output");
  expect(members).not.toContain("raise"); // the clause's
  expect(members).not.toContain("goto");
});

test("at the start of a case — the roots of the switch scope", async () => {
  expect(await lsp.completions(at(`      - case: "<|self>.output.charged > 1000"`))).toEqual([
    "input",
    "outputs",
    "self",
  ]);
});

// ── the leaf being written must not defeat the scope ─────────────────────────────

// A half-typed expression does not type, its slot recovers as {}, and everything reading that
// slot then offers nothing — exactly where help was asked for. `self.previous` is that case:
// a looping task's previous output IS the slot being written, so the leaf under the cursor
// must come out of the document before the scope is computed.
test("writing into a looping task's output still offers its own members", async () => {
  const looping = edit(orders, {
    '      charged: "$: self.result.total - (self.result.discount ?? 0)"':
      '      charged: "$: (self.previous.charged ?? 0) + self.result.total"\n' +
      '      running: "$: self.previous."',
    '      - goto: "$fulfil"': '      - goto: "$price"',
  });
  expect(await lsp.completions(at(`      running: "$: self.previous.<|>"`, looping))).toEqual([
    "charged",
    "running",
  ]);
});

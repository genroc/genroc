import { beforeAll, afterAll, expect, test } from "vitest";
import { at, Lsp, shipment, useWorkspace } from "./helpers.ts";

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

import { beforeAll, afterAll, expect, test } from "vitest";
import { at, edit, Lsp, orders, shipment, useWorkspace } from "./helpers.ts";
import type { Doc } from "./helpers.ts";

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

// An action runs before its own result exists, so completion asks the context view, not the schema.
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

test("an unclosed interpolation still answers", async () => {
  expect(
    await lsp.completions(at(`        X-Currency: "\${ input.<|currency> }"`)),
  ).toEqual(["amount", "currency", "customer_id"]);
});

// ── keys: the schema, discriminated by the action's own type ──────────────────────

// Reads the OpenAPI `discriminator` (`type`), which a JSON Schema validator ignores.
test("inside a `fetch` action — fetch's keys, and no other action's", async () => {
  // On the KEY, like the `switch` case below: in the VALUE the reader is writing get.
  const keys = await lsp.completions(at(`      <^method>: get`));
  expect(keys).toContain("body");
  expect(keys).toContain("query");
  expect(keys).not.toContain("name"); // child's
  expect(keys).not.toContain("children"); // child_map's
  expect(keys).not.toContain("over"); // child_list's
});

test("inside a `child` action — child's keys, and not fetch's", async () => {
  const keys = await lsp.completions(at(`      <^name>: shipment`));
  expect(keys).toContain("version");
  expect(keys).toContain("raises");
  expect(keys).not.toContain("url");
});

test("keys already written are not offered again", async () => {
  // On the KEY: in the value of a `switch` the cursor asks where to route.
  const keys = await lsp.completions(at(`    <^switch>: end`));
  expect(keys).not.toContain("switch");
  expect(keys).not.toContain("id");
  expect(keys).not.toContain("on_error");
  expect(keys).toContain("output");
});

// ── a user-supplied JSON Schema ──────────────────────────────────────────────────

// Eliding the first key puts the cursor on the empty line under `input_schema:`.
test("inside input_schema — the schema keywords, not the document's", async () => {
  const keys = await lsp.completions(at(`input_schema:\n  <|type: object>`));
  expect(keys).toContain("type"); // elided by the marker, so offered again
  expect(keys).toContain("description");
  expect(keys).toContain("additionalProperties");
  expect(keys).not.toContain("required"); // already written
  // Not the document root's keys.
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

test("a key completion carries the prose the struct tag already wrote", async () => {
  // `price` carries everything else, so these two are what is left to offer.
  const keys = await lsp.completionDetails(at(`  - <^id>: price`));
  expect(Object.keys(keys).sort()).toEqual(["only_once", "output_schema"]);
  expect(keys["only_once"].documentation).toContain("At-most-once");
  expect(keys["output_schema"].documentation).toContain("conformed");
});

test("a timeout completes on the action, not on the task", async () => {
  const taskKeys = await lsp.completionDetails(at(`  - <^id>: price`));
  expect(Object.keys(taskKeys)).not.toContain("timeout");

  const actionKeys = await lsp.completionDetails(at(`      <^method>: post`, shipment));
  expect(Object.keys(actionKeys)).toContain("timeout");
  expect(actionKeys["timeout"].documentation).toContain("Budget for one attempt");
});

// The fixture is valid, so nothing required is ever missing from it — the marker takes a line
// back out to ask what the editor would say before it was written.
test("a key that is required says so", async () => {
  const keys = await lsp.completionDetails(at(`    <|switch: end>`, shipment));
  // The detail is what shows BESIDE the key: whether it is required, and what it takes.
  expect(keys["switch"].detail).toBe("required object");
  expect(keys["only_once"].detail).toBe("null|boolean");
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
  const keys = await lsp.completions(at(`      - code: [http.500]\n        <^retry>: { retries: 3, delay: 2s }`));
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

// A `case` has no `$:`: only the slot says it is an expression.
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

// A half-typed leaf recovers its slot as {}, and `self.previous` reads that very slot, so the leaf
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

// ── a value with a closed set ────────────────────────────────────────────────────

// A routing slot is the one place a VALUE has a closed set.
test("after `goto: $` — the tasks it could name", async () => {
  expect(await lsp.completions(at(`        goto: "$<|review>"\n      - goto: "$fulfil"`))).toEqual([
    "$fulfil",
    "$price",
    "$review",
    "end",
    "next",
  ]);
});

test("a scalar switch offers the same set", async () => {
  expect(await lsp.completions(at(`    switch: <^end>`))).toContain("$price");
});

// `end` and `next` are not tasks, and they are the two answers a reader forgets.
test("the routing words that are not tasks are offered too", async () => {
  const values = await lsp.completionDetails(at(`    switch: <^end>`));
  expect(values["end"].detail).toBe("terminate the instance");
  expect(values["next"].detail).toBe("advance to the next task in the list");
});

// `$` is not a word character, so without a range an editor inserts beside it: `$$tick`.
test("a task name replaces the `$` already typed", async () => {
  const typed = edit(orders, { '      - goto: "$fulfil"': "      - goto: $" });
  const [first] = await lsp.completionItems(at(`      - goto: $<|>`, typed));
  expect(first.textEdit).toBeDefined();
  // The range covers the `$`: one character back from the cursor.
  const { start, end } = first.textEdit!.range;
  expect(end.character - start.character).toBe(1);
  expect(first.textEdit!.newText).toBe(first.label);
});

// The empty value has a zero-width node, so the cursor past it resolves to the enclosing sequence.
test("an empty `goto:` still offers what it may name", async () => {
  const typed = edit(orders, { '      - goto: "$fulfil"': "      - goto: " });
  // The line above is quoted too: `- goto: ` is a prefix of `- goto: end` further down.
  expect(
    await lsp.completions(at(`        goto: "$review"\n      - goto: <|>`, typed)),
  ).toEqual([
    "$fulfil",
    "$price",
    "$review",
    "end",
    "next",
  ]);
});

// The order a schema READS, as `genctl schema` prints one, not alphabetical.
test("schema keywords are offered in the order a schema reads", async () => {
  const items = await lsp.completionItems(at(`input_schema:\n  <|type: object>`));
  const inListOrder = items
    .slice()
    .sort((a, b) => (a.sortText ?? "").localeCompare(b.sortText ?? ""))
    .map((i) => i.label);
  expect(inListOrder.slice(0, 6)).toEqual([
    "description",
    "$ref",
    "type",
    "oneOf",
    "anyOf",
    "enum",
  ]);
  expect(inListOrder.at(-1)).toBe("$defs");
});

test("a required key is offered first", async () => {
  const items = await lsp.completionItems(at(`    <|switch: end>`, shipment));
  const first = items.slice().sort((a, b) => (a.sortText ?? "").localeCompare(b.sortText ?? ""))[0];
  expect(first.label).toBe("switch");
});

// ── the two closed sets a `type` may take ────────────────────────────────────────

test("a schema's type offers the JSON types", async () => {
  // The line above disambiguates: `  type: object` is also a prefix of the response schema's.
  expect(await lsp.completions(at(`input_schema:\n  type: <^object>`))).toEqual([
    "[]",
    "boolean",
    "integer",
    "null",
    "number",
    "object",
    "string",
  ]);
});

// `array` is ACCEPTED and not offered: beside `[]` it read as a second spelling of it, and the
// two mean opposite things — one value that is a list, or one of several types.
test("the type list leaves out array, which the list form was read as", async () => {
  expect(await lsp.completions(at(`input_schema:\n  type: <^object>`))).not.toContain("array");
});

// Bare `null` is YAML's null VALUE, so `type: null` decodes as no type at all — silently. The
// item is listed by the name a reader is looking for and writes the spelling that survives.
test("null is listed by name and written quoted", async () => {
  const items = await lsp.completionItems(at(`input_schema:\n  type: <^object>`));
  expect(items.find((i) => i.label === "null")?.textEdit?.newText).toBe('"null"');
  expect(items.find((i) => i.label === "string")?.textEdit?.newText).toBe("string");
});

// The list form declares a nullable property; last, because one type is the common case.
test("a schema's type offers the list form, which writes the brackets", async () => {
  const items = await lsp.completionItems(at(`input_schema:\n  type: <^object>`));
  const list = items.find((i) => i.label === "[]");
  expect(list?.textEdit?.newText).toBe("[]");
  const ordered = items
    .slice()
    .sort((a, b) => (a.sortText ?? "").localeCompare(b.sortText ?? ""))
    .map((i) => i.label);
  expect(ordered[ordered.length - 1]).toBe("[]");
});

const JSON_TYPES = ["boolean", "integer", "null", "number", "object", "string"];

// An empty flow list has a zero-width node, so the cursor between the brackets does not resolve
// through the index at all — it is the line's key that says where it is.
test("inside the list form, the names come back and the brackets do not", async () => {
  const nullable = edit(orders, { "    currency: { type: string }": "    currency:\n      type: []" });
  expect(await lsp.completions(at(`      type: [<|>]`, nullable))).toEqual(JSON_TYPES);
});

// A cursor on an element resolves to `type.1`, one segment past the slot the closed set is read
// from — so the index has to be stepped over rather than looked up.
test("a name already written in the list still completes as a type", async () => {
  const nullable = edit(orders, {
    "    currency: { type: string }": '    currency:\n      type: [string, "null"]',
  });
  expect(await lsp.completions(at(`      type: [string, "<^null>"]`, nullable))).toEqual(JSON_TYPES);
});

// An unclosed `[` swallows every line below it, so the document does not parse.
test("a list still being written completes while it is unclosed", async () => {
  const nullable = edit(orders, {
    "    currency: { type: string }": "    currency:\n      type: [string,",
  });
  expect(await lsp.completions(at(`      type: [string,<|>`, nullable))).toEqual(JSON_TYPES);
});

// Read from the discriminated union's arms, not from a list kept here.
test("an action's type offers the action types, with what each one is", async () => {
  const items = await lsp.completionItems(at(`      type: <^fetch>`));
  const byLabel = Object.fromEntries(items.map((i) => [i.label, i.detail]));
  expect(Object.keys(byLabel).sort()).toEqual([
    "child",
    "child_list",
    "child_map",
    "delay",
    "external",
    "fetch",
  ]);
  expect(byLabel["fetch"]).toBe("HTTP call");
});

// Only a schema's `type` takes several at once: an action is one kind of thing.
test("an action's type does not offer the list form", async () => {
  expect(await lsp.completions(at(`      type: <^fetch>`))).not.toContain("[]");
});

// Schema order, not alphabetical: `fetch` is the one an author reaches for most.
test("the action types keep the order the schema declares", async () => {
  const items = await lsp.completionItems(at(`      type: <^fetch>`));
  const ordered = items
    .slice()
    .sort((a, b) => (a.sortText ?? "").localeCompare(b.sortText ?? ""))
    .map((i) => i.label);
  expect(ordered[0]).toBe("fetch");
});

// ── the codes an on_error rule can catch ─────────────────────────────────────────

// The one value set no schema describes: what a task can FAIL with is decided by its ACTION.
test("inside `code:` — what this task's fetch can fail with", async () => {
  const codes = await lsp.completions(at(`      - code: [<|http.500>]`));
  expect(codes).toContain("http.timeout");
  expect(codes).toContain("result.invalid");
  expect(codes).not.toContain("not_reached"); // the rule's own keys, which is what it answered
  expect(codes).not.toContain("raise");
});

// A status is unbounded, so no list can name the one that happens: a pattern is what an editor
// can offer for it.
test("the status families are offered as patterns, with what they mean", async () => {
  const codes = await lsp.completionDetails(at(`      - code: [<|http.500>]`));
  expect(Object.keys(codes)).toContain("http.5%");
  expect(codes["pre.%"].detail).toContain("never left");
});

// An external task fails in its own way, and none of a fetch's codes can reach it.
test("an external task's codes are not a fetch's", async () => {
  const waiting = edit(orders, {
    "        required: [approved]\n    switch:":
      "        required: [approved]\n    on_error:\n      - code: []\n        goto: end\n    switch:",
  });
  expect(await lsp.completions(at(`      - code: [<|>]`, waiting))).toEqual([
    "external.lost",
    "external.timeout",
  ]);
});

// The code for a call whose outcome is unknown exists only where `only_once` is written, and it
// is the reason that task has an on_error at all.
test("only_once.interrupted is offered only on an only_once task", async () => {
  const codes = await lsp.completions(at(`      - code: [<|http.500>]`));
  expect(codes).not.toContain("only_once.interrupted");

  const once = edit(orders, { "  - id: price": "  - id: price\n    only_once: true" });
  expect(await lsp.completions(at(`      - code: [<|http.500>]`, once))).toContain(
    "only_once.interrupted",
  );
});

// `shipment` raises `carrier_down` in its own file; reading it would make the answer depend on a
// buffer nobody is looking at.
test("a child task offers nothing the other file declares", async () => {
  const codes = await lsp.completions(at(`      - code: [<|carrier_down>]`));
  expect(codes).toEqual(["result.invalid"]); // its output can still fail this task's schema
});

// `code:` written as a block list is the same slot, and an element with nothing typed has no
// node to stand in — so the cursor on the dash resolves through the KEY that opened the list.
test("a dash under `code:` with nothing typed still offers the codes", async () => {
  const block = edit(orders, {
    "      - code: [http.500]": "      - code:\n          - http.500\n          - ",
  });
  const codes = await lsp.completions(at(`          - http.500\n          - <|>`, block));
  expect(codes).toContain("http.timeout");
  expect(codes).not.toContain("retry");
});

// `raises` is where a child task writes down what a code carries, which makes its keys the
// codes this task expects — and the only spelling of them this document holds.
test("codes declared in `raises` are offered as this task's own", async () => {
  const declared = edit(orders, {
    "      name: shipment\n      input:":
      "      name: shipment\n      raises:\n        carrier_down: null\n      input:",
  });
  const codes = await lsp.completionDetails(at(`      - code: [<|carrier_down>]`, declared));
  expect(codes["carrier_down"].detail).toBe("declared in raises");
  expect(Object.keys(codes)).not.toContain("http.timeout"); // how the CHILD fails is its own
});

// A `code` list never takes a key, so the rule's keys must not stand in for an empty answer.
test("a task with nothing to catch offers nothing, not the rule's keys", async () => {
  const delayed = edit(orders, {
    '      type: child\n      name: shipment\n      input:\n        order: "$: input.customer_id"':
      "      type: delay\n      for: 1h",
  });
  expect(await lsp.completions(at(`      - code: [<|carrier_down>]`, delayed))).toEqual([]);
});

// Dots are not word characters, so the item must replace the whole `http.` token.
test("a code replaces the prefix already typed", async () => {
  const typed = edit(orders, { "      - code: [http.500]": "      - code: [http." });
  const [first] = await lsp.completionItems(at(`      - code: [http.<|>`, typed));
  expect(first.textEdit).toBeDefined();
  const { start, end } = first.textEdit!.range;
  expect(end.character - start.character).toBe("http.".length);
});

// ── an array is indexed, not read by name ────────────────────────────────────────

// The fixture has no array, and an array is the one type with nothing to offer by name.
function withTags(expression: string): Doc {
  return edit(orders, {
    "    currency: { type: string }":
      "    currency: { type: string }\n" +
      "    tags:\n" +
      "      type: array\n" +
      "      items:\n" +
      "        type: object\n" +
      "        properties:\n" +
      "          name: { type: string }",
    '        X-Currency: "${ input.currency }"': '        X-Currency: "' + expression + '"',
  });
}

test("an array offers the index, and the item replaces the dot", async () => {
  const doc = withTags("${ input.tags. }");
  const cursor = at('        X-Currency: "${ input.tags.<|> }"', doc);
  const items = await lsp.completionItems(cursor);
  expect(items.map((i) => i.label)).toEqual(["[0]"]);
  expect(items[0].detail).toBe("object{name?}|null");
  // `input.who.[0]` is not an expression: the edit starts ON the dot and swallows it.
  expect(items[0].textEdit?.newText).toBe("[0]");
  expect(items[0].textEdit?.range.start.character).toBe(cursor.character - 1);
});

// The tail scanner must step past `[`, or the ROOT scope is offered.
test("members are found through an index", async () => {
  const doc = withTags("${ input.tags[0]. }");
  expect(await lsp.completions(at('        X-Currency: "${ input.tags[0].<|> }"', doc))).toEqual([
    "name",
  ]);
});

// ── a key is written with its colon ──────────────────────────────────────────────

test("a key writes its colon, and a block key writes only the colon", async () => {
  const doc = edit(orders, { "  - id: review\n": "  - id: review\n    on\n" });
  const cursor = at("    on<|>\n", doc);
  const items = await lsp.completionItems(cursor);
  // A value that goes beside the key gets the space; `on_error` opens a list below it.
  expect(items.find((i) => i.label === "only_once")?.textEdit?.newText).toBe("only_once: ");
  expect(items.find((i) => i.label === "on_error")?.textEdit?.newText).toBe("on_error:");
  // The half-typed word is what it replaces, not the cursor.
  expect(items.find((i) => i.label === "only_once")?.textEdit?.range.start.character).toBe(
    cursor.character - 2,
  );
});

// A key chosen over one that is already written: the line carries its own colon, and a second
// would break it. The word being replaced also extends PAST the cursor.
test("a key written over an existing one keeps the line's own colon", async () => {
  const cursor = at(`      <^method>: get`);
  const body = (await lsp.completionItems(cursor)).find((i) => i.label === "body");
  expect(body?.textEdit?.newText).toBe("body");
  expect(body?.textEdit?.range.start.character).toBe(6);
  expect(body?.textEdit?.range.end.character).toBe(12);
});

// An action variant carries a `oneOf` of ITS OWN — which of its keys go together — and reading
// that as a choice of shape made `action` look like a key you write a value beside.
test("an action opens a block, though its arms carry unions of their own", async () => {
  const doc = edit(orders, {
    [`    action:\n      type: child\n      name: shipment\n      input:\n        order: "$: input.customer_id"\n`]:
      "    ac\n",
  });
  const items = await lsp.completionItems(at("    ac<|>\n", doc));
  expect(items.find((i) => i.label === "action")?.textEdit?.newText).toBe("action:");
});

// ── a value position is not a key position ───────────────────────────────────────

// An empty value has no extent, so the cursor falls out to the enclosing mapping: the NEXT line's keys.
test("after a key's colon, nothing belonging to the next line is offered", async () => {
  const doc = edit(orders, { "      method: get": "      method: " });
  expect(await lsp.completions(at("      method: <|>", doc))).toEqual([]);
});

test("the same once the value is written — the cursor is still in it", async () => {
  expect(await lsp.completions(at("      method: get<|>"))).toEqual([]);
});

// The rule must not swallow a flow collection: `{ retries: 3, | }` takes another KEY. (That it
// offers the enclosing rule's keys rather than retry's is a separate imprecision.)
test("inside an inline map, keys are still offered", async () => {
  const items = await lsp.completionItems(at("        retry: { retries: 3,<|> delay: 2s }"));
  expect(items.length).toBeGreaterThan(0);
  expect(items.every((i) => i.kind === 10)).toBe(true);
});

// A value slot that HAS an answer still gives it: those run before the rule above.
test("a value position with a closed set still answers", async () => {
  const doc = edit(orders, { '      - goto: "$fulfil"': "      - goto: " });
  // The line above disambiguates: `      - goto: ` is a prefix of `      - goto: end` too.
  const cursor = at('        goto: "$review"\n      - goto: <|>', doc);
  expect(await lsp.completions(cursor)).toContain("end");
});

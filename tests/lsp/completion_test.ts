import { beforeAll, afterAll, expect, test } from "vitest";
import { at, edit, Lsp, orders, shipment, useWorkspace } from "./helpers.ts";
import type { Doc } from "./helpers.ts";

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
  // On the KEY: a cursor in the value of a `switch` is asking where to route, not what a task
  // may hold.
  const keys = await lsp.completions(at(`    <^switch>: end`));
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
  const keys = await lsp.completionDetails(at(`  - <^id>: price`));
  expect(Object.keys(keys).sort()).toEqual(["only_once", "timeout"]);
  expect(keys["only_once"].documentation).toContain("At-most-once");
  expect(keys["timeout"].documentation).toContain("Maximum execution time");
});

// The fixture is valid, so nothing required is ever missing from it — the marker takes a line
// back out to ask what the editor would say before it was written.
test("a key that is required says so", async () => {
  const keys = await lsp.completionDetails(at(`    <|switch: end>`, shipment));
  // The detail is what shows BESIDE the key: whether it is required, and what it takes.
  expect(keys["switch"].detail).toBe("required object");
  expect(keys["timeout"].detail).toBe("object");
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

// ── a value with a closed set ────────────────────────────────────────────────────

// A routing slot is the one place a VALUE has a closed set. Without this the cursor read as
// sitting on a key and the clause's own siblings were offered while typing `goto: $`.
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

// `$` is not a word character, so an editor given no range inserts beside what was typed:
// choosing `$tick` after `goto: $` left `$$tick`. The item replaces the token instead.
test("a task name replaces the `$` already typed", async () => {
  const typed = edit(orders, { '      - goto: "$fulfil"': "      - goto: $" });
  const [first] = await lsp.completionItems(at(`      - goto: $<|>`, typed));
  expect(first.textEdit).toBeDefined();
  // The range covers the `$`: one character back from the cursor.
  const { start, end } = first.textEdit!.range;
  expect(end.character - start.character).toBe(1);
  expect(first.textEdit!.newText).toBe(first.label);
});

// `goto:` with nothing after it is the moment help is wanted most, and it was the moment the
// server answered with the clause's own keys — the empty value has a zero-width node, so the
// cursor past it resolves to the sequence around it.
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

// Alphabetical put `$anchor` at the top of a list of JSON Schema keywords — the least useful
// thing first. They are offered in the order a schema READS, which is the order
// `genctl schema` prints one in.
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

// A key a definition cannot be registered without comes before the rest, whatever the order
// says about the others.
test("a required key is offered first", async () => {
  const items = await lsp.completionItems(at(`    <|switch: end>`, shipment));
  const first = items.slice().sort((a, b) => (a.sortText ?? "").localeCompare(b.sortText ?? ""))[0];
  expect(first.label).toBe("switch");
});

// ── the two closed sets a `type` may take ────────────────────────────────────────

// A `type:` inside a user schema takes JSON types. Without this the cursor after it read as
// sitting on a key and the schema's own keywords came back.
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

// `type` takes a LIST of names as well as one, which is how a nullable property is declared —
// so the list is offered as a value of its own, and it writes the brackets rather than its
// label. Last, because one type is the common case.
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

// The state a list is in for as long as it takes to write one: an unclosed `[` swallows every
// line below it, so the document does not parse and nothing at all was offered.
test("a list still being written completes while it is unclosed", async () => {
  const nullable = edit(orders, {
    "    currency: { type: string }": "    currency:\n      type: [string,",
  });
  expect(await lsp.completions(at(`      type: [string,<|>`, nullable))).toEqual(JSON_TYPES);
});

// A task's `action.type` is a different closed set, and the schema says which: the variants of
// the union it discriminates, read from the arms themselves rather than a list kept here.
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

// They are offered in the order the schema declares them, not alphabetically — `fetch` is the
// one an author reaches for most and it is written first.
test("the action types keep the order the schema declares", async () => {
  const items = await lsp.completionItems(at(`      type: <^fetch>`));
  const ordered = items
    .slice()
    .sort((a, b) => (a.sortText ?? "").localeCompare(b.sortText ?? ""))
    .map((i) => i.label);
  expect(ordered[0]).toBe("fetch");
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

// Reported from an editor: `input.who.` offered nothing at all, which reads as a server that
// does not work rather than as a dot that does not belong there.
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

// The tail scanner stopped at `[`, so the container came out empty and the ROOT scope was
// offered — `input`, `self` and `outputs`, in a position where none of them is legal.
test("members are found through an index", async () => {
  const doc = withTags("${ input.tags[0]. }");
  expect(await lsp.completions(at('        X-Currency: "${ input.tags[0].<|> }"', doc))).toEqual([
    "name",
  ]);
});

// ── a key is written with its colon ──────────────────────────────────────────────

// Choosing a key used to leave the reader to type the `:` themselves, which is the one thing
// that is never in doubt.
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

// Reported from an editor: a half-written `for: ` answered with the task's remaining KEYS —
// `on_error`, `only_once`, `timeout`. An empty value has no extent, so the cursor fell out to
// the mapping around it and got the answer for the NEXT line while this one was being written.
test("after a key's colon, nothing belonging to the next line is offered", async () => {
  const doc = edit(orders, { "      method: get": "      method: " });
  expect(await lsp.completions(at("      method: <|>", doc))).toEqual([]);
});

test("the same once the value is written — the cursor is still in it", async () => {
  expect(await lsp.completions(at("      method: get<|>"))).toEqual([]);
});

// The rule must not swallow a flow collection: `{ attempts: 3, | }` takes another KEY, and the
// cursor is past a colon there too. (Which keys it offers is a separate imprecision — the
// enclosing rule's rather than retry's own.)
test("inside an inline map, keys are still offered", async () => {
  const items = await lsp.completionItems(at("        retry: { attempts: 3,<|> delay: 2s }"));
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

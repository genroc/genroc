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

// ─── The battery ────────────────────────────────────────────────────────────────
//
// Every slot that takes a declaration gets the SAME cases, because the rules are the relation's
// and not any slot's: what the closed check refuses, and which of the null/absence gaps the
// conform can close and therefore the relation must admit. A slot tested by hand is a slot
// whose battery has a hole nobody can see.

/** `n` is nullable, so every shape below feeds a nullable value into whatever is declared. */
const PRELUDE = ['input_schema:', '  type: object', '  properties: { n: { type: [number, "null"] } }'];

/** One slot, as the pair of one-line flow mappings that vary: the shape, and its declaration. */
interface Slot {
  name: string;
  build: (shape: string, decl: string) => Doc;
}

const slots: Slot[] = [
  {
    name: "fetch body",
    build: (shape, decl) =>
      doc([
        ...PRELUDE,
        "tasks:",
        "  - id: call",
        `    action: { type: fetch, url: 'http://x.invalid/y', method: post, body: ${shape}, body_schema: ${decl} }`,
        "    switch: [{ goto: end }]",
        "output: { ok: true }",
      ]),
  },
  {
    name: "fetch query",
    build: (shape, decl) =>
      doc([
        ...PRELUDE,
        "tasks:",
        "  - id: call",
        `    action: { type: fetch, url: 'http://x.invalid/y', method: get, query: ${shape}, query_schema: ${decl} }`,
        "    switch: [{ goto: end }]",
        "output: { ok: true }",
      ]),
  },
  {
    name: "child input",
    build: (shape, decl) =>
      doc([
        ...PRELUDE,
        "tasks:",
        "  - id: call",
        `    action: { type: child, name: no_such_process, input: ${shape}, input_schema: ${decl} }`,
        "    switch: [{ goto: end }]",
        "output: { ok: true }",
      ]),
  },
  {
    name: "child_map entry input",
    build: (shape, decl) =>
      doc([
        ...PRELUDE,
        "tasks:",
        "  - id: call",
        `    action: { type: child_map, children: { a: { name: no_such_process, input: ${shape}, input_schema: ${decl} } } }`,
        "    switch: [{ goto: end }]",
        "output: { ok: true }",
      ]),
  },
  {
    name: "external input",
    build: (shape, decl) =>
      doc([
        ...PRELUDE,
        "tasks:",
        "  - id: wait",
        `    action: { type: external, input: ${shape}, input_schema: ${decl} }`,
        "    switch: [{ goto: end }]",
        "output: { ok: true }",
      ]),
  },
  {
    name: "task output",
    build: (shape, decl) =>
      doc([
        ...PRELUDE,
        "tasks:",
        "  - id: only",
        `    output: ${shape}`,
        `    output_schema: ${decl}`,
        "    switch: [{ goto: end }]",
        "output: { ok: true }",
      ]),
  },
  {
    name: "process output",
    build: (shape, decl) =>
      doc([
        ...PRELUDE,
        "tasks:",
        "  - id: only",
        "    switch: [{ goto: end }]",
        `output: ${shape}`,
        `output_schema: ${decl}`,
      ]),
  },
];

const NULLABLE = "{ v: '$: input.n' }";
const EMPTY = "{}";

interface Case {
  name: string;
  shape: string;
  decl: string;
  /** The word the diagnostic must carry, or undefined where the case must be accepted. */
  refuses?: string;
}

const cases: Case[] = [
  {
    // THE case the feature exists for: absence is valid, so the conform removes the key and
    // the relation must admit the gap. Refusing here makes the declaration unusable.
    name: "an optional non-nullable property fed a nullable value is accepted",
    shape: NULLABLE,
    decl: "{ type: object, properties: { v: { type: number } } }",
  },
  {
    // Its mirror: absence is NOT valid, so there is no repair and the relation must refuse.
    name: "a REQUIRED non-nullable property fed a nullable value is refused",
    shape: NULLABLE,
    decl: "{ type: object, properties: { v: { type: number } }, required: [v] }",
    refuses: "v",
  },
  {
    // Both states are valid, so nothing is reconciled and the null survives.
    name: "a nullable target takes the same value untouched",
    shape: NULLABLE,
    decl: '{ type: object, properties: { v: { type: [number, "null"] } } }',
  },
  {
    // The conform's other half: it writes the null in, so the relation admits the absence.
    name: "a required NULLABLE property the shape never sets is accepted",
    shape: EMPTY,
    decl: '{ type: object, properties: { v: { type: [number, "null"] } }, required: [v] }',
  },
  {
    name: "a required non-nullable property the shape never sets is refused",
    shape: EMPTY,
    decl: "{ type: object, properties: { v: { type: number } }, required: [v] }",
    refuses: "v",
  },
  {
    // The closed rule. A conform would DROP this key, which is why it is refused instead.
    name: "a key the declaration does not name is refused",
    shape: "{ v: 1, w: 2 }",
    decl: "{ type: object, properties: { v: { type: number } } }",
    refuses: "w",
  },
  {
    name: "a value of the wrong type is refused",
    shape: "{ v: 'text' }",
    decl: "{ type: object, properties: { v: { type: number } } }",
    refuses: "v",
  },
  {
    name: "an optional property the shape never sets is accepted",
    shape: EMPTY,
    decl: "{ type: object, properties: { v: { type: number } } }",
  },
];

for (const slot of slots) {
  for (const c of cases) {
    test(`${slot.name}: ${c.name}`, async () => {
      const ds = await lsp.diagnostics(slot.build(c.shape, c.decl));
      if (c.refuses === undefined) {
        expect(ds, "accepted a gap the conform closes, or a value that fits").toEqual([]);
        return;
      }
      expect(ds, `expected one diagnostic, got ${JSON.stringify(ds)}`).toHaveLength(1);
      expect(ds[0]).toContain(c.refuses);
    });
  }
}

// An OPEN MAP on the value side is the arm of the closed rule that is silent when missing: its
// keys are named by no schema, so the conform's strip stays reachable and the assertion behind
// the whole design is quietly false. It needs an inferred open map, which only a declared
// `additionalProperties` upstream produces — legal there, and refused in a slot declaration.
test("an open map cannot be sent where the declaration names fixed properties", async () => {
  const ds = await lsp.diagnostics(
    doc([
      "input_schema:",
      "  type: object",
      "  properties: { bag: { type: object, additionalProperties: { type: string } } }",
      "  required: [bag]",
      "tasks:",
      "  - id: call",
      "    action:",
      "      type: fetch",
      "      url: http://x.invalid/y",
      "      method: post",
      "      body: '$: input.bag'",
      "      body_schema: { type: object, properties: { a: { type: string } } }",
      "    switch: [{ goto: end }]",
      "output: { ok: true }",
    ]),
  );
  expect(ds).toHaveLength(1);
  expect(ds[0]).toContain("open map");
});

// ─── child_list ─────────────────────────────────────────────────────────────────
//
// The one slot whose "shape" is not a value: it has no `input` at all, so the declaration types
// one ELEMENT of `over` and the battery above cannot be pointed at it. Checked against the
// absent input instead, it compares an empty object and asserts nothing.

function listDoc(element: string, decl: string): Doc {
  return doc([
    "input_schema:",
    "  type: object",
    `  properties: { rows: { type: array, items: ${element} } }`,
    "  required: [rows]",
    "tasks:",
    "  - id: fan",
    `    action: { type: child_list, name: no_such_process, over: '$: input.rows', input_schema: ${decl} }`,
    "    switch: [{ goto: end }]",
    "output: { ok: true }",
  ]);
}

const listCases: { name: string; element: string; decl: string; refuses?: string }[] = [
  {
    name: "a matching element type passes",
    element: "{ type: object, properties: { right: { type: number } } }",
    decl: "{ type: object, properties: { right: { type: number } } }",
  },
  {
    name: "an element key the declaration does not name is refused",
    element: "{ type: object, properties: { wrng: { type: number } } }",
    decl: "{ type: object, properties: { right: { type: number } } }",
    refuses: "wrng",
  },
  {
    name: "an element missing a required key is refused",
    element: "{ type: object, properties: { right: { type: number } } }",
    decl: "{ type: object, properties: { right: { type: number }, also: { type: string } }, required: [also] }",
    refuses: "also",
  },
  {
    name: "an element's optional non-nullable null is accepted, as everywhere else",
    element: '{ type: object, properties: { v: { type: [number, "null"] } } }',
    decl: "{ type: object, properties: { v: { type: number } } }",
  },
  {
    name: "an element's REQUIRED non-nullable null is refused, as everywhere else",
    element: '{ type: object, properties: { v: { type: [number, "null"] } } }',
    decl: "{ type: object, properties: { v: { type: number } }, required: [v] }",
    refuses: "v",
  },
];

for (const c of listCases) {
  test(`child_list: ${c.name}`, async () => {
    const ds = await lsp.diagnostics(listDoc(c.element, c.decl));
    if (c.refuses === undefined) {
      expect(ds).toEqual([]);
      return;
    }
    expect(ds, `expected one diagnostic, got ${JSON.stringify(ds)}`).toHaveLength(1);
    expect(ds[0]).toContain(c.refuses);
  });
}

test("child_list: an untyped element array says so rather than accepting anything", async () => {
  const ds = await lsp.diagnostics(
    doc([
      "input_schema:",
      "  type: object",
      "  properties: { rows: { type: array } }",
      "  required: [rows]",
      "tasks:",
      "  - id: fan",
      "    action: { type: child_list, name: no_such_process, over: '$: input.rows', input_schema: { type: object, properties: { v: { type: number } } } }",
      "    switch: [{ goto: end }]",
      "output: { ok: true }",
    ]),
  );
  expect(ds).toHaveLength(1);
  expect(ds[0]).toContain("element type");
});

// ─── Placement, and the schema document itself ──────────────────────────────────

const placements: { name: string; lines: string[]; says: string }[] = [
  {
    name: "body_schema on an action that makes no request",
    lines: ["    action: { type: delay, for: 1s, body_schema: { type: object } }"],
    says: "body_schema",
  },
  {
    name: "query_schema on an action that makes no request",
    lines: ["    action: { type: external, query_schema: { type: object } }"],
    says: "query_schema",
  },
  {
    name: "input_schema on a fetch, which sends a body",
    lines: [
      "    action: { type: fetch, url: 'http://x.invalid/y', method: post, input_schema: { type: object } }",
    ],
    says: "body_schema",
  },
  {
    name: "input_schema on a child_map, whose entries carry their own",
    lines: [
      "    action: { type: child_map, input_schema: { type: object }, children: { a: { name: p } } }",
    ],
    says: "children",
  },
  {
    name: "input_schema on a delay, which sends nothing",
    lines: ["    action: { type: delay, for: 1s, input_schema: { type: object } }"],
    says: "input_schema",
  },
];

for (const p of placements) {
  test(`refused by name: ${p.name}`, async () => {
    const ds = await lsp.diagnostics(
      doc(["tasks:", "  - id: t", ...p.lines, "    switch: [{ goto: end }]", "output: { ok: true }"]),
    );
    expect(ds.join("\n")).toContain(p.says);
  });
}

test("additionalProperties in a declared schema is refused by name", async () => {
  const ds = await lsp.diagnostics(
    doc([
      "tasks:",
      "  - id: only",
      "    output: { a: 1 }",
      "    output_schema: { type: object, properties: { a: { type: number } }, additionalProperties: { type: string } }",
      "    switch: [{ goto: end }]",
      "output: { ok: true }",
    ]),
  );
  expect(ds.join("\n")).toContain("additionalProperties");
});

test("additionalProperties nested inside a declared schema is refused too", async () => {
  const ds = await lsp.diagnostics(
    doc([
      "tasks:",
      "  - id: only",
      "    output: { a: { b: 1 } }",
      "    output_schema: { type: object, properties: { a: { type: object, additionalProperties: { type: number } } } }",
      "    switch: [{ goto: end }]",
      "output: { ok: true }",
    ]),
  );
  expect(ds.join("\n")).toContain("additionalProperties");
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

// ─── Hover ──────────────────────────────────────────────────────────────────────
//
// A declaration adds two things a reader cannot get anywhere else: the type the far side
// actually accepts, and the prose an imported schema carried with it. Hover is where both are
// read, and a hover is ONE line.

/** A fetch whose body is declared, with a property whose declared type DIFFERS from the
 *  expression feeding it — which is the case the two answers must not be confused on. */
function hoverDoc(): Doc {
  return doc([
    "input_schema:",
    "  type: object",
    '  properties: { n: { type: [number, "null"] } }',
    "tasks:",
    "  - id: call",
    "    action:",
    "      type: fetch",
    "      url: http://x.invalid/y",
    "      method: post",
    "      body:",
    "        discount: '$: input.n'",
    "      body_schema:",
    "        type: object",
    "        properties: { discount: { type: number, description: in minor units } }",
    "    switch: [{ goto: end }]",
    "    output: { v: 1 }",
    "    output_schema: { type: object, properties: { v: { type: number } } }",
    "output: { ok: true }",
    "output_schema: { type: object, properties: { ok: { type: boolean } } }",
  ]);
}

// The divergence is the point: the expression is nullable, the declaration is not, and the
// conform is what closes the gap. Pointing at the key asks what arrives.
test("hover on a declared key gives the DECLARED type, not the expression's", async () => {
  const h = await lsp.hover(at("        <^discount>: '$: input.n'", hoverDoc()));
  expect(h).toBe("**discount?** — `number` — in minor units");
});

test("hover inside the expression still gives what it evaluates to", async () => {
  const h = await lsp.hover(at("        discount: '$: input.<^n>'", hoverDoc()));
  expect(h).toBe("`input.n` → `number|null`");
});

test("hover on a declared key with no prose is still one line of type", async () => {
  const h = await lsp.hover(at("    <^output>: { v: 1 }", hoverDoc()));
  // The task output slot itself, not a property of it — the slot's published type.
  expect(h).toContain("`object{v");
});

test("hover on the declaration's own key says what the slot is for", async () => {
  const d = hoverDoc();
  expect(await lsp.hover(at("      <^body_schema>:", d))).toContain("conformed to");
  expect(await lsp.hover(at("    <^output_schema>: { type: object, properties: { v:", d))).toContain(
    "published type of outputs",
  );
  expect(await lsp.hover(at("<^output_schema>: { type: object, properties: { ok:", d))).toContain(
    "this process publishes",
  );
});

// The user-schema repair, read from the other side: inside a declaration the vocabulary is JSON
// Schema's, and without `pointAtUserSchema` listing the slot there is no prose at all.
test("hover inside a declaration describes the JSON Schema keyword", async () => {
  const h = await lsp.hover(at("        <^type>: object", hoverDoc()));
  expect(h).toContain("JSON type");
});

// §1's asymmetry, as a hover. A slot the definition SENDS reads as what is being sent, not as
// what the far side accepts: the declaration is that side's contract and is addressable there.
// The two differ here — the declaration leaves `discount` optional, and this body always sets
// it — and the difference is the point. Publishing the declaration instead shipped for a moment
// and made a generic child's `input` read `unknown` at the address a resolver types a script's
// argument from.
test("hover on a slot the definition SENDS shows what is sent", async () => {
  const h = await lsp.hover(at("      <^body>:", hoverDoc()));
  expect(h).toBe("**tasks.call.action.body** — `object{discount}`");
});

// The other side of it: a slot the definition HANDS BACK reads as its declaration, because that
// is the contract a consumer reads and `$process` spreads.
test("hover on a slot the definition HANDS BACK shows what it publishes", async () => {
  const d = doc([
    "tasks:",
    "  - id: only",
    "    switch: [{ goto: end }]",
    "    output: { v: 1, w: 2 }",
    // `w` is declared optional; the literal sets it, so the inferred type would call it required.
    "    output_schema: { type: object, properties: { v: { type: number }, w: { type: number } }, required: [v] }",
    "output: { ok: true }",
  ]);
  expect(await lsp.hover(at("    <^output>: { v: 1, w: 2 }", d))).toBe(
    "**tasks.only.output** — `object{v, w?}`",
  );
});

test("hover on a key of a slot that declares nothing is unchanged", async () => {
  const d = doc([
    "tasks:",
    "  - id: call",
    "    action:",
    "      type: fetch",
    "      url: http://x.invalid/y",
    "      method: post",
    "      body:",
    "        loose: '$: 1 + 1'",
    "    switch: [{ goto: end }]",
    "output: { ok: true }",
  ]);
  // No declaration, so there is nothing to say about the key and the expression answers.
  expect(await lsp.hover(at("        <^loose>: '$: 1 + 1'", d))).toBe("`1 + 1` → `integer`");
});

test("hover on a declared child input key needs no child definition", async () => {
  const d = doc([
    "tasks:",
    "  - id: call",
    "    action:",
    "      type: child",
    "      name: no_such_process",
    "      input:",
    "        count: 1",
    "      input_schema:",
    "        type: object",
    "        properties: { count: { type: number, description: how many } }",
    "        required: [count]",
    "    switch: [{ goto: end }]",
    "output: { ok: true }",
  ]);
  expect(await lsp.hover(at("        <^count>: 1", d))).toBe("**count** — `number` — how many");
});

test("hover on a nested declared key names the path inside the declaration", async () => {
  const d = doc([
    "tasks:",
    "  - id: call",
    "    action:",
    "      type: external",
    "      input:",
    "        who:",
    "          name: 'x'",
    "      input_schema:",
    "        type: object",
    "        properties: { who: { type: object, properties: { name: { type: string } } } }",
    "    switch: [{ goto: end }]",
    "output: { ok: true }",
  ]);
  expect(await lsp.hover(at("          <^name>: 'x'", d))).toBe("**who.name?** — `string`");
});

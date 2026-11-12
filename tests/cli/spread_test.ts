import { mkdtempSync, writeFileSync } from "fs";
import { tmpdir } from "os";
import { join } from "path";
import { beforeAll, expect, test } from "vitest";
import { buildGenctlBinary, runCli } from "../helpers/cli.ts";
import { client, waitForInstance } from "../helpers/client.ts";
import { startedID, uid } from "../helpers/genctl.ts";

// `<<: "$<resolver>: <params>"` pre-fills the mapping it sits in. specs/source-resolution.md
// §The spread form, §`$process`.

let bin: string;
beforeAll(() => {
  bin = buildGenctlBinary();
}, 60_000);

const OFFLINE = { GENROC_SERVER: "http://127.0.0.1:1" };

const CHILD = [
  "name: spread-child",
  "input_schema:",
  "  type: object",
  "  properties: { n: { type: number } }",
  "  required: [n]",
  "tasks:",
  "  - id: only",
  "    switch:",
  "      - case: 'input.n < 0'",
  "        raise: { code: negative, message: 'n is negative' }",
  "      - goto: end",
  "    output: { doubled: '$: input.n * 2', label: 'x' }",
  "output: { doubled: '$: outputs.only.doubled', label: '$: outputs.only.label' }",
  "",
].join("\n");

function parent(body: string[]): string {
  return [
    "name: spread-parent",
    "input_schema:",
    "  type: object",
    "  properties: { n: { type: number } }",
    // Required: optional reads as nullable, and the input_schema spread from the child refuses null.
    "  required: [n]",
    "tasks:",
    "  - id: call",
    "    action:",
    "      type: child",
    ...body,
    "    switch: [{ goto: end }]",
    "output: { r: '$: outputs.call' }",
    "",
  ].join("\n");
}

/** A project holding the child, plus whatever parent the test writes. */
function project(parentBody: string[], config?: string): { dir: string; parent: string } {
  const dir = mkdtempSync(join(tmpdir(), "genroc_spread_"));
  if (config !== undefined) writeFileSync(join(dir, ".genroc"), config, "utf8");
  writeFileSync(join(dir, "child.genroc.yaml"), CHILD, "utf8");
  const p = join(dir, "parent.genroc.yaml");
  writeFileSync(p, parent(parentBody), "utf8");
  return { dir, parent: p };
}

const SPREAD = '      <<: "$process: ./child.genroc.yaml"';

test("a spread fills name, result_schema and raises from the child definition", () => {
  const { parent: path } = project([SPREAD, "      input: { n: '$: input.n' }"]);

  const r = runCli(bin, ["schema", "type", "spread-parent", "tasks.call.action.result", "--json", "-f", path], OFFLINE);
  expect(r.stderr).toBe("");
  // The child's OUTPUT is the parent's result_schema -- a rename, not a copy.
  expect(JSON.parse(r.stdout)).toMatchObject({
    type: "object",
    properties: { doubled: { type: "number" }, label: { type: "string" } },
  });

  // `raises` came across too: without it the rule below names a code the call never declared.
  const withRule = project([
    SPREAD,
    "      input: { n: '$: input.n' }",
    "    on_error: [{ code: [negative], goto: end }]",
  ]);
  const r2 = runCli(bin, ["schema", "type", "spread-parent", "tasks.call.action.result", "--json", "-f", withRule.parent], OFFLINE);
  expect(r2.stderr).toBe("");
});

test("an explicit key beats the spread, and only that key", () => {
  const { parent: path } = project([
    SPREAD,
    "      input: { n: '$: input.n' }",
    "      result_schema: { type: object, properties: { doubled: { type: number } } }",
  ]);
  const r = runCli(bin, ["schema", "type", "spread-parent", "tasks.call.action.result", "--json", "-f", path], OFFLINE);
  expect(r.stderr).toBe("");
  const got = JSON.parse(r.stdout);
  // The hand-written schema won outright -- shallow, so `label` is gone rather than merged in.
  expect(got.properties).toHaveProperty("doubled");
  expect(got.properties).not.toHaveProperty("label");
  // ...while `name`, which the mapping does not carry, still came from the spread.
  const named = runCli(bin, ["schema", "type", "spread-parent", "tasks.call", "--json", "-f", path], OFFLINE);
  expect(named.stderr).toBe("");
});

test("a spread cycle is refused by path, though a recursive CALL is ordinary", () => {
  const dir = mkdtempSync(join(tmpdir(), "genroc_spread_"));
  const self = join(dir, "self.genroc.yaml");
  writeFileSync(
    self,
    [
      "name: spread-self",
      "tasks:",
      "  - id: again",
      "    action:",
      "      type: child",
      '      <<: "$process: ./self.genroc.yaml"',
      "      input: {}",
      "    switch: [{ goto: end }]",
      "output: { done: true }",
      "",
    ].join("\n"),
    "utf8",
  );
  const r = runCli(bin, ["schema", "type", "spread-self", "output", "--json", "-f", self], OFFLINE);
  expect(r.ok).toBe(false);
  expect(r.stderr).toContain("spread cycle");
  expect(r.stderr).toContain("self.genroc.yaml");
});

test("$process takes exactly one file, and a second word is refused by name", () => {
  const { parent: path } = project(['      <<: "$process: ./child.genroc.yaml extra"', "      input: { n: '$: input.n' }"]);
  const r = runCli(bin, ["schema", "type", "spread-parent", "output", "--json", "-f", path], OFFLINE);
  expect(r.ok, "a word $process does not read must not be dropped in silence").toBe(false);
  expect(r.stderr).toContain("$process takes one file, and is given 2 arguments");
});

test("$process reads a quoted path as one word", () => {
  const { dir } = project([]);
  writeFileSync(join(dir, "my child.genroc.yaml"), CHILD, "utf8");
  const p = join(dir, "quoted.genroc.yaml");
  writeFileSync(p, parent([`      <<: "$process: './my child.genroc.yaml'"`, "      input: { n: '$: input.n' }"]), "utf8");
  const r = runCli(bin, ["schema", "type", "spread-parent", "tasks.call.action.result", "--json", "-f", p], OFFLINE);
  expect(r.stderr).toBe("");
  expect(JSON.parse(r.stdout).properties).toHaveProperty("doubled");
});

test("`<<` with a value that is not a directive stays the parse error it always was", () => {
  const { parent: path } = project(["      <<: nope", "      name: spread-child"]);
  const r = runCli(bin, ["schema", "type", "spread-parent", "output", "--json", "-f", path], OFFLINE);
  expect(r.ok).toBe(false);
  expect(r.stderr).toContain("`<<`");
});

test("a .genroc entry of the same name is matched before the built-in", () => {
  // The local entry claims the name AND the suffix, so first-match stops at it and never
  // reaches the built-in -- visible because its command is RUN, and there is no such file.
  const { parent: path } = project(
    [SPREAD, "      input: { n: '$: input.n' }"],
    "resolvers:\n  - { name: process, phase: structural, ext: [.genroc.yaml], command: [node, x.mjs] }\n",
  );
  const r = runCli(bin, ["schema", "type", "spread-parent", "output", "--json", "-f", path], OFFLINE);
  expect(r.ok).toBe(false);
  expect(r.stderr).toContain("x.mjs");
});

test("an override is per suffix: a local entry claiming another one falls through", () => {
  // The local entry does not accept this argument, so the built-in still answers it.
  const { parent: path } = project(
    [SPREAD, "      input: { n: '$: input.n' }"],
    "resolvers:\n  - { name: process, phase: structural, ext: [.genroc.json], command: [node, x.mjs] }\n",
  );
  const r = runCli(bin, ["schema", "type", "spread-parent", "tasks.call.action.result", "--json", "-f", path], OFFLINE);
  expect(r.stderr).toBe("");
  expect(JSON.parse(r.stdout).properties).toHaveProperty("label");
});

test("an escaped directive in the child stays escaped in what the spread copies", () => {
  const dir = mkdtempSync(join(tmpdir(), "genroc_spread_esc_"));
  writeFileSync(join(dir, "child.genroc.yaml"), [
    "name: esc-child",
    "input_schema:",
    "  type: object",
    '  properties: { a: { type: string, description: "$$import: ./x.ts" } }',
    "  required: [a]",
    "tasks: [{ id: only, switch: end }]",
    "",
  ].join("\n"), "utf8");
  const p = join(dir, "parent.genroc.yaml");
  writeFileSync(p, [
    "name: esc-parent",
    "tasks:",
    "  - id: call",
    "    action:",
    "      type: child",
    '      <<: "$process: ./child.genroc.yaml"',
    "      input: { a: hi }",
    "    switch: end",
    "",
  ].join("\n"), "utf8");

  const r = runCli(bin, ["generate", "-f", p], OFFLINE);
  expect(r.stderr, "the child's literal `$import:` text became a live directive in the parent").toBe("");
  expect(r.stdout).toBe("no resolvers - nothing to do\n");
});

// ── what a spread yields must register, and then run ────────────────────────────

/** A project holding exactly these files, as the `-f` arguments naming them. */
function files(contents: Record<string, string[]>): string[] {
  const dir = mkdtempSync(join(tmpdir(), "genroc_spread_reg_"));
  return Object.entries(contents).flatMap(([name, lines]) => {
    writeFileSync(join(dir, name), [...lines, ""].join("\n"), "utf8");
    return ["-f", join(dir, name)];
  });
}

test("a child's result passed on as a task output keeps its type", () => {
  const child = [
    "name: spread-untyped-child",
    "input_schema:",
    "  type: object",
    "  properties: { n: { type: integer } }",
    "  required: [n]",
    "tasks:",
    "  - { id: t, output: '$: input.n', switch: end }",
    "output: { v: '$: outputs.t' }",
  ];
  const call = (id: string, input: string, rest: string[]) => [
    `  - id: ${id}`,
    "    action:",
    "      type: child",
    ...rest,
    `      input: { n: ${input} }`,
  ];
  const a = [...call("a", "1", [SPREAD]), "    output: '$: self.result.v'", "    switch: next"];
  for (const b of [
    call("b", "'$: outputs.a'", [SPREAD]),
    // Without input_schema the same type reaches the server's parent/child check instead.
    call("b", "'$: outputs.a'", ["      name: spread-untyped-child"]),
  ]) {
    const r = runCli(bin, ["apply", "--check-only", ...files({
      "child.genroc.yaml": child,
      "parent.genroc.yaml": ["name: spread-untyped-parent", "tasks:", ...a, ...b, "    switch: end"],
    })]);
    expect(r.stderr, "outputs.a is the child's integer, pooled as an alias of the child's def").toBe("");
    expect(r.ok).toBe(true);
  }
});

// `t` does not run on the path the parent takes, so each child below outputs its `??` fallback.
const SKIP_T = [
  "input_schema: { type: object, properties: { skip: { type: boolean } }, required: [skip] }",
  "tasks:",
  "  - id: start",
  "    switch:",
  "      - { case: input.skip, goto: $tail }",
  "      - goto: next",
];
const T = (value: string, then: string) => ["  - id: t", `    output: "$: ${value}"`, `    switch: ${then}`];

/** `v` is `outputs.t ?? fallback`, joined over the process's endings or computed inside one task. */
function coalescing(value: string, fallback: string, inTask: boolean): string[] {
  const expr = `"$: outputs.t ?? ${fallback}"`;
  return inTask
    ? [...T(value, "next"), "  - id: tail", `    output: ${expr}`, "    switch: end", "output: { v: '$: outputs.tail' }"]
    : [...T(value, "end"), "  - { id: tail, switch: end }", `output: { v: ${expr} }`];
}

test("a `??` over a child's task outputs imports a type the child's own values fit, on every path", async () => {
  for (const [what, value, fallback, ran, skipped] of [
    ["an array", "[1, 2]", "[]", [1, 2], []],
    ["a string", "'x'", "'none'", "x", "none"],
    // A closed {x} also takes {x, y} by stripping `y`, so the two arms overlap without one covering the other.
    ["an object", "{x: 1, y: 's'}", "{x: 2}", { x: 1, y: "s" }, { x: 2 }],
  ] as const) {
    for (const inTask of [false, true]) {
      const where = `${what}, ${inTask ? "in a task output" : "joined over endings"}`;
      const child = uid("spread_oneof_child");
      const parent = uid("spread_oneof_parent");
      const applied = runCli(bin, ["apply", ...files({
        "child.genroc.yaml": [`name: ${child}`, ...SKIP_T, ...coalescing(value, fallback, inTask)],
        "parent.genroc.yaml": [
          `name: ${parent}`,
          "input_schema: { type: object, properties: { skip: { type: boolean } }, required: [skip] }",
          "tasks:",
          "  - id: a",
          "    action:",
          "      type: child",
          SPREAD,
          "      input: { skip: '$: input.skip' }",
          "    output: '$: self.result.v'",
          "    switch: end",
          "output: '$: outputs.a'",
        ],
      })]);
      expect(applied.stderr, where).toBe("");

      for (const [skip, want] of [[true, skipped], [false, ran]] as const) {
        const id = startedID(runCli(bin, ["run", parent, "--input", JSON.stringify({ skip })]).stdout);
        expect(await waitForInstance(id), `${where}, skip=${skip}: the child's value fit two arms of the oneOf the spread imported`)
          .toBe("completed");
        expect(JSON.parse(runCli(bin, ["detail", id, "--json"]).stdout).output, `${where}, skip=${skip}`).toEqual(want);
      }
    }
  }
});

test("a looping child's output joined with a fallback imports a type its own values fit", async () => {
  const child = uid("spread_loop_child");
  const parent = uid("spread_loop_parent");
  const applied = runCli(bin, ["apply", ...files({
    "child.genroc.yaml": [
      `name: ${child}`,
      ...SKIP_T,
      "  - id: t",
      "    output: { count: '$: (self.previous.count ?? 0) + 1', prev: '$: self.previous' }",
      "    switch:",
      "      - { case: self.output.count < 2, goto: $t }",
      "      - goto: end",
      "  - { id: tail, switch: end }",
      "output: { v: '$: outputs.t ?? {count: 0}' }",
    ],
    "parent.genroc.yaml": [
      `name: ${parent}`,
      "input_schema: { type: object, properties: { skip: { type: boolean } }, required: [skip] }",
      "tasks:",
      "  - id: a",
      "    action:",
      "      type: child",
      SPREAD,
      "      input: { skip: '$: input.skip' }",
      "    output: '$: self.result.v'",
      "    switch: end",
      "output: '$: outputs.a'",
    ],
  })]);
  expect(applied.stderr).toBe("");

  for (const [skip, want] of [[true, { count: 0 }], [false, { count: 2, prev: { count: 1, prev: null } }]] as const) {
    const id = startedID(runCli(bin, ["run", parent, "--input", JSON.stringify({ skip })]).stdout);
    expect(await waitForInstance(id), `skip=${skip}: the loop's value fit two arms of the oneOf the spread imported`)
      .toBe("completed");
    expect(JSON.parse(runCli(bin, ["detail", id, "--json"]).stdout).output, `skip=${skip}`).toEqual(want);
  }
});

test("a child whose input holds a map imports, and the map arrives whole", async () => {
  const child = uid("spread_map_child");
  const parent = uid("spread_map_parent");
  const applied = runCli(bin, ["apply", ...files({
    "child.genroc.yaml": [
      `name: ${child}`,
      "input_schema:",
      "  type: object",
      "  properties:",
      "    texts: { type: object, additionalProperties: { type: string } }",
      "  required: [texts]",
      "tasks: [{ id: t, switch: end }]",
      "output: '$: input.texts'",
    ],
    "parent.genroc.yaml": [
      `name: ${parent}`,
      "tasks:",
      "  - id: a",
      "    action:",
      "      type: child",
      SPREAD,
      "      input: { texts: { en: Pay now, cs: Zaplatit } }",
      "    output: '$: self.result'",
      "    switch: end",
      "output: '$: outputs.a'",
    ],
  })]);
  expect(applied.stderr, "the spread input_schema holds the child's map, which a declared slot takes").toBe("");

  const id = startedID(runCli(bin, ["run", parent]).stdout);
  expect(await waitForInstance(id)).toBe("completed");
  expect(JSON.parse(runCli(bin, ["detail", id, "--json"]).stdout).output).toEqual({ en: "Pay now", cs: "Zaplatit" });
});

test("a directive in an x- value resolves like any other leaf, and $$ escapes it", async () => {
  const dir = mkdtempSync(join(tmpdir(), "genroc_spread_x_"));
  writeFileSync(join(dir, "child.genroc.yaml"), CHILD, "utf8");
  const name = uid("spread_x");
  const parentFile = join(dir, "parent.genroc.yaml");
  writeFileSync(parentFile, [
    `name: ${name}`,
    'x-child: "$process: ./child.genroc.yaml"',
    'x-literal: "$$process: ./child.genroc.yaml"',
    "tasks: [{ id: t, switch: end }]",
    "",
  ].join("\n"), "utf8");
  expect(runCli(bin, ["apply", "-f", parentFile]).stderr).toBe("");

  const { data } = await client.GET("/definitions/{name}", { params: { path: { name } } });
  // eslint-disable-next-line @typescript-eslint/no-explicit-any
  const doc = data?.definition as any;
  expect(doc["x-child"], "resolution reads the document, x- values included").toMatchObject({ name: "spread-child" });
  expect(doc["x-literal"], "and the escape is how a tool keeps a directive's text").toBe("$process: ./child.genroc.yaml");
});

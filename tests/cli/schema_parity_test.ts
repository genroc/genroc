import { mkdtempSync, writeFileSync } from "fs";
import { tmpdir } from "os";
import { join } from "path";
import { beforeAll, expect, test } from "vitest";
import { buildGenctlBinary, runCli } from "../helpers/cli.ts";

// An expression can be typed by two different routes, and they must agree. `-e` types a BARE
// expression against the slot's context; the same text written as a `$:` leaf at that slot is
// typed through the template layer, inside the checker's own pass over the definition. They
// share the context and the inference primitive but not the path to it — so a divergence would
// mean `schema context` describes a definition nobody could actually write.
//
// The leaf's type is read back out of the DEFINITION's own answer: `self.output` in the task's
// switch is the output map the checker inferred, so `.v` is that leaf. specs/schema-command.md.

let bin: string;
beforeAll(() => {
  bin = buildGenctlBinary();
}, 60_000);

const OFFLINE = { GENROC_SERVER: "http://127.0.0.1:1" };

const INPUT = {
  type: "object",
  properties: {
    n: { type: "integer" },
    name: { type: "string" },
    opt: { type: ["string", "null"] },
    list: {
      type: "array",
      items: { type: "object", properties: { k: { type: "string" } }, required: ["k"] },
    },
  },
  required: ["n", "name", "list"],
};

/** One task whose output map holds the expression under test, as the `$:` leaf `v`. */
function probeFile(expr: string): string {
  const def = {
    name: "probe",
    input_schema: INPUT,
    tasks: [
      {
        id: "probe",
        action: {
          type: "fetch",
          url: "http://x",
          method: "GET",
          responses: {
            200: { type: "object", properties: { fee: { type: "number" } }, required: ["fee"] },
          },
        },
        output: { v: `$: ${expr}` },
        switch: [{ goto: "end" }],
      },
    ],
  };
  const dir = mkdtempSync(join(tmpdir(), "genroc_parity_"));
  const path = join(dir, "proc.yaml");
  writeFileSync(path, JSON.stringify(def)); // JSON is YAML
  return path;
}

/** `type` as the set it is. The definition's output map is canonicalized on its way out of the
 *  solver — sorted, because the recursive-inference fixpoint keys off byte-equality — while `-e`
 *  reports inference's own form, which keeps the declared order. It is the ONLY thing normalized
 *  here: anything else the two routes disagree about is a real disagreement. */
function typeSet(node: Record<string, unknown>): Record<string, unknown> {
  if (!Array.isArray(node.type)) return node;
  return { ...node, type: [...(node.type as string[])].sort() };
}

/** $refs followed against the document's own pool, so two self-contained answers compare by
 *  structure rather than by which pool they happen to name. */
function resolve(node: unknown, defs: Record<string, unknown>): unknown {
  if (Array.isArray(node)) return node.map((n) => resolve(n, defs));
  if (node === null || typeof node !== "object") return node;
  const obj = node as Record<string, unknown>;
  if (typeof obj.$ref === "string") {
    const name = obj.$ref.replace("#/$defs/", "");
    return resolve(defs[name], defs);
  }
  const out: Record<string, unknown> = {};
  for (const [k, v] of Object.entries(obj)) {
    if (k === "$defs") continue;
    out[k] = resolve(v, defs);
  }
  return typeSet(out);
}

function schemaAt(path: string, address: string, expr?: string) {
  const args = ["schema", "context", "probe", address, ...(expr ? ["-e", expr] : []), "--json", "-f", path];
  const r = runCli(bin, args, OFFLINE);
  expect(r.ok, `${args.join(" ")}: ${r.stdout}${r.stderr}`).toBe(true);
  const doc = JSON.parse(r.stdout);
  return resolve(doc, doc.$defs ?? {});
}

// One row per type constructor inference can produce, because a divergence is most likely
// where the two routes hand the type on differently — a nullable, a literal, a $ref.
const PARITY: { name: string; expr: string; want: unknown }[] = [
  { name: "a field of the result", expr: "self.result.fee", want: { type: "number" } },
  { name: "a field of the input", expr: "input.name", want: { type: "string" } },
  { name: "arithmetic", expr: "input.n + 1", want: { type: "integer" } },
  // The one a template could not carry: `${ }` stringifies, and a null interpolation is
  // refused outright, so only the `$:` spelling and `-e` can answer with the null.
  { name: "a nullable read", expr: "input.opt", want: { type: ["string", "null"] } },
  { name: "a default", expr: "input.opt ?? 'x'", want: { type: "string" } },
  { name: "a comparison", expr: "input.n > 1", want: { type: "boolean" } },
  {
    name: "an object literal",
    expr: "{a: input.n, b: input.name}",
    want: {
      type: "object",
      properties: { a: { type: "integer" }, b: { type: "string" } },
      required: ["a", "b"],
    },
  },
  { name: "an array literal", expr: "[input.n, 2]", want: { type: "array", items: { type: "integer" } } },
  {
    name: "a map",
    expr: "map(input.list, x => x.k)",
    want: { type: "array", items: { type: "string" } },
  },
  { name: "a whole declared root", expr: "input", want: INPUT },
  {
    name: "a whole result",
    expr: "self.result",
    want: { type: "object", properties: { fee: { type: "number" } }, required: ["fee"] },
  },
];

for (const c of PARITY) {
  test(`schema context -e types ${c.name} as the definition does`, () => {
    const path = probeFile(c.expr);

    const direct = schemaAt(path, "tasks.probe.output", c.expr);
    // The checker's own answer for the same text: the output map it inferred, as the task's
    // switch reads it back.
    const switchCtx = schemaAt(path, "tasks.probe.switch") as any;
    const inDefinition = switchCtx.properties.self.properties.output.properties.v;

    expect(direct, "the two routes disagree about the type of one expression").toEqual(inDefinition);
    // The expectation goes through the same normalizer, so the table can be written in the
    // declared order rather than the solver's.
    expect(direct, "…and the type itself is not what either route claims").toEqual(resolve(c.want, {}));
  });
}

// ── the two VIEWS have to agree too ──────────────────────────────────────────────
//
// `type` projects a finished SchemaFile; `context` builds scopes through the checker's own
// constructors. Two documents, two code paths, and one value in both — so every row below reads
// the same thing twice: once as a type, once at the path an expression would read it by. The Go
// side pins the library (TestTypeSlotsAreTheCheckersOwn); this pins the COMMAND, where a bug in
// how an answer is narrowed or its pool rewritten would show and the library test could not see
// it.

/** A definition reaching every slot both views name. */
function bothViewsFile(): string {
  const def = {
    name: "probe",
    input_schema: INPUT,
    tasks: [
      {
        id: "probe",
        action: {
          type: "fetch",
          url: "http://x",
          method: "GET",
          responses: {
            200: { type: "object", properties: { fee: { type: "number" } }, required: ["fee"] },
            429: { type: "object", properties: { wait: { type: "number" } }, required: ["wait"] },
          },
        },
        output: { v: "$: self.result.fee" },
        on_error: [{ code: ["http.429"], goto: "$handler" }],
        switch: [{ goto: "end" }],
      },
      { id: "handler", output: { why: "$: last_error.code" }, switch: [{ goto: "end" }] },
    ],
    output: { fee: "$: outputs.probe.v ?? 0" },
  };
  const dir = mkdtempSync(join(tmpdir(), "genroc_parity_"));
  const path = join(dir, "proc.yaml");
  writeFileSync(path, JSON.stringify(def));
  return path;
}

function view(kind: "type" | "context", path: string, address: string, expr?: string) {
  const args = ["schema", kind, "probe", address, ...(expr ? ["-e", expr] : []), "--json", "-f", path];
  const r = runCli(bin, args, OFFLINE);
  expect(r.ok, `${args.join(" ")}: ${r.stdout}${r.stderr}`).toBe(true);
  const doc = JSON.parse(r.stdout);
  return resolve(doc, doc.$defs ?? {});
}

const CROSS: { name: string; type: string; context: string; path: string }[] = [
  { name: "the process input", type: "input", context: "tasks.probe.action", path: "input" },
  {
    name: "what the action hands back",
    type: "tasks.probe.result",
    context: "tasks.probe.output",
    path: "self.result",
  },
  {
    name: "what the output map produces",
    type: "tasks.probe.output",
    context: "tasks.probe.switch",
    path: "self.output",
  },
  {
    name: "an output as a later reader sees it",
    type: "tasks.handler.output",
    context: "tasks.handler.switch",
    path: "self.output",
  },
  // One route reaches the handler, so the payload it carries is guaranteed and the read is not
  // nullable. Where two routes with different payloads meet, `data` is optional and the read
  // adds a null the declaration does not have — pinned on the Go side, where it arises.
  {
    name: "the routed failure's payload",
    type: "tasks.handler.last_error",
    context: "tasks.handler.action",
    path: "last_error.data",
  },
];

for (const c of CROSS) {
  test(`schema type and schema context agree on ${c.name}`, () => {
    const path = bothViewsFile();
    expect(view("type", path, c.type)).toEqual(view("context", path, c.context, c.path));
  });
}

// The other half: an expression rooted at a navigated schema is the same answer as the same
// expression rooted at the slot and walked there. Navigation and `-e` compose, in both views.
test("an expression rooted at a navigated type is rooted at the slot in the other view", () => {
  const path = bothViewsFile();

  const onTheType = view("type", path, "tasks.probe.result", "fee > 0");
  const onTheContext = view("context", path, "tasks.probe.output", "self.result.fee > 0");
  const walkedThere = view("context", path, "tasks.probe.output.self.result", "fee > 0");

  expect(onTheType).toEqual({ type: "boolean" });
  expect(onTheContext).toEqual(onTheType);
  expect(walkedThere).toEqual(onTheType);
});

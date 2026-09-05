import { mkdtempSync, writeFileSync } from "fs";
import { tmpdir } from "os";
import { join } from "path";
import { beforeAll, expect, test } from "vitest";
import { buildGenctlBinary, runCli } from "../helpers/cli.ts";

// `genctl schema type` answers what shape a slot IS, in the same address space as
// `schema context`, which answers what an expression written there may read. One slot, two
// questions. specs/schema-command.md §7.

let bin: string;
beforeAll(() => {
  bin = buildGenctlBinary();
}, 60_000);

const OFFLINE = { GENROC_SERVER: "http://127.0.0.1:1" };

const DEF = [
  "name: pricing",
  "input_schema:",
  "  type: object",
  "  properties: { amount: { type: number }, currency: { type: string } }",
  "  required: [amount]",
  "tasks:",
  "  - id: price",
  "    action:",
  "      type: fetch",
  "      url: http://x/price",
  "      method: POST",
  "      body: { amount: '$: input.amount' }",
  "      responses:",
  "        200: { type: object, properties: { fee: { type: number }, tiers: { type: array, items: { type: string } } }, required: [fee] }",
  "        429: { type: object, properties: { wait: { type: number } }, required: [wait] }",
  "    output: { fee: '$: self.result.fee' }",
  "    on_error:",
  "      - code: [http.429]",
  "        goto: $recover",
  "    switch: [{ goto: end }]",
  "  - id: recover",
  "    output: { why: '$: last_error.code' }",
  "    switch: [{ raise: { code: payment.declined, data: { reason: '$: last_error.message' } } }]",
  "output: { fee: '$: outputs.price.fee ?? 0' }",
  "",
].join("\n");

function defFile(body = DEF): string {
  const dir = mkdtempSync(join(tmpdir(), "genroc_type_"));
  const path = join(dir, "proc.yaml");
  writeFileSync(path, body);
  return path;
}

function typeAt(path: string, address?: string) {
  const args = ["schema", "type", "pricing", ...(address ? [address, "--json"] : []), "-f", path];
  return runCli(bin, args, OFFLINE);
}

test("schema type — lists the contract boundaries, one line each", () => {
  const r = typeAt(defFile());
  expect(r.ok, `${r.stdout}${r.stderr}`).toBe(true);
  const addresses = r.stdout
    .trim()
    .split("\n")
    .map((l) => l.split(/\s{2,}/)[0]);

  // Every place someone generates code from, and nothing else: an expression slot has no shape,
  // so `switch` and `on_error` are absent by construction, and what the ACTION has sits under
  // the same `action` segment the definition writes.
  expect(addresses).toEqual([
    "input",
    "output",
    'raises["payment.declined"]',
    "tasks.price.action.body",
    "tasks.price.action.result",
    "tasks.price.output",
    "tasks.recover.last_error",
    "tasks.recover.output",
  ]);
  // The summary names what is there, so an address can be picked without printing nine schemas.
  expect(r.stdout).toContain("object{amount, currency?}");
  expect(r.stdout).toContain("object{fee, tiers?}");
});

test("schema type — an address answers with a standalone document, and navigates into it", () => {
  const path = defFile();

  const result = typeAt(path, "tasks.price.action.result");
  expect(result.ok, result.stderr).toBe(true);
  expect(JSON.parse(result.stdout).properties.fee).toEqual({ type: "number" });

  // Navigation continues past the slot, into the schema — the slot is the first three
  // segments and the rest is `schema.At`.
  const nested = typeAt(path, "tasks.price.action.result.tiers[0]");
  expect(nested.ok, nested.stderr).toBe(true);
  // An element of an optional array comes back nullable: the array may not be there.
  expect(JSON.parse(nested.stdout).type.sort()).toEqual(["null", "string"]);

  // A raise code holds a dot, so the code is quoted like any other non-identifier key.
  const raised = typeAt(path, 'raises["payment.declined"].reason');
  expect(raised.ok, raised.stderr).toBe(true);
  expect(JSON.parse(raised.stdout)).toEqual({ type: "string" });
});

// The fetch decision: `result` is what self.result sees, and the statuses that are NOT accepted
// are the other half of the contract, addressed as what routed on — not a second address family.
test("schema type — result is the accepted response, last_error the routed one", () => {
  const path = defFile();

  const result = JSON.parse(typeAt(path, "tasks.price.action.result").stdout);
  expect(Object.keys(result.properties).sort()).toEqual(["fee", "tiers"]);

  const routed = JSON.parse(typeAt(path, "tasks.recover.last_error").stdout);
  expect(routed.properties, "the 429 body, at the task its rule routed to").toEqual({
    wait: { type: "number" },
  });
});

// The listing names a type in one line, and the three shapes that are easy to get wrong are the
// ones a raise can carry: nothing at all, a union, and an array.
test("schema type — the listing names null, a union and an element type", () => {
  const path = defFile(
    [
      "name: pricing",
      "input_schema:",
      "  type: object",
      "  properties: { opt: { type: [string, 'null'] } }",
      "tasks:",
      "  - id: t",
      "    switch:",
      "      - case: 'true'",
      "        raise: { code: bare, message: nothing }",
      "      - case: 'true'",
      "        raise: { code: maybe, message: a, data: '$: input.opt' }",
      "      - case: 'true'",
      "        raise: { code: many, message: b, data: '$: [1, 2]' }",
      "      - goto: end",
      "",
    ].join("\n"),
  );

  const r = typeAt(path);
  expect(r.ok, `${r.stdout}${r.stderr}`).toBe(true);
  const line = (a: string) => r.stdout.split("\n").find((l) => l.startsWith(a + " ")) ?? "";

  // A raise attaching nothing is a DECLARATION that the code carries nothing — null, not absent,
  // which is what tells a parent it can catch the code and find no data.
  expect(line("raises.bare")).toContain("null");
  expect(line("raises.maybe"), "a union prints its members").toContain("string|null");
  expect(line("raises.many"), "an array prints what it holds").toContain("array<integer>");
});

// The two views share one address space, so a miss on one is usually a question asked of the
// wrong half — and the answer says which half has it rather than only that this one does not.
// Inference declares `<id>_output` for every task because that name is what a self-referencing
// output resolves through. Where the output simply IS another definition, the placeholder is
// left over saying nothing — and a generator reading the pool emits a type alias per task.
test("both views — a definition that only names another is collapsed away", () => {
  const path = defFile(
    [
      "name: pricing",
      "tasks:",
      "  - id: price",
      "    action:",
      "      type: fetch",
      "      url: http://x/price",
      '      responses: { 200: { $ref: "#/$defs/quote" } }',
      "    output: '$: self.result'",
      "    switch: [{ goto: end }]",
      "output: '$: outputs.price'",
      "$defs:",
      "  quote: { type: object, properties: { fee: { type: number } }, required: [fee] }",
      "",
    ].join("\n"),
  );

  for (const address of ["tasks.price.output", "output"]) {
    const doc = JSON.parse(typeAt(path, address).stdout);
    expect(doc.$ref, `${address} is the definition, not a name for a name for it`).toBe(
      "#/$defs/quote",
    );
    expect(Object.keys(doc.$defs), `${address} carries no def that only forwards`).toEqual([
      "quote",
    ]);
  }

  // Both views print through one narrowing, so the pool a context reads from is the same one.
  const ctx = runCli(
    bin,
    ["schema", "context", "pricing", "output", "--json", "-f", path],
    OFFLINE,
  );
  expect(ctx.ok, ctx.stderr).toBe(true);
  const doc = JSON.parse(ctx.stdout);
  expect(doc.properties.outputs.properties.price).toEqual({ $ref: "#/$defs/quote" });
  expect(Object.keys(doc.$defs)).toEqual(["quote"]);
});

test("schema type — an address the other view answers names that view", () => {
  const path = defFile();

  for (const address of ["tasks.price.switch", "tasks.price.on_error[0]"]) {
    const r = typeAt(path, address);
    expect(r.ok, `${address} has no shape`).toBe(false);
    expect(r.stderr).toContain("`genctl schema context` has it");
  }

  // The reverse, from the other side.
  const inContext = runCli(
    bin,
    ["schema", "context", "pricing", "tasks.price.action.result", "--json", "-f", path],
    OFFLINE,
  );
  expect(inContext.ok).toBe(false);
  expect(inContext.stderr).toContain("`genctl schema type` has it");

  // …and an address neither answers just says what is there.
  const neither = typeAt(path, "tasks.price.nonsense");
  expect(neither.ok).toBe(false);
  expect(neither.stderr).toContain("which holds:");
  expect(neither.stderr).not.toContain("has it");
});

// A schema is a scope too — its properties are the roots — so `-e` works here as well, rooted
// at whatever the address selected. It is the same navigation one step further.
test("schema type -e — an expression is typed against the selected schema", () => {
  const path = defFile();

  const expr = runCli(
    bin,
    ["schema", "type", "pricing", "tasks.price.action.result", "-e", "fee > 0", "--json", "-f", path],
    OFFLINE,
  );
  expect(expr.ok, expr.stderr).toBe(true);
  expect(JSON.parse(expr.stdout)).toEqual({ type: "boolean" });

  // …and it reads the same schema navigation would: one names the field, the other computes on it.
  const navigated = typeAt(path, "tasks.price.action.result.fee");
  expect(JSON.parse(navigated.stdout)).toEqual({ type: "number" });
});

// The claim the shared space rests on: `tasks.<id>.output` names one slot, and the two answers
// are about that slot — what it produces, and what an expression in it may read.
test("schema type and schema context answer about the same slot", () => {
  const path = defFile();

  const produced = JSON.parse(typeAt(path, "tasks.price.output").stdout);
  const context = JSON.parse(
    runCli(bin, ["schema", "context", "pricing", "tasks.price.output", "--json", "-f", path], OFFLINE)
      .stdout,
  );

  // What the output map PRODUCES. An inferred output is a $ref into the pool that travels with
  // it — refs survive because a task output may reference itself (§4).
  expect(produced.$ref).toBe("#/$defs/price_output");
  expect(produced.$defs.price_output.properties.fee).toEqual({ type: "number" });
  // …and what an expression written there may READ, which is a different document entirely.
  expect(Object.keys(context.properties).sort()).toEqual(["input", "outputs", "self"]);
});

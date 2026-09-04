import { mkdtempSync, writeFileSync, existsSync } from "fs";
import { tmpdir } from "os";
import { join } from "path";
import { load } from "js-yaml";
import { beforeAll, expect, test } from "vitest";
import { buildGenctlBinary, runCli } from "../helpers/cli.ts";

// `genctl schema context` answers what an expression at a slot may read. It infers locally, so
// none of this needs a server — which is the point, since it is meant to be run while writing
// YAML. specs/schema-command.md.

let bin: string;
beforeAll(() => {
  bin = buildGenctlBinary();
}, 60_000);

// Nothing listens on port 1: every test here would fail if the command reached for a server.
const OFFLINE = { GENROC_SERVER: "http://127.0.0.1:1" };

const DEF = [
  "name: pricing",
  "input_schema:",
  "  type: object",
  "  properties: { amount: { type: number } }",
  "  required: [amount]",
  "tasks:",
  "  - id: price",
  "    action:",
  "      type: fetch",
  "      url: http://x/price",
  "      method: POST",
  "      body: { amount: '$: input.amount' }",
  "      responses:",
  "        200: { type: object, properties: { fee: { type: number } }, required: [fee] }",
  "        429: { type: object, properties: { wait: { type: number } }, required: [wait] }",
  "    output: { fee: '$: self.result.fee' }",
  "    on_error:",
  "      - code: [http.429]",
  "        retry: { attempts: 2, delay: '$: error.data.wait' }",
  "      - code: ['http.%']",
  "        goto: $explain",
  "    switch: [{ goto: end }]",
  "  - id: explain",
  "    output: { why: '$: last_error.code' }",
  "    switch: [{ goto: end }]",
  "output: { fee: '$: outputs.price.fee ?? 0' }",
  "",
].join("\n");

function defFile(body = DEF): string {
  const dir = mkdtempSync(join(tmpdir(), "genroc_schema_"));
  const path = join(dir, "proc.yaml");
  writeFileSync(path, body);
  return path;
}

/** The command's stdout as JSON — the document, with the resolved-phase note left on stderr. */
function schemaOf(path: string, address?: string) {
  // --json for the machine form: the tests assert on documents, and the YAML default has
  // its own test below.
  const args = ["schema", "context", "pricing", ...(address ? [address, "--json"] : []), "-f", path];
  const r = runCli(bin, args, OFFLINE);
  expect(r.ok, `${r.stdout}${r.stderr}`).toBe(true);
  return { doc: JSON.parse(r.stdout), stderr: r.stderr };
}

test("schema context — lists one slot per phase, and what each can read", () => {
  const r = runCli(bin, ["schema", "context", "pricing", "-f", defFile()], OFFLINE);
  expect(r.ok, `${r.stdout}${r.stderr}`).toBe(true);
  // A slot whose context has arms prints one line per arm, indented under its address.
  const addresses = r.stdout
    .trim()
    .split("\n")
    .filter((l) => !l.startsWith(" "))
    .map((l) => l.split(/\s{2,}/)[0]);

  // Four phases per task and the process output — not one row per slot, which would repeat
  // identical contexts and bury the four that differ.
  expect(addresses.sort()).toEqual([
    "output",
    "tasks.explain.action",
    "tasks.explain.output",
    "tasks.explain.switch",
    "tasks.price.action",
    "tasks.price.on_error.0",
    "tasks.price.on_error.1",
    "tasks.price.output",
    "tasks.price.switch",
  ]);

  const line = (a: string) => r.stdout.split("\n").find((l) => l.startsWith(a + " ")) ?? "";
  // `self.result` exists once the action has answered, and `self.output` only after the output
  // map has run — the two moments that separate the three task phases.
  expect(line("tasks.price.action")).not.toContain("self");
  expect(line("tasks.price.output")).toContain("self{headers, result, status}");
  expect(line("tasks.price.switch")).toContain("output");
});

// The process output is evaluated once, on whichever path the instance ended — so its context
// is one arm per ending, and each arm says what that ending holds. The `=null` is the
// correlation: the branch that did not run set nothing, and naming it is what lets an
// expression recover from it with `??`.
test("schema context — the process output has one arm per way the process can end", () => {
  const path = defFile();
  const r = runCli(bin, ["schema", "context", "pricing", "-f", path], OFFLINE);
  expect(r.ok, `${r.stdout}${r.stderr}`).toBe(true);
  const rendered = r.stdout.split("\n").filter((l) => l.startsWith("output") || l.startsWith("  "));
  expect(rendered.join("\n")).toContain('ending at task "price"');
  expect(rendered.join("\n")).toContain('ending at task "explain"');

  const doc = schemaOf(path, "output").doc;
  expect(doc.anyOf, "one arm per terminal").toHaveLength(2);
  const arms = doc.anyOf.map((a: any) => a.description);
  expect(arms.some((d: string) => d.includes("price"))).toBe(true);
  // Each arm types the output the OTHER ending produced as null rather than dropping it, which
  // is what an expression joining the two branches reads.
  const nulls = doc.anyOf.map((a: any) => Object.entries(a.properties.outputs.properties)
    .filter(([, v]: any) => v.type === "null").map(([k]) => k));
  expect(nulls.flat().sort()).toEqual(["explain", "price"]);
});

test("schema context — a rule reads `error`, a routed task reads `last_error`", () => {
  const path = defFile();

  // Inside the rule: the failure it caught, typed by the code it catches — `wait` comes from
  // the 429 body, which is what makes `retry.delay` writable.
  const rule = schemaOf(path, "tasks.price.on_error.0").doc;
  expect(Object.keys(rule.properties).sort()).toEqual(["error", "input", "outputs"]);
  expect(rule.properties.error.properties.data.properties).toEqual({ wait: { type: "number" } });

  // At the task the second rule routes to: the failure that got it there, and no `error` —
  // there is no rule being written at that task's own slots.
  const routed = schemaOf(path, "tasks.explain.output").doc;
  expect(Object.keys(routed.properties)).toContain("last_error");
  expect(Object.keys(routed.properties)).not.toContain("error");
});

test("schema context — an address is a path, and a task is an object of its phases", () => {
  const path = defFile();

  // A task holds its phases and nothing else: `url` and `timeout` are slots of the ACTION, and
  // the address space is the document, so they are not addresses. The miss names what is.
  const url = runCli(bin, ["schema", "context", "pricing", "tasks.price.url", "-f", path], OFFLINE);
  expect(url.ok).toBe(false);
  expect(url.stderr).toContain("which holds: action, on_error, output, switch");

  // The rules are keyed, not indexed (`items` types every element alike), and the key is spelled
  // three ways that must agree. The dotted one is canonical because it is the only one a shell
  // leaves alone: zsh reads `[0]` as a glob and refuses the command before genctl sees it.
  const dotted = schemaOf(path, "tasks.price.on_error.0");
  expect(schemaOf(path, "tasks.price.on_error[0]").doc).toEqual(dotted.doc);
  expect(schemaOf(path, 'tasks.price.on_error["0"]').doc).toEqual(dotted.doc);

  // A number is a KEY, so it reads an object and not an array: a property lookup into an array
  // means nothing, and saying so beats inventing a second way to index.
  const onArray = runCli(
    bin,
    ["schema", "type", "pricing", "tasks.price.output.fee.0", "-f", path],
    OFFLINE,
  );
  expect(onArray.ok).toBe(false);
});

// Past the slot, an address NAVIGATES what the slot answered — the same rule `schema type` has,
// so one grammar covers both. It is what makes an address that names nothing fail: a context has
// roots, and `headers` is not one of them.
test("schema context — the tail navigates the context, and a name that is not there fails", () => {
  const path = defFile();

  const self = schemaOf(path, "tasks.price.output.self");
  expect(Object.keys(self.doc.properties).sort()).toEqual(["headers", "result", "status"]);
  // Nothing was resolved, so nothing is reported: the address was walked into, not redirected.
  expect(self.stderr).toBe("");

  const deeper = schemaOf(path, "tasks.price.output.self.result.fee");
  expect(deeper.doc).toEqual({ type: "number" });

  for (const missing of ["tasks.price.output.headers", "output.fee"]) {
    const r = runCli(bin, ["schema", "context", "pricing", missing, "-f", path], OFFLINE);
    expect(r.ok, `${missing} names no root of that context`).toBe(false);
    expect(r.stderr).toContain("which holds:");
  }

  // The process output's context is one arm per ending, and navigation walks them WITHOUT
  // flattening: each arm keeps what that ending set and what it did not, which is the
  // correlation an expression reads with `??`.
  const arms = schemaOf(path, "output.outputs").doc;
  expect(arms.anyOf, "one arm per ending, still").toHaveLength(2);
  const nulls = arms.anyOf.map((a: any) =>
    Object.entries(a.properties)
      .filter(([, v]: any) => v.type === "null")
      .map(([k]) => k),
  );
  expect(nulls.flat().sort()).toEqual(["explain", "price"]);
});

// The other half of the same idea: an object schema IS a scope, its properties the roots, so an
// expression can be typed against whatever the address selected rather than only against a slot.
test("schema context -e — an expression is rooted where the address stopped", () => {
  const path = defFile();

  const fromSlot = runCli(
    bin,
    ["schema", "context", "pricing", "tasks.price.output", "-e", "self.result.fee", "--json", "-f", path], // eslint-disable-line
    OFFLINE,
  );
  const fromInside = runCli(
    bin,
    ["schema", "context", "pricing", "tasks.price.output.self", "-e", "result.fee", "--json", "-f", path], // eslint-disable-line
    OFFLINE,
  );
  expect(fromSlot.ok, fromSlot.stderr).toBe(true);
  expect(fromInside.ok, fromInside.stderr).toBe(true);
  expect(JSON.parse(fromInside.stdout)).toEqual(JSON.parse(fromSlot.stdout));
  expect(JSON.parse(fromSlot.stdout)).toEqual({ type: "number" });
});

// A schema prints as YAML unless JSON is asked for: it is the language definitions are written
// in, so an answer can be pasted into one, and the keys come out in the order they are read —
// what it IS before what it holds, with the pool last. Both forms are the same document.
test("schema context — a document is YAML by default, and JSON on request", () => {
  const path = defFile();
  const args = ["schema", "context", "pricing", "tasks.price.output", "-f", path];

  const asYAML = runCli(bin, args, OFFLINE);
  const asJSON = runCli(bin, [...args, "--json"], OFFLINE);
  expect(asYAML.ok, asYAML.stderr).toBe(true);
  expect(asJSON.ok, asJSON.stderr).toBe(true);

  expect(load(asYAML.stdout), "the two surfaces carry one document").toEqual(
    JSON.parse(asJSON.stdout),
  );

  // Reading order, not alphabetical: `type` before `properties`, and the pool it resolves
  // against after both.
  const keys = asYAML.stdout.split("\n").filter((l) => /^\S/.test(l)).map((l) => l.split(":")[0]);
  expect(keys).toEqual(["type", "properties", "required", "$defs"]);
});

test("schema context --json — the listing is the same addresses, as documents over one pool", () => {
  const r = runCli(bin, ["schema", "context", "pricing", "--json", "-f", defFile()], OFFLINE);
  expect(r.ok, `${r.stdout}${r.stderr}`).toBe(true);
  const doc = JSON.parse(r.stdout);

  const addresses = Object.keys(doc).filter((k) => k !== "$defs");
  expect(addresses.sort()).toEqual([
    "output",
    "tasks.explain.action",
    "tasks.explain.output",
    "tasks.explain.switch",
    "tasks.price.action",
    "tasks.price.on_error.0",
    "tasks.price.on_error.1",
    "tasks.price.output",
    "tasks.price.switch",
  ]);
  // One pool for the whole listing: the same definitions are reached from most addresses, so a
  // pool per entry would repeat most of the answer.
  expect(Object.keys(doc.$defs).length).toBeGreaterThan(0);
  for (const [address, entry] of Object.entries(doc)) {
    if (address === "$defs") continue;
    expect(entry, `${address} carries no pool of its own`).not.toHaveProperty("$defs");
  }
});

test("schema context — the answer is self-contained, and carries no pool it does not use", () => {
  const doc = schemaOf(defFile(), "tasks.price.on_error[0]").doc;

  // Every $ref resolves inside what was printed, so it can be piped into a generator whole.
  const refs = JSON.stringify(doc).match(/#\/\$defs\/[A-Za-z0-9_]+/g) ?? [];
  expect(refs.length, "this context references the process input").toBeGreaterThan(0);
  for (const ref of refs) {
    expect(Object.keys(doc.$defs ?? {})).toContain(ref.replace("#/$defs/", ""));
  }
  // …and nothing else: the pool holds four definitions, of which this slot reaches one.
  expect(Object.keys(doc.$defs)).toEqual(["input"]);
});

test("schema context — an unresolved $import types as a string, so no resolver runs", () => {
  const dir = mkdtempSync(join(tmpdir(), "genroc_schema_"));
  // A resolver that would write a file if it ever ran. The query must not spend a subprocess
  // (or a `tsc`) to answer what a code slot's context is.
  writeFileSync(
    join(dir, ".genroc"),
    `resolvers:\n  import: { phase: code, ext: .ts, command: [node, -e, "require('fs').writeFileSync('${join(dir, "RAN")}','x')"] }\n`,
  );
  writeFileSync(join(dir, "script.ts"), "export default () => 1;\n");
  const path = join(dir, "proc.yaml");
  writeFileSync(
    path,
    [
      "name: pricing",
      "tasks:",
      "  - id: price",
      "    action:",
      "      type: external",
      '      input: { code: "$import: ./script.ts" }',
      "      result_schema: {}",
      "    switch: [{ goto: end }]",
      "",
    ].join("\n"),
  );

  const r = runCli(bin, ["schema", "context", "pricing", "tasks.price.action", "-f", path], OFFLINE);
  expect(r.ok, `${r.stdout}${r.stderr}`).toBe(true);
  expect(existsSync(join(dir, "RAN")), "a context query must not run the project's resolvers").toBe(false);
});

test("schema context — an address that names nothing says what the task has", () => {
  const path = defFile();
  const noTask = runCli(bin, ["schema", "context", "pricing", "tasks.nope.output", "-f", path], OFFLINE);
  expect(noTask.ok).toBe(false);
  expect(noTask.stderr, "the tasks it does have are the answer").toContain("which holds: explain, price");

  const noRule = runCli(bin, ["schema", "context", "pricing", "tasks.price.on_error.9", "-f", path], OFFLINE);
  expect(noRule.ok).toBe(false);
  expect(noRule.stderr, "the rules it does have are the list of what to type").toContain("which holds: 0, 1");

  const noSwitch = runCli(bin, ["schema", "context", "nothing", "-f", path], OFFLINE);
  expect(noSwitch.ok).toBe(false);
  expect(noSwitch.stderr, "and a wrong process name lists the ones read").toContain("pricing");
});

// A task id is `validate:"required"` and nothing else, so it may hold a dot — and an address is
// a path in the expression language's own accessor syntax, where a dot is a step. Quoting is how
// such an id is named, and it is what the listing prints back.
const WEIRD = [
  "name: pricing",
  "tasks:",
  "  - id: step.one",
  "    action:",
  "      type: fetch",
  "      url: http://x/price",
  "      method: GET",
  "      responses:",
  "        200: { type: object, properties: { fee: { type: number } }, required: [fee] }",
  "    output: { fee: '$: self.result.fee' }",
  "    on_error:",
  "      - code: ['http.%']",
  "        goto: end",
  "    switch: [{ goto: next }]",
  "  - id: my task",
  "    output: { n: '$: 1' }",
  "    switch: [{ goto: end }]",
  "",
].join("\n");

test("schema context — an id no identifier can spell is addressed, and printed, quoted", () => {
  const path = defFile(WEIRD);
  const r = runCli(bin, ["schema", "context", "pricing", "-f", path], OFFLINE);
  expect(r.ok, `${r.stdout}${r.stderr}`).toBe(true);
  const addresses = r.stdout
    .trim()
    .split("\n")
    .filter((l) => !l.startsWith(" "))
    .map((l) => l.split(/\s{2,}/)[0]);

  // The rendering quotes anything that is not a plain identifier, which is wider than what the
  // grammar strictly needs — dotting a key that needed brackets is wrong, quoting one that did
  // not is merely verbose.
  expect(addresses).toContain('tasks["step.one"].output');
  expect(addresses).toContain('tasks["my task"].action');

  // The parser is the looser half: only a dot is a step, so an id with a space still resolves
  // bare — and the note says which address it landed on.
  const bare = runCli(bin, ["schema", "context", "pricing", "tasks.my task.output", "-f", path], OFFLINE);
  expect(bare.ok, bare.stderr).toBe(true);
  expect(bare.stderr, "nothing was resolved — a space is not a separator").toBe("");

  // What the listing prints IS an address: every key it emitted resolves, and to itself.
  for (const a of addresses) {
    const back = runCli(bin, ["schema", "context", "pricing", a, "-f", path], OFFLINE);
    expect(back.ok, `${a}: ${back.stderr}`).toBe(true);
  }
});

test("schema context — a dotted id names the task it splits into, and the error says so", () => {
  const path = defFile(WEIRD);
  const at = (address: string) =>
    runCli(bin, ["schema", "context", "pricing", address, "--json", "-f", path], OFFLINE);

  const dotted = at("tasks.step.one.output");
  expect(dotted.ok, "`tasks.step.one` names the task `step`, which does not exist").toBe(false);
  expect(dotted.stderr, "the fix is the quoted form, named in the error").toContain('["step.one"]');

  // Under a quoted id the rest of the path navigates as anywhere else, keyed rules included.
  const rule = at('tasks["step.one"].on_error[0]');
  expect(rule.ok, rule.stderr).toBe(true);
  expect(Object.keys(JSON.parse(rule.stdout).properties)).toContain("error");

  const action = at('tasks["step.one"].action');
  expect(action.ok, action.stderr).toBe(true);
  expect(JSON.parse(action.stdout).properties).toHaveProperty("outputs");
});

/** `-e` at a slot: the raw run, so a refusal can be read the same way as an answer. */
function typeOf(path: string, address: string, expr: string) {
  return runCli(bin, ["schema", "context", "pricing", address, "-e", expr, "--json", "-f", path], OFFLINE);
}

test("schema context -e — types one expression, and the slot is what decides", () => {
  const path = defFile();

  const ok = typeOf(path, "tasks.price.output", "self.result.fee");
  expect(ok.ok, `${ok.stdout}${ok.stderr}`).toBe(true);
  expect(JSON.parse(ok.stdout)).toEqual({ type: "number" });

  // The same expression one phase earlier: the action has not answered, so there is no
  // `self.result` to read — which is the whole reason the answer is per slot. The message is
  // the checker's own (one Roots hook, `validation.slotRoots`), not inference's "field not
  // found", which names the member and reads as a typo rather than a rule.
  const early = typeOf(path, "tasks.price.action", "self.result.fee");
  expect(early.ok, "self.result must not be readable from the action slots").toBe(false);
  expect(early.stderr).toContain("self.result is not available here");

  // …and the same expression at a phase that does carry it.
  const action = typeOf(path, "tasks.price.action", "input.amount");
  expect(action.ok, action.stderr).toBe(true);
  expect(JSON.parse(action.stdout)).toEqual({ type: "number" });
});

// The arms are not decoration: an expression is typed under each ending and the results joined,
// so `??` recovering from the branch that did not run is visible in the type it produces.
test("schema context -e — an expression at `output` is typed under every ending", () => {
  const path = defFile();

  const bare = typeOf(path, "output", "outputs.price.fee");
  expect(bare.ok, bare.stderr).toBe(true);
  const type = JSON.parse(bare.stdout).type.sort();
  expect(type, "null on the ending that ran `explain` instead").toEqual(["null", "number"]);

  const coalesced = typeOf(path, "output", "outputs.price.fee ?? 0");
  expect(coalesced.ok, coalesced.stderr).toBe(true);
  const recovered = JSON.parse(coalesced.stdout).type;
  expect(recovered, "?? removes the null under every arm").not.toContain("null");
});

// Availability is checked before inference, in that order, so a reference that is unavailable
// HERE gets the rule rather than the navigation failure. Same sentence the checker gives at
// registration; only the location is spelled in this command's own idiom.
test("schema context -e — an unavailable root is refused with the checker's reason", () => {
  const path = defFile(
    [
      "name: pricing",
      "tasks:",
      "  - id: price",
      "    action: { type: external }",
      "    output: { v: '$: 1' }",
      "    switch: [{ goto: end }]",
      "",
    ].join("\n"),
  );

  const untyped = typeOf(path, "tasks.price.output", "self.result.x");
  expect(untyped.ok).toBe(false);
  expect(untyped.stderr, "the action types no result, which is a rule and not a typo")
    .toContain("references self.result, but the action has no result_schema");

  const previous = typeOf(path, "tasks.price.output", "self.previous");
  expect(previous.ok).toBe(false);
  expect(previous.stderr, "nothing routes back to it, so there is no previous run")
    .toContain("no path returns to task \"price\"");
});

// A rule is a slot like any other, so the availability rule answers there too — under either
// spelling of the key, since both parse to the same four segments.
test("schema context -e — availability answers at an on_error rule, keyed or indexed", () => {
  const path = defFile();

  for (const address of ['tasks.price.on_error["0"]', "tasks.price.on_error[0]"]) {
    const r = typeOf(path, address, "self.result.fee");
    expect(r.ok, `${address}: a rule runs before the action has answered`).toBe(false);
    expect(r.stderr).toContain("self.result is not available here");
  }

  // …and the rule's own error IS readable there, which is what makes it a different context.
  const ok = typeOf(path, 'tasks.price.on_error["0"]', "error.data.wait");
  expect(ok.ok, ok.stderr).toBe(true);
  expect(JSON.parse(ok.stdout)).toEqual({ type: "number" });
});

test("schema context -e — a bad path is refused, and a pasted leaf is named as one", () => {
  const path = defFile();

  const bad = typeOf(path, "tasks.price.output", "self.result.nope");
  expect(bad.ok).toBe(false);
  expect(bad.stderr, "the checker's own diagnostic, with no apply and no server").toContain("nope");

  // The likely paste. Its parse error points at a `$`, which says nothing about the wrapper it
  // came from — so the hint carries the expression back, ready to run.
  const wrapped = typeOf(path, "tasks.price.output", "${self.result.fee}");
  expect(wrapped.ok).toBe(false);
  expect(wrapped.stderr).toContain("-e takes the expression itself: -e 'self.result.fee'");

  // An expression is evaluated somewhere; there is no such thing as one typed at the process.
  const args = ["schema", "context", "pricing", "-e", "input.amount", "-f", path];
  const noAddress = runCli(bin, args, OFFLINE);
  expect(noAddress.ok).toBe(false);
  expect(noAddress.stderr).toContain("needs an address");
});

// `secret: true` is declared on a config property and nothing else (registration refuses it
// elsewhere), and the answer carries it — which is what tells an author the slot they are
// writing reads one.
test("schema context -e — a config secret is reported as secret", () => {
  const path = defFile(
    [
      "name: pricing",
      "config_schema:",
      "  type: object",
      "  properties: { api_key: { type: string, secret: true } }",
      "  required: [api_key]",
      "tasks:",
      "  - id: price",
      "    action: { type: fetch, url: http://x, method: GET, responses: { 200: {} } }",
      "    switch: [{ goto: end }]",
      "",
    ].join("\n"),
  );

  const read = typeOf(path, "tasks.price.action", "config.api_key");
  expect(read.ok, read.stderr).toBe(true);
  expect(JSON.parse(read.stdout)).toEqual({ type: "string", secret: true });
});

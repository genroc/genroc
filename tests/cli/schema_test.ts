import { mkdtempSync, writeFileSync, existsSync } from "fs";
import { tmpdir } from "os";
import { join } from "path";
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
  const args = ["schema", "context", "pricing", ...(address ? [address] : []), "-f", path];
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
    "tasks.price.on_error[0]",
    "tasks.price.on_error[1]",
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
  const rule = schemaOf(path, "tasks.price.on_error[0]").doc;
  expect(Object.keys(rule.properties).sort()).toEqual(["error", "input", "outputs"]);
  expect(rule.properties.error.properties.data.properties).toEqual({ wait: { type: "number" } });

  // At the task the second rule routes to: the failure that got it there, and no `error` —
  // there is no rule being written at that task's own slots.
  const routed = schemaOf(path, "tasks.explain.output").doc;
  expect(Object.keys(routed.properties)).toContain("last_error");
  expect(Object.keys(routed.properties)).not.toContain("error");
});

test("schema context — a finer slot resolves to its phase, and says which", () => {
  const path = defFile();
  const url = runCli(bin, ["schema", "context", "pricing", "tasks.price.url", "-f", path], OFFLINE);

  expect(url.ok, url.stderr).toBe(true);
  // The resolution is the answer's most useful half — `url`, `timeout` and `body` are one
  // context, and nothing in the YAML says so — but it goes to stderr so stdout stays a document.
  expect(url.stderr).toContain("tasks.price.url → tasks.price.action");
  expect(JSON.parse(url.stdout)).toEqual(schemaOf(path, "tasks.price.action").doc);

  // `action.` is an optional segment, not a second spelling to remember.
  const withAction = schemaOf(path, "tasks.price.action.body.amount").doc;
  expect(withAction).toEqual(schemaOf(path, "tasks.price.input").doc);
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

  const r = runCli(bin, ["schema", "context", "pricing", "tasks.price.input", "-f", path], OFFLINE);
  expect(r.ok, `${r.stdout}${r.stderr}`).toBe(true);
  expect(existsSync(join(dir, "RAN")), "a context query must not run the project's resolvers").toBe(false);
});

test("schema context — an address that names nothing says what the task has", () => {
  const path = defFile();
  const noTask = runCli(bin, ["schema", "context", "pricing", "tasks.nope.output", "-f", path], OFFLINE);
  expect(noTask.ok).toBe(false);
  expect(noTask.stderr).toContain("names no task");

  const noRule = runCli(bin, ["schema", "context", "pricing", "tasks.price.on_error[9]", "-f", path], OFFLINE);
  expect(noRule.ok).toBe(false);
  expect(noRule.stderr, "the count is what tells you which index to use").toContain("2 on_error rule(s)");

  const noSwitch = runCli(bin, ["schema", "context", "nothing", "-f", path], OFFLINE);
  expect(noSwitch.ok).toBe(false);
  expect(noSwitch.stderr, "and a wrong process name lists the ones read").toContain("pricing");
});

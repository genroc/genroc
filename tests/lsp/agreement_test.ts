import { mkdtempSync, writeFileSync } from "fs";
import { tmpdir } from "os";
import { join } from "path";
import { beforeAll, afterAll, expect, test } from "vitest";
import { buildGenctlBinary, runCli } from "../helpers/cli.ts";
import { at, Doc, Lsp, useWorkspace } from "./helpers.ts";

// The two binaries answer "what is this slot" from ONE place. `genctl schema type <process>`
// lists every slot with its summary, and a hover on a slot's key prints the same summary — both
// are `validation.TypeSlots` rendered by `schema.Summary`, reached through two front doors.
//
// This pins the front doors. The rule that made them disagree lived one level down, on a KEY
// inside a slot, and is pinned at that level by `TestKeyHoverIsTheCLIsOwnAnswer` in
// internal/lsp — in Go, because the CLI prints a document there and a summary is not derivable
// from one out here without re-implementing Summary.

let lsp: Lsp;
let dir: string;
beforeAll(async () => {
  useWorkspace();
  dir = mkdtempSync(join(tmpdir(), "genroc_agree_"));
  lsp = await Lsp.start();
}, 60_000);
afterAll(async () => lsp?.stop());

const OFFLINE = { GENROC_SERVER: "http://127.0.0.1:1" };

/** Writes the definition to disk (the CLI reads a file) and hands back both handles. */
function both(name: string, lines: string[]): { doc: Doc; file: string } {
  const file = join(dir, `${name}.genroc.yaml`);
  const text = [`name: ${name}`, ...lines].join("\n") + "\n";
  writeFileSync(file, text, "utf8");
  return { doc: { uri: `file://${file}`, text }, file };
}

/** The CLI's summary for one slot address, off the listing. */
function listed(name: string, file: string, address: string): string {
  const r = runCli(buildGenctlBinary(), ["schema", "type", name, "-f", file], OFFLINE);
  expect(r.stderr).toBe("");
  const line = r.stdout.split("\n").find((l) => l.startsWith(address + " "));
  expect(line, `no listing line for ${address} in:\n${r.stdout}`).toBeDefined();
  return line!.slice(address.length).trim();
}

/** The type inside a hover's code span. */
function hovered(h: string): string {
  const m = /`([^`]+)`/.exec(h);
  expect(m, `no type in hover: ${h}`).not.toBeNull();
  return m![1];
}

const cases: { name: string; lines: string[]; address: string; fragment: string }[] = [
  {
    // The pair that disagreed one level down: nullable expression, non-nullable declaration.
    name: "agree_repair",
    lines: [
      "input_schema:",
      "  type: object",
      '  properties: { n: { type: [number, "null"] } }',
      "tasks:",
      "  - id: t",
      "    action:",
      "      type: child",
      "      name: nope",
      "      input:",
      "        v: '$: input.n'",
      "      input_schema: { type: object, properties: { v: { type: number } } }",
      "    switch: [{ goto: end }]",
      "output: { ok: true }",
    ],
    address: "tasks.t.action.input",
    fragment: "      <^input>:",
  },
  {
    // A generic child's payload, declared as the top type.
    name: "agree_unknown",
    lines: [
      "tasks:",
      "  - id: t",
      "    action:",
      "      type: child",
      "      name: nope",
      "      input:",
      "        payload:",
      "          who: 'x'",
      "      input_schema: { type: object, properties: { payload: { description: opaque } } }",
      "    switch: [{ goto: end }]",
      "output: { ok: true }",
    ],
    address: "tasks.t.action.input",
    fragment: "      <^input>:",
  },
  {
    // One of the two slots that was not in the type view at all, so the CLI could not answer.
    name: "agree_query",
    lines: [
      "tasks:",
      "  - id: t",
      "    action:",
      "      type: fetch",
      "      url: http://x.invalid/y",
      "      method: get",
      "      query:",
      "        page: 2",
      "      query_schema: { type: object, properties: { page: { type: number } } }",
      "    switch: [{ goto: end }]",
      "output: { ok: true }",
    ],
    address: "tasks.t.action.query",
    fragment: "      <^query>:",
  },
  {
    // A slot the definition HANDS BACK, where the declaration is what both publish.
    name: "agree_output",
    lines: [
      "tasks:",
      "  - id: t",
      "    output: { v: 1, w: 2 }",
      "    output_schema: { type: object, properties: { v: { type: number }, w: { type: number } }, required: [v] }",
      "    switch: [{ goto: end }]",
      "output: { ok: true }",
    ],
    address: "tasks.t.output",
    fragment: "    <^output>: { v: 1, w: 2 }",
  },
];

for (const c of cases) {
  test(`${c.name}: the CLI's listing and the hover print one summary`, async () => {
    const { doc, file } = both(c.name, c.lines);
    const cli = listed(c.name, file, c.address);
    const hover = hovered(await lsp.hover(at(c.fragment, doc)));
    expect(hover, `hover ${JSON.stringify(hover)} vs CLI ${JSON.stringify(cli)}`).toBe(cli);
  });
}

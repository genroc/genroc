import { beforeAll, afterAll, expect, test } from "vitest";
import { Lsp, orders, useWorkspace, type Cursor } from "./helpers.ts";

// EVERY column of every line, asserting what must never happen rather than what each place says.

let lsp: Lsp;
beforeAll(async () => {
  useWorkspace();
  lsp = await Lsp.start();
}, 60_000);
afterAll(async () => lsp?.stop());

const lines = () => orders.text.split("\n");

/** Every cursor position in the fixture, one per column of every non-empty line. */
function everyPosition(): Cursor[] {
  const out: Cursor[] = [];
  lines().forEach((text, line) => {
    for (let character = 0; character <= text.length; character++) {
      out.push({ ...orders, line, character, quoted: `${line + 1}:${character} ${text}` });
    }
  });
  return out;
}

// By KIND, not name: `headers` is both a fetch key and a member of its `self.result`.
const FIELD = 5; // a member of the scope
const PROPERTY = 10; // a key of the definition language
const VALUE = 12; // one of a closed set, like a task a `goto` may name

/** Whether a column sits inside an expression: a `$:` leaf, a `${ }`, or a bare `case`. */
function insideExpression(text: string, character: number): boolean {
  const head = text.slice(0, character);
  if (/^\s*(-\s*)?case:\s*"/.test(text) && head.includes('"')) return true;
  const open = head.lastIndexOf("${");
  if (open >= 0 && !head.slice(open).includes("}")) return true;
  return head.includes('"$:');
}

/** Whether a column sits inside the value of a slot that names a task. */
function insideRouting(text: string, character: number): boolean {
  const m = /^\s*(-\s*)?(goto|switch):[ \t]+(?=\S)/.exec(text);
  // A `switch:` with its cases on the lines below has no value on this one, and what a reader
  // writes there is a key.
  return m !== null && character >= m[0].length;
}

test("inside an expression, only the scope is offered", async () => {
  const wrong: string[] = [];
  for (const cursor of everyPosition()) {
    const text = lines()[cursor.line];
    if (!insideExpression(text, cursor.character)) continue;
    const items = await lsp.completionItems(cursor);
    const misplaced = items.filter((i) => i.kind !== FIELD);
    if (misplaced.length > 0) {
      wrong.push(`${cursor.quoted} -> ${misplaced.map((i) => `${i.label}(${i.kind})`).join(", ")}`);
    }
  }
  expect(wrong).toEqual([]);
}, 120_000);

test("inside a routing value, only tasks and the routing words are offered", async () => {
  const wrong: string[] = [];
  for (const cursor of everyPosition()) {
    const text = lines()[cursor.line];
    if (!insideRouting(text, cursor.character)) continue;
    const items = await lsp.completionItems(cursor);
    const misplaced = items.filter((i) => i.kind !== VALUE);
    if (misplaced.length > 0) {
      wrong.push(`${cursor.quoted} -> ${misplaced.map((i) => `${i.label}(${i.kind})`).join(", ")}`);
    }
  }
  expect(wrong).toEqual([]);
}, 120_000);

// Root keys anywhere else mean the server fell back to the outermost node.
test("the document's own keys are offered only at column 1", async () => {
  const wrong: string[] = [];
  for (const cursor of everyPosition()) {
    const items = await lsp.completionItems(cursor);
    if (!items.some((i) => i.kind === PROPERTY && (i.label === "tasks" || i.label === "config_schema"))) {
      continue;
    }
    // Column 1 is where a root key would go, whatever the line beneath the cursor holds; and
    // anywhere on a line that is itself at the root is the root's level.
    const indent = lines()[cursor.line].search(/\S/);
    if (indent !== 0 && cursor.character > 0) wrong.push(cursor.quoted);
  }
  expect(wrong).toEqual([]);
}, 120_000);

test("no diagnostic covers the whole document", async () => {
  const broken = {
    uri: orders.uri,
    text: orders.text.replace('- goto: "$fulfil"', '- goto: "$nope"'),
  };
  const ds = await lsp.diagnostics(broken);
  expect(ds).not.toEqual([]);
  for (const d of ds) {
    expect(d.startsWith("1-"), `covers the file: ${d}`).toBe(false);
  }
}, 60_000);

// DIFFERENTIAL: Enter adds no key and removes none, so a new blank line must answer as an existing
// sibling does. An absolute list of which mappings offer keys would rot.
test("pressing Enter inside a mapping answers as its siblings do", async () => {
  const original = lines();
  const wrong: string[] = [];
  for (let i = 0; i < original.length - 1; i++) {
    if (!/^\s*[\w$-]+:\s*$/.test(original[i])) continue; // a key introducing a block
    const childIndent = original[i + 1].search(/\S/);
    if (childIndent <= original[i].search(/\S/)) continue; // no block beneath it
    // A list child is skipped: Enter after `tasks:` starts a NEW element, while the dash of
    // the first one is beside that element's own keys. Different questions, different answers.
    if (!/^\s*[\w$-]+:/.test(original[i + 1]) || /^\s*-/.test(original[i + 1])) continue;

    const onSibling = await lsp.completionItems({
      uri: orders.uri,
      text: orders.text,
      line: i + 1,
      character: childIndent,
      quoted: "",
    });
    const opened = original.slice(0, i + 1).concat(" ".repeat(childIndent), original.slice(i + 1));
    const onBlank = await lsp.completionItems({
      uri: orders.uri,
      text: opened.join("\n"),
      line: i + 1,
      character: childIndent,
      quoted: "",
    });

    const set = (items: { label: string }[]) => items.map((it) => it.label).sort().join(", ");
    if (set(onBlank) !== set(onSibling)) {
      wrong.push(`${i + 1}: ${original[i].trim()}\n     blank:   ${set(onBlank)}\n     sibling: ${set(onSibling)}`);
    }
  }
  expect(wrong).toEqual([]);
}, 120_000);

// No tree node covers the gap after a dash but the sequence, which has no keys. Past the end of a
// line is left to the root-keys sweep: a flow value like `{ type: string }` ends there.
test("the gap after a list dash answers as the key beside it does", async () => {
  const wrong: string[] = [];
  for (const [i, text] of lines().entries()) {
    const m = /^(\s*)(-\s+)(?=[\w$"'-]+:)/.exec(text);
    if (!m) continue;
    const ask = async (character: number) =>
      (await lsp.completionItems({ ...orders, line: i, character, quoted: "" }))
        .map((it) => it.label)
        .sort()
        .join(", ");

    const reference = await ask(m[0].length);
    for (let character = m[1].length; character < m[0].length; character++) {
      const got = await ask(character);
      if (got !== reference) {
        wrong.push(`${i + 1}:${character} ${text.trim()}\n     here: ${got}\n     key:  ${reference}`);
      }
    }
  }
  expect(wrong).toEqual([]);
}, 120_000);

// `$` is not a word character, so an editor given no range inserts beside it: `$$tick`.
test("every value completion replaces what is already typed", async () => {
  const wrong: string[] = [];
  for (const cursor of everyPosition()) {
    const items = await lsp.completionItems(cursor);
    const values = items.filter((i) => i.kind === VALUE);
    if (values.length === 0) continue;
    for (const item of values) {
      if (!item.textEdit) {
        wrong.push(`${cursor.quoted} -> ${item.label} carries no edit`);
        continue;
      }
      const { start, end } = item.textEdit.range;
      if (end.character !== cursor.character) {
        wrong.push(`${cursor.quoted} -> ${item.label} ends at ${end.character}, not the cursor`);
      }
      // What sits behind the cursor is part of the name being typed, so it must be inside the
      // range rather than left beside the insertion.
      const before = lines()[cursor.line][cursor.character - 1] ?? "";
      if (/[\w$-]/.test(before) && start.character >= cursor.character) {
        wrong.push(`${cursor.quoted} -> ${item.label} leaves ${JSON.stringify(before)} behind`);
      }
    }
  }
  expect(wrong).toEqual([]);
}, 120_000);

// An empty value has a zero-width node, so the cursor past it lands on whatever encloses it.
test("a routing slot emptied of its value still offers what it may name", async () => {
  const wrong: string[] = [];
  for (const [i, text] of lines().entries()) {
    const m = /^(\s*(?:-\s+)?(?:goto|switch):)\s*\S.*$/.exec(text);
    if (!m) continue;
    const opened = lines().map((l, j) => (j === i ? `${m[1]} ` : l));
    const items = await lsp.completionItems({
      uri: orders.uri,
      text: opened.join("\n"),
      line: i,
      character: m[1].length + 1,
      quoted: `${i + 1}: ${text.trim()}`,
    });
    if (!items.every((it) => it.kind === VALUE) || items.length === 0) {
      wrong.push(`${i + 1}: ${text.trim()} -> ${items.map((it) => `${it.label}(${it.kind})`).join(", ")}`);
    }
  }
  expect(wrong).toEqual([]);
}, 120_000);

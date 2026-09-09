import { beforeAll, afterAll, expect, test } from "vitest";
import { Lsp, orders, useWorkspace, type Cursor } from "./helpers.ts";

// Every other file here picks positions by hand, and every bug reported from a real editor has
// been at a position nobody picked — `input_schema:`, a `case`, a `goto`. This one puts the
// cursor at EVERY column of every line and asserts what must never happen, rather than what
// each place should say.

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

// The KIND is what makes this exact. A name list cannot work: `headers` is a definition key on
// a fetch AND a member of that fetch's `self.result`, and a user's own schema may name a field
// anything at all. What can never be ambiguous is which QUESTION the server answered.
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

// Answering with keys inside an expression is the shape of every bug reported from an editor
// so far: a `case`, a `goto`, an interpolation. The server had read the cursor as sitting on a
// key, and the clause's own siblings came back.
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

// The root's keys are the answer only at column 1, which is where a root key would go. Anywhere
// else they are the shape of "the server fell back to the outermost thing containing this
// position" — which is what `input_schema:` answered with.
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

// A diagnostic that covers the file says nothing about where to look. Every rule that reports
// prose rather than a path used to land there.
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

// The sweeps above cover every position in a document that is FINISHED. The bug that started
// this file needed one that is not: a line pressed open and not yet typed.
//
// The check is DIFFERENTIAL, because an absolute one needs a list of which mappings have keys
// to offer and which are open maps of the author's own names — a list that would rot. Pressing
// Enter adds no key and removes none, so the answer on the new blank line must be the answer
// on a sibling that is already there.
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

// The gap a list dash leaves before its first key is writing that element's keys, but nothing
// in the document tree covers it — the sequence does, and a sequence has no keys of its own.
// Past the end of a line is NOT checked here: a flow value like `{ type: string }` ends there,
// so the cursor is inside it, and the root-keys sweep above already guards that position.
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

// An item offered where a token is half-typed must REPLACE it. `$` is not a word character, so
// an editor given no range inserts beside it — which is how `goto: $` became `$$tick`.
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

// A routing slot with nothing written yet is where help is wanted most, and its empty value
// has a zero-width node — so the cursor past it lands on whatever encloses it.
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

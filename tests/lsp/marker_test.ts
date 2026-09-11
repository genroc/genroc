import { beforeAll, expect, test } from "vitest";
import { at, edit, orders, useWorkspace } from "./helpers.ts";

// The markers are test infrastructure with real logic in them, and a bug here would not fail a
// test — it would quietly point every other test at the wrong character.

beforeAll(() => useWorkspace());

test("<|> puts the cursor where it stands and changes nothing", () => {
  const c = at(`      method: <|>get`);
  expect(c.text).toBe(orders.text);
  expect(orders.text.split("\n")[c.line].slice(c.character)).toBe("get");
});

test("<|text> takes the text back out and leaves the cursor in its place", () => {
  const c = at(`      url: "https://api.example.com/price?customer=\${ input.<|customer_id> }"`);
  // Scoped to the line: `input.customer_id` is written again in the fulfil task, and the
  // elision must take out only the one the cursor is on.
  expect(c.text.split("\n")[c.line]).toContain('customer=${ input. }');
  expect(c.text).toContain('order: "$: input.customer_id"');
  // The cursor sits where the next character would be typed.
  expect(c.text.split("\n")[c.line].slice(c.character)).toBe(" }\"");
});

test("<^text> keeps the text and lands inside it", () => {
  const c = at(`      name: <^shipment>`);
  expect(c.text).toBe(orders.text);
  const line = orders.text.split("\n")[c.line];
  expect(line.slice(c.character)).toBe("ment");
});

test("a fragment that matches nothing is refused, not guessed at", () => {
  expect(() => at(`      method: <^put>`)).toThrow(/not in/);
});

// Two `goto: "$review"` lines are written identically. Picking either would make the test mean
// something other than what it reads as.
test("a fragment that matches twice is refused", () => {
  expect(() => at(`        goto: "$<^review>"`)).toThrow(/more than once/);
});

test("a snippet with no marker is a mistake, not a no-op", () => {
  expect(() => at(`      method: get`)).toThrow(/no <\|> or/);
});

test("edit refuses an ambiguous replacement the same way", () => {
  expect(() => edit(orders, { 'goto: "$review"': "goto: end" })).toThrow(/more than once/);
});

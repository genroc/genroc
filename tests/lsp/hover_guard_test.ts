import { afterAll, beforeAll, expect, test } from "vitest";
import { at, guarded, Lsp, useWorkspace } from "./helpers.ts";

// A hover on either side of a null check must differ: saying nullable where the checker proved
// otherwise contradicts registration. specs/guard-narrowing.md.

let lsp: Lsp;
beforeAll(async () => {
  useWorkspace();
  lsp = await Lsp.start();
}, 60_000);
afterAll(async () => lsp?.stop());

// ── within one switch: a later case sees the earlier ones having failed ──────────

test("before the null check, the output is nullable", async () => {
  expect(await lsp.hover(at(`      - case: "self.<^output> == null"`, guarded))).toBe(
    "`self.output` → `object{activated, email}|null`",
  );
});

test("the case below the null check reads it narrowed", async () => {
  expect(await lsp.hover(at(`      - case: "self.<^output>.activated"`, guarded))).toBe(
    "`self.output` → `object{activated, email}`",
  );
});

test("and so the member is a plain boolean, not boolean|null", async () => {
  expect(await lsp.hover(at(`      - case: "self.output.<^activated>"`, guarded))).toBe(
    "`self.output.activated` → `boolean`",
  );
});

// ── across an edge: the proof travels to the task the case routes to ─────────────

test("the task the guard routes to reads the proved value", async () => {
  expect(await lsp.hover(at(`        to: "$: outputs.load.<^email>"`, guarded))).toBe(
    "`outputs.load.email` → `string`",
  );
});

// ── on_error: only a rule with no `code` is a pure case, so only it narrows ──────

test("before the rule above it, the payload is nullable", async () => {
  expect(await lsp.hover(at(`      - case: "error.data.<^retry_after> == null"`, guarded))).toBe(
    "`error.data.retry_after` → `integer|null`",
  );
});

test("the rule below a pure case reads it narrowed", async () => {
  expect(await lsp.hover(at(`      - case: "error.data.<^retry_after> > 0"`, guarded))).toBe(
    "`error.data.retry_after` → `integer`",
  );
});

// Not the negation of the cases above: `missing` is reached only by the null check, so it reads
// that case's own proof, that the value IS null.
test("a case's own proof travels the edge it selects", async () => {
  expect(await lsp.hover(at(`      absent: "$: outputs.<^load> == null"`, guarded))).toBe(
    "`outputs.load` → `null`",
  );
});

// ── a clause is not its case: `panic`, `raise` and `retry` assume the guard held ──

// The case establishes the fact, so showing it already proved would be circular.
test("the rule's own case still reads the payload nullable", async () => {
  expect(await lsp.hover(at(`        case: "error.data.<^wait> != null"`, guarded))).toBe(
    "`error.data.wait` → `integer|null`",
  );
});

// A rule retries only when it CAUGHT, and catching means `(code…) && case` held.
test("the retry delay beside it reads it narrowed", async () => {
  expect(await lsp.hover(at(`          delay: "$: error.data.<^wait>"`, guarded))).toBe(
    "`error.data.wait` → `integer`",
  );
});

// The panic renders only if its case matched, so it may read what that case proved.
test("a switch case's panic message reads what its own case proved", async () => {
  expect(await lsp.hover(at(`          message: "already \${ self.output.<^receipt> }"`, guarded))).toBe(
    "`self.output.receipt` → `string`",
  );
});

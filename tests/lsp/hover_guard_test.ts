import { afterAll, beforeAll, expect, test } from "vitest";
import { at, guarded, Lsp, useWorkspace } from "./helpers.ts";

// What a GUARD proves, as the author sees it: hover the same reference on both sides of a
// null check and the answer must differ. `<^text>` puts the cursor inside `text`.
//
// This is the pairing that matters — the checker accepting a definition while the hover says
// the value may be null is the editor contradicting registration, and the author believes the
// editor. specs/guard-narrowing.md.

let lsp: Lsp;
beforeAll(async () => {
  useWorkspace();
  lsp = await Lsp.start();
}, 60_000);
afterAll(async () => lsp?.stop());

// ── within one switch: a later case sees the earlier ones having failed ──────────

test("before the null check, the output is nullable", async () => {
  expect(await lsp.hover(at(`      - case: "self.<^output> == null"`, guarded))).toBe(
    "`self.output` → **object{activated, email}|null**",
  );
});

test("the case below the null check reads it narrowed", async () => {
  expect(await lsp.hover(at(`      - case: "self.<^output>.activated"`, guarded))).toBe(
    "`self.output` → **object{activated, email}**",
  );
});

test("and so the member is a plain boolean, not boolean|null", async () => {
  expect(await lsp.hover(at(`      - case: "self.output.<^activated>"`, guarded))).toBe(
    "`self.output.activated` → **boolean**",
  );
});

// ── across an edge: the proof travels to the task the case routes to ─────────────

test("the task the guard routes to reads the proved value", async () => {
  expect(await lsp.hover(at(`        to: "$: outputs.load.<^email>"`, guarded))).toBe(
    "`outputs.load.email` → **string**",
  );
});

// ── on_error: only a rule with no `code` is a pure case, so only it narrows ──────

test("before the rule above it, the payload is nullable", async () => {
  expect(await lsp.hover(at(`      - case: "error.data.<^retry_after> == null"`, guarded))).toBe(
    "`error.data.retry_after` → **integer|null**",
  );
});

test("the rule below a pure case reads it narrowed", async () => {
  expect(await lsp.hover(at(`      - case: "error.data.<^retry_after> > 0"`, guarded))).toBe(
    "`error.data.retry_after` → **integer**",
  );
});

// The other half of an edge: not the NEGATION of the cases above, but what the case that was
// taken proves itself. `missing` is reached only by the null check, so the proof arriving
// there is that one's own — and it proves the value IS null, the state nothing else covers.
test("a case's own proof travels the edge it selects", async () => {
  expect(await lsp.hover(at(`      absent: "$: outputs.<^load> == null"`, guarded))).toBe(
    "`outputs.load` → **null**",
  );
});

// ── a clause is not its case: `panic`, `raise` and `retry` assume the guard held ──

// The case expression must keep the UNNARROWED type — it is what establishes the fact, and an
// editor that already showed it proved would be reasoning in a circle.
test("the rule's own case still reads the payload nullable", async () => {
  expect(await lsp.hover(at(`        case: "error.data.<^wait> != null"`, guarded))).toBe(
    "`error.data.wait` → **integer|null**",
  );
});

// One line down, inside the retry the case guards, it is proved: the rule only retries when it
// CAUGHT, and catching means `(code…) && case` held.
test("the retry delay beside it reads it narrowed", async () => {
  expect(await lsp.hover(at(`          delay: "$: error.data.<^wait>"`, guarded))).toBe(
    "`error.data.wait` → **integer**",
  );
});

// The same one level down in a switch: the panic renders only if its case matched, so it may
// read what that case proved — which is the shape an author writes first, a guard and the
// message it was written to make safe.
test("a switch case's panic message reads what its own case proved", async () => {
  expect(await lsp.hover(at(`          message: "already \${ self.output.<^receipt> }"`, guarded))).toBe(
    "`self.output.receipt` → **string**",
  );
});

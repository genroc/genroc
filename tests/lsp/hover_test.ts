import { beforeAll, afterAll, expect, test } from "vitest";
import { at, Lsp, useWorkspace } from "./helpers.ts";

// What the server says about the thing under the cursor. `<^text>` puts the cursor inside
// `text`, which stays — this is reading, not writing.

let lsp: Lsp;
beforeAll(async () => {
  useWorkspace();
  lsp = await Lsp.start();
}, 60_000);
afterAll(async () => lsp?.stop());

// The reason to build hover: the type an author is otherwise guessing at.
test("an expression reports the type it infers to", async () => {
  expect(
    await lsp.hover(at(`      charged: "$: <^self.result.total> - (self.result.discount ?? 0)"`)),
  ).toContain("`self.result.total - (self.result.discount ?? 0)` → **number**");
});

// Hover types the whole LEAF, not the sub-expression under the cursor: the two positions
// below are inside different parts of one expression and answer the same. Worth pinning
// because it is a limit someone will otherwise mistake for a bug.
test("hover types the whole expression, wherever in it the cursor sits", async () => {
  const onTotal = await lsp.hover(
    at(`      charged: "$: self.result.<^total> - (self.result.discount ?? 0)"`),
  );
  const onDiscount = await lsp.hover(
    at(`      charged: "$: self.result.total - (self.result.<^discount> ?? 0)"`),
  );
  expect(onTotal).toBe(onDiscount);
});

// A `${ }` inside a longer string types as the string it renders into, so the leaf says
// nothing useful — the interpolation being written is what has a type.
test("an interpolation inside a url is typed on its own", async () => {
  expect(
    await lsp.hover(
      at(`      url: "https://api.example.com/price?customer=\${ <^input.customer_id> }"`),
    ),
  ).toContain("`input.customer_id` → **string**");
});

test("a slot reports its own type", async () => {
  expect(await lsp.hover(at(`    <^output>:`))).toContain("**tasks.price.output** — object{charged}");
});

// The scope is not one thing. An action runs before its own result exists; the switch after it
// can read what the output produced. The same fixture shows both.
test("the scope named on hover is the slot's, not the file's", async () => {
  expect(await lsp.hover(at(`      method: <^GET>`))).toContain(
    "_in scope at `tasks.price.action`_",
  );
  expect(await lsp.hover(at(`      - case: "<^self.output.charged> > 1000"`))).toContain(
    "_in scope at `tasks.price.switch`_",
  );
});

test("an action's scope has no self; the switch after it does", async () => {
  expect(await lsp.hover(at(`      method: <^GET>`))).not.toContain("self");
  expect(await lsp.hover(at(`      - case: "<^self.output.charged> > 1000"`))).toContain("self");
});

// `discount` is optional, so the `?? 0` is what makes the expression type at all. Taking it
// back out is the mistake someone actually makes, and hovering it is when they ask why.
test("an expression that does not type says why, instead of going quiet", async () => {
  const md = await lsp.hover(
    at(`      charged: "$: self.result.total - (self.result.discount<| ?? 0>)"`),
  );
  expect(md).toContain("operator requires non-nullable operands");
  // And it still names the scope, so the fix (`?? 0`) is written with the same information.
  expect(md).toContain("_in scope at \`tasks.price.output\`_");
});

test("there is nothing to say about the process name", async () => {
  expect(await lsp.hover(at(`name: <^orders>`))).toBe("");
});

import { beforeAll, afterAll, expect, test } from "vitest";
import { at, Lsp, useWorkspace } from "./helpers.ts";

// Where a reference points. `definition()` answers `<file>:<line>`, so the assertion reads as
// the jump a reader would make.

let lsp: Lsp;
beforeAll(async () => {
  useWorkspace();
  lsp = await Lsp.start();
}, 60_000);
afterAll(async () => lsp?.stop());

// `goto: "$review"` is written twice — once routed to, once escalated to. The quoted fragment
// carries the line above so the cursor is never ambiguous; the helper refuses a fragment that
// matches more than one place rather than picking.
test("a goto in a switch case jumps to the task it names", async () => {
  expect(
    await lsp.definition(
      at(`      - case: "self.output.charged > 1000"\n        goto: "$<^review>"`),
    ),
  ).toBe("orders.genroc.yaml:37");
});

test("a goto in an on_error rule reaches the same task from a different slot", async () => {
  expect(
    await lsp.definition(
      at(`        retry: { attempts: 3, delay: 2s }\n        goto: "$<^review>"`),
    ),
  ).toBe("orders.genroc.yaml:37");
});

test("a goto written as the catch-all case jumps too", async () => {
  expect(await lsp.definition(at(`      - goto: "$<^fulfil>"`))).toBe("orders.genroc.yaml:50");
});

// `end` terminates the instance and `next` is positional: neither names a task, and a jump to
// somewhere arbitrary is worse than no jump.
test("`end` names no task, so there is nowhere to go", async () => {
  expect(await lsp.definition(at(`      - goto: <^end>`))).toBe("");
});

// The reference that leaves the file. It resolves through the workspace the editor named at
// startup, not through `.genroc` — which answers which files DEPLOY, a different question.
test("a child action's process jumps to the file that defines it", async () => {
  expect(await lsp.definition(at(`      name: <^shipment>`))).toBe("shipment.genroc.yaml:1");
});

// `name` is a common field. Only a child action's is a process reference.
test("the definition's own name is not a reference to itself", async () => {
  expect(await lsp.definition(at(`name: <^orders>`))).toBe("");
});

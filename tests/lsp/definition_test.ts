import { beforeAll, afterAll, expect, test } from "vitest";
import { at, Lsp, useWorkspace } from "./helpers.ts";

let lsp: Lsp;
beforeAll(async () => {
  useWorkspace();
  lsp = await Lsp.start();
}, 60_000);
afterAll(async () => lsp?.stop());

// `goto: "$review"` is written twice, so the fragment carries the line above it.
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
      at(`        retry: { retries: 3, delay: 2s }\n        goto: "$<^review>"`),
    ),
  ).toBe("orders.genroc.yaml:37");
});

test("a goto written as the catch-all case jumps too", async () => {
  expect(await lsp.definition(at(`      - goto: "$<^fulfil>"`))).toBe("orders.genroc.yaml:50");
});

test("`end` names no task, so there is nowhere to go", async () => {
  expect(await lsp.definition(at(`      - goto: <^end>`))).toBe("");
});

// Resolved through the editor's workspace, not `.genroc`, which answers which files DEPLOY.
test("a child action's process jumps to the file that defines it", async () => {
  expect(await lsp.definition(at(`      name: <^shipment>`))).toBe("shipment.genroc.yaml:1");
});

// `name` is a common field. Only a child action's is a process reference.
test("the definition's own name is not a reference to itself", async () => {
  expect(await lsp.definition(at(`name: <^orders>`))).toBe("");
});

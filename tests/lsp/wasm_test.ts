import { beforeAll, afterAll, expect, test } from "vitest";
import { at, edit, Lsp, orders, useWorkspace } from "./helpers.ts";

// The extension's wasm fallback, checked against the binary. Also the only test of host stdio:
// Node's WASI stdin is non-blocking on Linux and blocking on macOS, so these can pass on a laptop
// and fail on CI (blockStdio in cmd/genctl).

let wasm: Lsp;
let native: Lsp;
beforeAll(async () => {
  useWorkspace();
  wasm = await Lsp.startWasm();
  native = await Lsp.start();
}, 180_000);
afterAll(async () => {
  await wasm?.stop();
  await native?.stop();
});

test("it underlines what the binary underlines", async () => {
  const broken = edit(orders, { "    customer_id: { type: string }": "    customer_id: string" });
  const answer = await wasm.diagnostics(broken);
  expect(answer).toEqual([`6: customer_id: a schema must be an object, not a string`]);
  expect(answer).toEqual(await native.diagnostics(broken));
});

test("it offers what the binary offers", async () => {
  const cursor = at(`      url: "https://api.example.com/price?customer=\${ input.<|customer_id> }"`);
  const answer = await wasm.completions(cursor);
  expect(answer).toEqual(["amount", "currency", "customer_id"]);
  expect(answer).toEqual(await native.completions(cursor));
});

test("it reaches the other file, which needs the workspace preopened", async () => {
  const cursor = at(`      name: <^shipment>`);
  expect(await wasm.definition(cursor)).toBe("shipment.genroc.yaml:1");
  expect(await wasm.definition(cursor)).toBe(await native.definition(cursor));
});

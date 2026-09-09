import { beforeAll, afterAll, expect, test } from "vitest";
import { at, edit, Lsp, orders, useWorkspace } from "./helpers.ts";

// The server the extension bundles for a machine with no genctl: the same code, compiled to
// WebAssembly. Every test here is DIFFERENTIAL — the fallback has to answer what the binary
// answers, because a fallback that quietly disagrees is worse than no fallback at all.
//
// It is also the only test of the host's stdio: Node's WASI hands the guest a NON-BLOCKING
// stdin on Linux and a blocking one on macOS, so these pass on a laptop while failing on CI.
// That asymmetry is the point of running them there — see blockStdio in cmd/genctl.

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

// The one answer that needs the FILESYSTEM: a wasm module reaches no path that is not preopened
// for it, so this is what proves the loader hands it the workspace.
test("it reaches the other file, which needs the workspace preopened", async () => {
  const cursor = at(`      name: <^shipment>`);
  expect(await wasm.definition(cursor)).toBe("shipment.genroc.yaml:1");
  expect(await wasm.definition(cursor)).toBe(await native.definition(cursor));
});

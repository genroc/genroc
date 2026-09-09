// Runs `genctl lsp` as a WebAssembly module over THIS process's stdio, so the editor speaks to
// it exactly as it speaks to the binary — the extension stays a launcher, it just launches
// something else. specs/language-server.md §4.
//
// Started as VS Code's own Electron with ELECTRON_RUN_AS_NODE=1, so there is no second runtime
// to install; node:wasi is what that Node carries.
import { readFile } from "node:fs/promises";
import { WASI } from "node:wasi";

const [, , module, ...roots] = process.argv;

// Each workspace folder is preopened AT ITS OWN ABSOLUTE PATH, so a path inside the module is
// the same string as outside it: the server reads the `file://` URIs the editor sends and walks
// the workspace with no translation between them. A folder that is not preopened is simply not
// found, which is what a server with no workspace already does.
const preopens = {};
for (const root of roots) preopens[root] = root;

const wasi = new WASI({ version: "preview1", args: ["genctl", "lsp"], env: {}, preopens, returnOnExit: true });

try {
  const wasm = await WebAssembly.compile(await readFile(module));
  process.exit(wasi.start(await WebAssembly.instantiate(wasm, wasi.getImportObject())));
} catch (err) {
  // stderr, never stdout: stdout is the protocol, and a line of prose there is a frame the
  // editor cannot parse.
  process.stderr.write(`genroc: cannot run ${module}: ${err}\n`);
  process.exit(1);
}

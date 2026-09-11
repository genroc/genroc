import { beforeAll, expect, test } from "vitest";
import { writeFileSync } from "node:fs";
import { join } from "node:path";
import { tmpdir } from "node:os";
import { spawnSync } from "node:child_process";
import { buildGenctlBinary, runCli } from "../helpers/cli.ts";
import { uid } from "../helpers/genctl.ts";

// A rejected definition used to print prose with the location inside it, so nothing could
// jump to the line. The slot address now travels with the diagnostic and genctl turns it back
// into a position using the index it parsed the file with. specs/language-server.md §2, §3.

let bin: string;
beforeAll(() => {
  bin = buildGenctlBinary();
}, 60_000);

function writeSource(body: string): string {
  const path = join(tmpdir(), `genroc_located_${Date.now()}_${Math.random().toString(36).slice(2)}.genroc.yaml`);
  writeFileSync(path, body, "utf8");
  return path;
}

test("apply — every broken slot is reported as file:line:col", () => {
  const name = uid("located");
  //            1        2       3            4           5              6         7
  const path = writeSource(
    `name: ${name}\n` +      // 1
      `tasks:\n` +           // 2
      `  - id: a\n` +        // 3
      `    action:\n` +      // 4
      `      type: fetch\n` + // 5
      `      method: post\n` + // 6
      `      url: "$: nope.x"\n` + // 7
      `    switch: next\n` + // 8
      `  - id: b\n` +        // 9
      `    action:\n` +      // 10
      `      type: fetch\n` + // 11
      `      method: post\n` + // 12
      `      url: "$: alsonope.y"\n` + // 13
      `    switch: end\n`,   // 14
  );

  const r = runCli(bin, ["apply", "--check-only", "-f", path]);
  expect(r.exitCode).toBe(1);

  const lines = r.stderr.trim().split("\n");
  // Both, not just the first: inference used to stop at the failure it found.
  expect(lines).toHaveLength(2);
  for (const line of lines) expect(line.startsWith("genctl: ")).toBe(true);

  // The url of each task, not the action block it sits in (specs/language-server.md §7b).
  expect(lines[0]).toContain(`${path}:7:12:`);
  expect(lines[0]).toContain(`field "nope" not found`);
  expect(lines[1]).toContain(`${path}:13:12:`);
  expect(lines[1]).toContain(`field "alsonope" not found`);
});

test("apply — a missing required field points at the node that lacks it", () => {
  // `tasks[0].id` has no node of its own, so the location is the task it is missing from.
  const name = uid("nofield");
  const path = writeSource(`name: ${name}\ntasks:\n  - action:\n      type: fetch\n      url: u\n    switch: end\n`);
  const r = runCli(bin, ["apply", "--check-only", "-f", path]);
  expect(r.exitCode).toBe(1);
  expect(r.stderr).toContain(`${path}:3:5:`);
  expect(r.stderr).toContain("id is required");
});

// Both argv shapes, because both are real: the VS Code extension runs a bare `genctl lsp`,
// and clients that name the transport append `--stdio`. The extension asks for no flag ON
// PURPOSE — depending on one it does not need is what turned an older genctl into five
// restarts and a disposed connection.
test.each([[[]], [["--stdio"]]])(
  "lsp — genctl lsp %j speaks LSP on stdio and underlines the key a typo is in",
  (extraArgs: string[]) => {
  // The whole editor path through the binary people already have: framed in, framed out.
  const text = 'name: demo\ntasks:\n  - id: a\n    on_eror: []\n    switch: end\n';
  const frames = [
    { jsonrpc: "2.0", id: 1, method: "initialize", params: {} },
    {
      jsonrpc: "2.0",
      method: "textDocument/didOpen",
      params: { textDocument: { uri: "file:///w/d.genroc.yaml", version: 1, text } },
    },
    { jsonrpc: "2.0", id: 2, method: "shutdown" },
    { jsonrpc: "2.0", method: "exit" },
  ]
    .map((m) => {
      const b = Buffer.from(JSON.stringify(m), "utf8");
      return `Content-Length: ${b.length}\r\n\r\n${b}`;
    })
    .join("");

  const r = spawnSync(bin, ["lsp", ...extraArgs], { input: frames, encoding: "utf8" });
  expect(r.status, r.stderr).toBe(0);

  const msgs = readFrames(r.stdout ?? "");
  const publish = msgs.find((m) => m.method === "textDocument/publishDiagnostics");
  expect(publish, `no publishDiagnostics among ${msgs.length} message(s)`).toBeDefined();

  const ds = publish!.params.diagnostics;
  expect(ds).toHaveLength(1);
  expect(ds[0].code).toBe("def.unknown_key");
  expect(ds[0].message).toContain("on_eror");
  // Line 4 (0-based 3), and the key's own columns rather than its value's.
  expect(ds[0].range.start).toEqual({ line: 3, character: 4 });
  },
);

/** Read a Content-Length framed LSP stream. */
function readFrames(out: string): any[] {
  const msgs: any[] = [];
  let rest = Buffer.from(out, "utf8");
  while (rest.length > 0) {
    const sep = rest.indexOf("\r\n\r\n");
    if (sep < 0) break;
    const n = Number(/Content-Length: (\d+)/.exec(rest.subarray(0, sep).toString())![1]);
    const body = rest.subarray(sep + 4, sep + 4 + n);
    msgs.push(JSON.parse(body.toString("utf8")));
    rest = rest.subarray(sep + 4 + n);
  }
  return msgs;
}

// The extension probes with this before starting the client. It must exit 0 on a binary that
// has the subcommand — `-v` cannot answer, because it succeeds on every genctl ever built,
// including the ones that answer `genctl lsp` with a usage dump and exit 1.
test("lsp — `genctl lsp -h` is the probe an editor can trust", () => {
  const r = runCli(bin, ["lsp", "-h"]);
  expect(r.exitCode).toBe(0);
  expect(r.stdout).toContain("stdin/stdout");
});

test("lsp — an argument that is not a transport flag is still refused", () => {
  const r = runCli(bin, ["lsp", "--frobnicate"]);
  expect(r.exitCode).toBe(1);
  expect(r.stderr).toContain("--stdio");
});

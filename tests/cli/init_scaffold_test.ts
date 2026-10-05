import { spawnSync } from "child_process";
import { mkdtempSync, readFileSync, readdirSync, writeFileSync } from "fs";
import { tmpdir } from "os";
import { join } from "path";
import { beforeAll, expect, test } from "vitest";
import { buildGenctlBinary, runCli } from "../helpers/cli.ts";

// What `genctl init` writes must APPLY: it is embedded rather than exercised, so it rots silently.
// No server anywhere here: the scaffold must typecheck on a laptop.

let bin: string;
beforeAll(() => {
  bin = buildGenctlBinary();
}, 60_000);

const OFFLINE = { GENROC_SERVER: "http://127.0.0.1:1" };
const REPO = new URL("../../", import.meta.url).pathname;

/** An inferred type is stored as a `$ref` into its own `$defs`; this is what it points at. */
// eslint-disable-next-line @typescript-eslint/no-explicit-any
function resolved(t: any): any {
  const name = typeof t?.$ref === "string" ? t.$ref.replace("#/$defs/", "") : "";
  return t?.$defs?.[name] ?? t;
}

function scaffold(...flags: string[]): string {
  const dir = mkdtempSync(join(tmpdir(), "genroc_scaffold_"));
  const r = runCli(bin, ["init", dir, "-y", ...flags], OFFLINE);
  expect(r.exitCode, r.stderr).toBe(0);
  return dir;
}

test("the base scaffold typechecks with no server", () => {
  const dir = scaffold();
  const r = runCli(
    bin,
    ["schema", "type", "hello", "output", "--json", "-f", join(dir, "definitions/hello.genroc.yaml")],
    OFFLINE,
  );
  expect(r.stderr).toBe("");
  expect(resolved(JSON.parse(r.stdout))).toMatchObject({
    properties: { greeting: { type: "string" } },
  });
});

test("the eval-node scaffold typechecks with no server, spread and all", () => {
  const dir = scaffold("--eval-node");
  const r = runCli(
    bin,
    ["schema", "type", "hello", "output", "--json", "-f", join(dir, "definitions/hello.genroc.yaml")],
    OFFLINE,
  );
  expect(r.stderr).toBe("");
  // Two claims: the `$process` spread resolved, and the explicit `result_schema` beat what it
  // supplied (script-node forwards the top type, which would be `unknown`).
  expect(resolved(JSON.parse(r.stdout))).toMatchObject({
    properties: { greeting: { type: "string" } },
  });
});

// `.genroc` binds the generated types to these addresses, so a scaffold that typechecks can still
// generate `Input = unknown`.
test("the addresses the resolver generates types from resolve to real types", () => {
  const dir = scaffold("--eval-node");
  const f = join(dir, "definitions/hello.genroc.yaml");
  const ask = (address: string) =>
    resolved(JSON.parse(runCli(bin, ["schema", "type", "hello", address, "--json", "-f", f], OFFLINE).stdout));

  // `Input`: what this caller wrote, not what script-node accepts (the top type).
  expect(ask("tasks.greet.action.input.input")).toMatchObject({
    properties: { who: { type: "string" } },
  });
  // `Output`: what the script must RETURN, from the caller's own narrowing.
  expect(ask("tasks.greet.action.result")).toMatchObject({
    properties: { greeting: { type: "string" } },
  });
});

test("the caller no longer names the child, so the two cannot drift", () => {
  const dir = scaffold("--eval-node");
  const hello = readFileSync(join(dir, "definitions/hello.genroc.yaml"), "utf8");
  expect(hello).toContain("$process: ./script-node.genroc.yaml");
  expect(hello, "the spread supplies the name; writing one beside it invites drift").not.toContain(
    "name: script-node",
  );
});

test("the spread's input_schema catches a typo in the input, with no server", () => {
  const dir = scaffold("--eval-node");
  const broken = readFileSync(join(dir, "definitions/hello.genroc.yaml"), "utf8")
    .replace("code: ", "cdoe: ")
    .replace("name: hello", "name: broken");
  // Written BESIDE the child, so the spread's `./script-node.genroc.yaml` still resolves.
  const beside = join(dir, "definitions/broken.genroc.yaml");
  writeFileSync(beside, broken, "utf8");
  const r = runCli(bin, ["apply", "--check-only", "-f", beside], OFFLINE);
  expect(r.exitCode).not.toBe(0);
  expect(r.stderr).toContain("cdoe");
  expect(r.stderr, "the check must not need a server").not.toMatch(/connection refused|dial tcp/i);
});

test("the addresses asserted above are the ones the scaffold actually binds", () => {
  const dir = scaffold("--eval-node");
  const cfg = readFileSync(join(dir, ".genroc"), "utf8");
  // Hard-coding an address in the test above is only safe while the config still names it.
  expect(cfg).toContain("Input: task.action.input.input");
  expect(cfg).toContain("Output: task.action.result");
});

// The `$schema` comment is looser than the server on the typos people make (`on_eror:`).
test("no scaffolded definition points at the published JSON Schema", () => {
  for (const flags of [[], ["--eval-node"]]) {
    const dir = scaffold(...flags);
    const defs = join(dir, "definitions");
    for (const f of readdirSync(defs).filter((f) => f.endsWith(".genroc.yaml"))) {
      expect(readFileSync(join(defs, f), "utf8"), `${flags} ${f}`).not.toContain(
        "yaml-language-server",
      );
    }
  }
});

// The script's types exist only once the resolver writes them: run it, then `tsc` under the
// scaffold's own tsconfig, which is what the author's editor reads.
test("the eval-node scaffold's script typechecks against the declarations it generates", () => {
  const dir = scaffold("--eval-node");
  const cfg = join(dir, ".genroc");
  const before = readFileSync(cfg, "utf8");
  expect(before).toContain("command: [npx, genroc-import]");
  writeFileSync(
    cfg,
    before.replace(
      "command: [npx, genroc-import]",
      `command: [node, ${join(REPO, "eval-node/import.ts")}]`,
    ),
  );
  const r = runCli(bin, ["generate", "-f", join(dir, "definitions/hello.genroc.yaml")], OFFLINE);
  expect(r.exitCode, r.stderr).toBe(0);
  const decls = readFileSync(join(dir, "definitions/greet.genroc.d.ts"), "utf8");
  expect(decls).toContain("who: string");
  expect(decls).toContain("greeting: string");
  const tsc = spawnSync(join(REPO, "tests/node_modules/.bin/tsc"), ["--noEmit", "-p", dir], {
    encoding: "utf8",
  });
  expect(tsc.status, tsc.stdout + tsc.stderr).toBe(0);
}, 60_000);

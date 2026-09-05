#!/usr/bin/env node
// The code-phase resolver: manifest on stdin, `{"code": [...]}` on stdout, non-zero exit
// with the diagnostic on stderr. genctl never parses TypeScript and this never parses YAML
// — the manifest is the whole contract. See specs/source-resolution.md.
//
// Two modes, one binary: "types" writes the declarations an editor needs and returns no
// code; "build" typechecks and bundles. A separate types hook would mean a second `tsc`
// over the same project.

import { spawn } from "node:child_process";
import { existsSync } from "node:fs";
import { access, mkdir, writeFile } from "node:fs/promises";
import { builtinModules } from "node:module";
import { dirname, join, relative, resolve } from "node:path";
import { fileURLToPath } from "node:url";

import commonjs from "@rollup/plugin-commonjs";
import json from "@rollup/plugin-json";
import { nodeResolve } from "@rollup/plugin-node-resolve";
import { rollup, type Plugin } from "rollup";
import ts from "typescript";

type Schema = Record<string, any>;

type Site = {
  task?: string;
  // What the site IS: the task's action type, and the process a child action calls.
  action?: string;
  child?: string;
  // The slot as a path of keys and indices, so nothing has to unescape a JSON Pointer.
  pointer: (string | number)[];
  // Everything after `$<resolver>:`, verbatim. genctl does not read it as a path — that is
  // this resolver's reading of it.
  argument: string;
  // What .genroc asked genctl to type, keyed by the name it chose. This resolver reads Input
  // and Output; which ADDRESS each came from is the config's business, not ours.
  types?: Record<string, Schema>;
};

type ManifestProcess = {
  name: string;
  // The definition's own location, split because a relative argument is relative to the
  // DIRECTORY — joining is ours to do, and this is the base.
  dir: string;
  file: string;
  sites: Site[];
  // Only what this process's fragments reach — a `$ref` in `types` points here.
  $defs?: Record<string, Schema>;
};

type Manifest = {
  mode: "types" | "build";
  root: string;
  processes: ManifestProcess[];
};

function die(message: string): never {
  console.error(message);
  process.exit(1);
}

async function exists(path: string): Promise<boolean> {
  try {
    await access(path);
    return true;
  } catch {
    return false;
  }
}

/** Creates the parent directory, which `.genroc-cache/` relies on: nothing else makes it. */
async function write(path: string, content: string): Promise<void> {
  await mkdir(dirname(path), { recursive: true });
  await writeFile(path, content);
}

// ── JSON Schema → TypeScript ───────────────────────────────────────────────────

/** Only genroc's keyword set is handled; anything else its strict decoder would have
 *  refused before this ran (internal/schema, allowedKeywords). */
function tsType(s: Schema | undefined, used: Set<string>): string {
  if (s === undefined || s === null) return "unknown";
  if (typeof s.$ref === "string") {
    const name = s.$ref.replace(/^#\/\$defs\//, "");
    used.add(name);
    return identifier(name);
  }
  if (Array.isArray(s.enum)) {
    return s.enum.map((v: unknown) => JSON.stringify(v)).join(" | ") || "never";
  }
  if (Array.isArray(s.anyOf))
    return union(s.anyOf.map((a: Schema) => tsType(a, used)));
  if (Array.isArray(s.oneOf))
    return union(s.oneOf.map((a: Schema) => tsType(a, used)));
  if (Array.isArray(s.allOf)) {
    return s.allOf.map((a: Schema) => tsType(a, used)).join(" & ") || "unknown";
  }

  const types: string[] =
    s.type === undefined ? [] : Array.isArray(s.type) ? s.type : [s.type];
  if (types.length === 0) {
    // The top type: `{}` means unknown, not "an empty object". specs/unknown-type.md.
    return s.properties ? objectType(s, used) : "unknown";
  }
  return union(types.map((t) => scalarType(t, s, used)));
}

function scalarType(t: string, s: Schema, used: Set<string>): string {
  switch (t) {
    case "object":
      return objectType(s, used);
    case "array":
      return s.items ? `Array<${tsType(s.items, used)}>` : "unknown[]";
    case "string":
      return "string";
    case "number":
    case "integer":
      return "number";
    case "boolean":
      return "boolean";
    case "null":
      return "null";
    default:
      return "unknown";
  }
}

function objectType(s: Schema, used: Set<string>): string {
  const props: Record<string, Schema> = s.properties ?? {};
  const required = new Set<string>(s.required ?? []);
  const lines: string[] = [];
  for (const [key, sub] of Object.entries(props)) {
    const doc =
      typeof sub.description === "string"
        ? `  /** ${sub.description} */\n`
        : "";
    lines.push(
      `${doc}  ${propKey(key)}${required.has(key) ? "" : "?"}: ${tsType(sub, used)};`,
    );
  }
  if (s.additionalProperties && typeof s.additionalProperties === "object") {
    lines.push(`  [key: string]: ${tsType(s.additionalProperties, used)};`);
  }
  if (lines.length === 0) return "Record<string, unknown>";
  return `{\n${lines.join("\n")}\n}`;
}

function union(parts: string[]): string {
  const seen = [...new Set(parts)];
  return seen.length === 0 ? "unknown" : seen.join(" | ");
}

const IDENT = /^[A-Za-z_$][A-Za-z0-9_$]*$/;
const propKey = (k: string) => (IDENT.test(k) ? k : JSON.stringify(k));
const identifier = (n: string) =>
  IDENT.test(n) ? n : `Def_${n.replace(/[^A-Za-z0-9_$]/g, "_")}`;

function deref(
  s: Schema | undefined,
  defs: Record<string, Schema>,
): Schema | undefined {
  let cur = s;
  for (let i = 0; cur && typeof cur.$ref === "string" && i < 16; i++) {
    cur = defs[cur.$ref.replace(/^#\/\$defs\//, "")];
  }
  return cur;
}

/** Emits one named type per reachable $def rather than inlining: a task output may
 *  reference itself (specs/recursive-type-inference.md) and inlining would not terminate. */
function declarations(at: Located): string {
  const { site, where } = at;
  const defs = where.$defs ?? {};
  const used = new Set<string>();
  const input = tsType(site.types?.Input, used);
  const output = tsType(site.types?.Output, used);

  const emitted: string[] = [];
  const done = new Set<string>();
  while (true) {
    const next = [...used].find((n) => !done.has(n));
    if (next === undefined) break;
    done.add(next);
    const body = tsType(defs[next], used);
    emitted.push(`export type ${identifier(next)} = ${body};`);
  }

  return [
    "// Generated by genroc. Do not edit - regenerate with `genctl types`.",
    `// ${where.name}  (${address(site.pointer)})`,
    "",
    ...emitted,
    emitted.length ? "" : "",
    `export type Input = ${input};`,
    "",
    `export type Output = ${output};`,
    "",
  ].join("\n");
}

/** Keyed by the script's PATH, not the task id: keyed by task, renaming a task would break
 *  the author's `import type` line with the error landing nowhere near the rename. */
function typesPathFor(scriptPath: string): string {
  return scriptPath.replace(/\.[^.\/]+$/, "") + ".genroc.d.ts";
}

// ── typecheck ──────────────────────────────────────────────────────────────────

/** The nearest tsconfig above the script — the one the author's editor already reads. Two
 *  different configs mean a red editor over a clean apply, or the reverse. The walk stops at
 *  the project root: above it is not this project. */
async function nearestTsconfig(
  from: string,
  root: string,
): Promise<string | null> {
  for (let dir = from; ; dir = dirname(dir)) {
    const candidate = join(dir, "tsconfig.json");
    if (await exists(candidate)) return candidate;
    if (dir === root || dirname(dir) === dir) return null;
  }
}

async function typecheck(sites: Located[]): Promise<void> {
  // NOT `.genroc`: that is the project config FILE, and a directory of the same name cannot
  // coexist with it. The suffix is what keeps the scratch area out of its way.
  const dir = join(root, ".genroc-cache");
  await write(join(dir, ".gitignore"), "*\n");

  // One tsc per distinct base config: `extends` takes a single base, so merging two would
  // check each script under the other author's options.
  const groups = new Map<string, Located[]>();
  for (const at of sites) {
    const base = (await nearestTsconfig(dirname(at.file), root)) ?? "";
    const group = groups.get(base);
    if (group) group.push(at);
    else groups.set(base, [at]);
  }

  let n = 0;
  for (const [base, group] of groups) {
    const config: Record<string, unknown> = {
      ...(base ? { extends: relative(dir, base) } : {}),
      compilerOptions: {
        noEmit: true,
        strict: true,
        skipLibCheck: true,
        moduleDetection: "force",
        module: "preserve",
        target: "esnext",
        // `lib` DESCRIBES the realm and is written after `extends` so a base cannot widen it:
        // a worker thread has no document, whatever an author's config claims.
        lib: ["esnext", "webworker"],
        // `types` is the author's, and it is how a script opts into the node globals —
        // the worker realm has them, so refusing the declarations would only lie. With no
        // base config there is nothing to opt in with, so the default stays none.
        ...(base ? {} : { types: [] }),
      },
      files: group.flatMap((s) => [
        relative(dir, s.file),
        relative(dir, typesPathFor(s.file)),
      ]),
      // `files` overrides the base's, but a base `include` survives beside it and would
      // drag the author's whole tree in, to be checked under the worker lib.
      include: [],
    };
    const configPath = join(
      dir,
      groups.size === 1 ? "tsconfig.json" : `tsconfig.${n++}.json`,
    );
    await write(configPath, JSON.stringify(config, null, 2));
    await runTsc(root, configPath);
  }
}

async function runTsc(root: string, configPath: string): Promise<void> {
  const tsc = fileURLToPath(import.meta.resolve("typescript/bin/tsc"));
  const proc = spawn(process.execPath, [tsc, "--noEmit", "-p", configPath], {
    cwd: root,
    stdio: ["ignore", "pipe", "pipe"],
  });
  let out = "";
  let err = "";
  proc.stdout.on("data", (c: Buffer) => (out += c));
  proc.stderr.on("data", (c: Buffer) => (err += c));
  const code = await new Promise<number>((resolve, reject) => {
    proc.on("error", reject);
    proc.on("close", (c) => resolve(c ?? 1));
  });
  if (code !== 0) {
    // tsc reports on stdout; the exit code IS the type check, so this is the diagnostic
    // genctl surfaces and the reason a failed import never produces a string.
    die([out, err].filter(Boolean).join("\n").trimEnd());
  }
}

// ── bundle ─────────────────────────────────────────────────────────────────────

/** Transpiles only. The typecheck above already ran over the author's OWN tsconfig, and a
 *  second opinion from a config they do not control could fail a build they cannot fix. */
const transpile: Plugin = {
  name: "genroc-transpile",
  transform(code, id) {
    if (!id.endsWith(".ts") && !id.endsWith(".tsx")) return null;
    const out = ts.transpileModule(code, {
      fileName: id,
      compilerOptions: {
        target: ts.ScriptTarget.ESNext,
        module: ts.ModuleKind.ESNext,
        verbatimModuleSyntax: false,
        jsx: id.endsWith(".tsx") ? ts.JsxEmit.ReactJSX : undefined,
      },
    });
    return { code: out.outputText, map: out.sourceMapText ?? null };
  },
};

const BUILTIN = new Set([
  ...builtinModules,
  ...builtinModules.map((m) => `node:${m}`),
]);

/** Resolves imports through TYPESCRIPT, using the same config the typecheck ran under, so a
 *  `paths` alias that compiles also bundles. Reimplementing `paths` here would be a second
 *  resolver to keep in agreement with tsc; this one cannot disagree.
 *  A package resolving to a `.d.ts` is declined — that is a type, not the implementation —
 *  which leaves node_modules to nodeResolve. */
function tsResolve(configPath: string | null): Plugin {
  let options: ts.CompilerOptions = {};
  if (configPath) {
    const read = ts.readConfigFile(configPath, ts.sys.readFile);
    options = ts.parseJsonConfigFileContent(
      read.config ?? {},
      ts.sys,
      dirname(configPath),
    ).options;
  }
  return {
    name: "genroc-ts-resolve",
    resolveId(source, importer) {
      if (!importer || BUILTIN.has(source)) return null;
      const { resolvedModule } = ts.resolveModuleName(
        source,
        importer,
        options,
        ts.sys,
      );
      if (!resolvedModule || resolvedModule.isExternalLibraryImport)
        return null;
      return resolvedModule.resolvedFileName.endsWith(".d.ts")
        ? null
        : resolvedModule.resolvedFileName;
    },
  };
}

/** Bundles to a self-contained ES module, which is what the evaluator imports: the default
 *  export it calls is the author's own, so nothing wraps or rewrites the code between the two.
 *  Bundling is entirely the importer's job, so a definition version pins its code forever. */
async function bundle(at: Located): Promise<string> {
  const site = at.site;
  // Builtins are EXTERNALISED as imports the realm resolves natively. Anything else
  // unresolved is a REFUSAL, not an external: rollup's default is to leave it as an import
  // of a module that will not be there, which bundles clean and fails at runtime.
  const built = await rollup({
    input: at.file,
    external: (id) => BUILTIN.has(id),
    plugins: [
      tsResolve(await nearestTsconfig(dirname(at.file), root)),
      nodeResolve({ extensions: [".ts", ".tsx", ".mjs", ".js", ".json"] }),
      commonjs(),
      // A `.json` import is a data file inlined at build time, which the previous bundler
      // did natively; without it rollup hands the JSON to the JS parser.
      json(),
      transpile,
    ],
    onwarn(warning) {
      if (warning.code === "UNRESOLVED_IMPORT") {
        die(
          `${at.file}: cannot resolve ${warning.exporter ?? "an import"} — is it installed?`,
        );
      }
    },
  }).catch((e: unknown) =>
    die(`${at.file}: ${e instanceof Error ? e.message : String(e)}`),
  );

  const { output } = await built.generate({
    format: "es",
    inlineDynamicImports: true,
  });
  await built.close();
  // Refused here rather than in the realm: the evaluator can only report it against a running
  // instance, and the file it names is on this machine.
  if (!output[0].exports.includes("default")) {
    die(`${at.file}: a script must \`export default\` the function to run`);
  }
  return output[0].code;
}

// ── main ───────────────────────────────────────────────────────────────────────

const stdin: string = await new Promise((resolve, reject) => {
  let raw = "";
  process.stdin.setEncoding("utf8");
  process.stdin.on("data", (c) => (raw += c));
  process.stdin.on("end", () => resolve(raw));
  process.stdin.on("error", reject);
});
const manifest = JSON.parse(stdin) as Manifest;
// genctl runs a resolver with the project root as its cwd, so the manifest need not say so.
const root = process.cwd();
if (!manifest || !Array.isArray(manifest.processes))
  die("stdin is not a genroc resolver manifest");

/** Every site with the process it sits in and the file its argument names, in the manifest's own
 *  order — which is the order `code` must answer in. genctl passes the argument verbatim, so
 *  joining it to the definition's directory is this resolver's business: only it knows the
 *  argument is a path at all. */
type Located = { site: Site; where: ManifestProcess; file: string };

/** A pointer as the address it is, so a generated comment or an error can be pasted into
 *  `genctl schema`. A key no identifier can spell is bracketed, as the grammar spells it. */
function address(pointer: (string | number)[]): string {
  return pointer
    .map((seg) =>
      typeof seg === "string" && /^[A-Za-z_][A-Za-z0-9_]*$/.test(seg)
        ? `.${seg}`
        : `[${JSON.stringify(seg)}]`,
    )
    .join("")
    .replace(/^\./, "");
}
const located: Located[] = manifest.processes.flatMap((where) =>
  where.sites.map((site) => ({
    site,
    where,
    file: resolve(where.dir, site.argument),
  })),
);

// genctl is agnostic about what a script is for; the contract that an evaluation request carries
// its module in `code` is THIS resolver's, so it is the one that checks a directive landed there.
// `child` is the shape the scaffold uses — a call to a process that forwards to the evaluator —
// and `external` is the same request made directly.
for (const at of located) {
  // Two halves, from the two places that carry them: the action's KIND is a field, and WHERE in
  // it the directive sits is the pointer. A child call is the shape the scaffold generates — to a
  // process that forwards to the evaluator — and an external task is the same request made
  // directly.
  const kind = at.site.action ?? "";
  const slot = at.site.pointer.slice(-2).join(".");
  if ((kind !== "child" && kind !== "external") || slot !== "input.code") {
    die(
      `${join(at.where.dir, at.where.file)}: ${address(at.site.pointer)}: an evaluated script ` +
        "belongs in the `code` field of a child or external task's input, and this is " +
        `${kind ? `a ${kind} task's ` : ""}\`${slot}\`.`,
    );
  }
}

// genctl no longer stats the argument — it does not know it is a file — so the resolver that
// does is the one that must say when it is not there.
for (const at of located) {
  if (!existsSync(at.file)) {
    die(
      `${join(at.where.dir, at.where.file)}: ${address(at.site.pointer)}: ` +
        `"$import: ${at.site.argument}" names no file (looked at ${at.file})`,
    );
  }
}

// One script at two sites with different input types is a refusal, not a union: the union
// is sound and would typecheck a body that is wrong at one of the sites.
const byPath = new Map<string, Located>();
for (const at of located) {
  const seen = byPath.get(at.file);
  if (
    seen &&
    JSON.stringify(seen.site.types) !== JSON.stringify(at.site.types)
  ) {
    die(
      `${at.file} is imported at ${address(seen.site.pointer)} and ` +
        `${address(at.site.pointer)} with different input types.\n` +
        "Split it into two scripts, or make the two call sites pass the same shape.",
    );
  }
  byPath.set(at.file, at);
}

for (const at of byPath.values()) {
  await write(typesPathFor(at.file), declarations(at));
}

if (manifest.mode === "types") {
  console.log(JSON.stringify(manifest, null, 2));
  process.exit(0);
}

await typecheck([...byPath.values()]);

const code: string[] = [];
for (const at of located) {
  code.push(await bundle(at));
}
process.stdout.write(JSON.stringify({ code }));

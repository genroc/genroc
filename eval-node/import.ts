#!/usr/bin/env node
// The code-phase resolver: manifest on stdin, `{"code": [...]}` on stdout, diagnostic on stderr
// with a non-zero exit (specs/source-resolution.md). Two modes share one `tsc` pass: "types"
// writes declarations only, "build" typechecks and bundles.

import { existsSync } from "node:fs";
import { access, writeFile } from "node:fs/promises";
import { builtinModules } from "node:module";
import { dirname, join, resolve } from "node:path";

import commonjs from "@rollup/plugin-commonjs";
import json from "@rollup/plugin-json";
import { nodeResolve } from "@rollup/plugin-node-resolve";
import { rollup, type Plugin } from "rollup";
import ts from "typescript";

type Schema = Record<string, any>;

type Site = {
  // Which namespace the directive sits in: `process`, `task` or `action`.
  level: "process" | "task" | "action";
  task?: string;
  // What the site IS, at action level: the action's type, and the process a child call names.
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
      `${doc}  ${propKey(key)}${required.has(key) ? "" : "?"}: ${nest(tsType(sub, used))};`,
    );
  }
  if (s.additionalProperties && typeof s.additionalProperties === "object") {
    lines.push(
      `  [key: string]: ${nest(tsType(s.additionalProperties, used))};`,
    );
  }
  if (lines.length === 0) return "Record<string, unknown>";
  return `{\n${lines.join("\n")}\n}`;
}

/** Every line but the first, which is already placed by the property that opens it. */
const nest = (t: string) => t.replace(/\n/g, "\n  ");

function union(parts: string[]): string {
  const seen = [...new Set(parts)];
  return seen.length === 0 ? "unknown" : seen.join(" | ");
}

const IDENT = /^[A-Za-z_$][A-Za-z0-9_$]*$/;
const propKey = (k: string) => (IDENT.test(k) ? k : JSON.stringify(k));
const identifier = (n: string) =>
  IDENT.test(n) ? n : `Def_${n.replace(/[^A-Za-z0-9_$]/g, "_")}`;

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

/** The tsconfig the author's editor reads, so editor and apply agree. Stops at the project
 *  root. */
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
  // One program per distinct base config: `extends` takes a single base, so merging two would
  // check each script under the other author's options.
  const groups = new Map<string, Located[]>();
  for (const at of sites) {
    const base = (await nearestTsconfig(dirname(at.file), root)) ?? "";
    const group = groups.get(base);
    if (group) group.push(at);
    else groups.set(base, [at]);
  }

  const diagnostics: ts.Diagnostic[] = [];
  for (const [base, group] of groups) {
    const config: Record<string, unknown> = {
      ...(base ? { extends: base } : {}),
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
        // `types` is the author's opt-in to node globals, which the realm has. With no base
        // config there is nothing to opt in with, so it stays none.
        ...(base ? {} : { types: [] }),
      },
      files: group.flatMap((s) => [s.file, typesPathFor(s.file)]),
      // `files` overrides the base's, but a base `include` survives beside it and would
      // drag the author's whole tree in, to be checked under the worker lib.
      include: [],
    };
    // Never written; anchors `@types` lookup at the root. Not `tsconfig.json`: that is often the
    // base it extends, and tsc refuses a circular extends.
    const parsed = ts.parseJsonConfigFileContent(
      config,
      ts.sys,
      root,
      undefined,
      join(root, "tsconfig.genroc.json"),
    );
    const program = ts.createProgram({
      rootNames: parsed.fileNames,
      options: parsed.options,
      projectReferences: parsed.projectReferences,
      configFileParsingDiagnostics: parsed.errors,
    });
    diagnostics.push(...parsed.errors, ...ts.getPreEmitDiagnostics(program));
  }

  if (diagnostics.length > 0) {
    // The diagnostics are the whole type check: this is what genctl surfaces, and the reason a
    // failed import never produces a string.
    die(
      ts
        .formatDiagnostics(diagnostics, {
          getCanonicalFileName: (f) => f,
          getCurrentDirectory: () => root,
          getNewLine: () => "\n",
        })
        .trimEnd(),
    );
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

/** Resolves through TYPESCRIPT under the typecheck's config, so a `paths` alias that compiles
 *  also bundles. A `.d.ts` resolution is declined (a type, not code), leaving node_modules to
 *  nodeResolve. */
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

/** A self-contained ES module whose default export is the author's own, unwrapped. Bundling here
 *  means a definition version pins its code forever. */
async function bundle(at: Located): Promise<string> {
  const site = at.site;
  // Builtins are externalised; anything else unresolved is a REFUSAL, since rollup's default
  // leaves an import that bundles clean and fails at runtime.
  const built = await rollup({
    input: at.file,
    external: (id) => BUILTIN.has(id),
    plugins: [
      tsResolve(await nearestTsconfig(dirname(at.file), root)),
      nodeResolve({ extensions: [".ts", ".tsx", ".mjs", ".js", ".json"] }),
      commonjs(),
      // Inlines `.json` imports; without it rollup hands JSON to the JS parser.
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

/** Sites in manifest order, the order `code` must answer in. The argument is joined to the
 *  definition's directory here: only this resolver knows it is a path. */
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

// The `code`-field contract is this resolver's, not genctl's, so it checks the directive landed
// there: in a child call (the scaffold's shape) or an external task's input.
for (const at of located) {
  // The action's kind is a field; where in it the directive sits is the pointer.
  const kind = at.site.level === "action" ? (at.site.action ?? "") : "";
  const slot = at.site.pointer.slice(-2).join(".");
  if ((kind !== "child" && kind !== "external") || slot !== "input.code") {
    die(
      `${join(at.where.dir, at.where.file)}: ${address(at.site.pointer)}: an evaluated script ` +
        "belongs in the `code` field of a child or external task's input, and this is " +
        `${kind ? `a ${kind} task's ` : ""}\`${slot}\`.`,
    );
  }
}

// genctl does not know the argument is a file, so this resolver reports a missing one.
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
  await writeFile(typesPathFor(at.file), declarations(at));
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

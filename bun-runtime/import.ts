// The code-phase resolver: manifest on stdin, `{"code": [...]}` on stdout, non-zero exit
// with the diagnostic on stderr. genctl never parses TypeScript and this never parses YAML
// — the manifest is the whole contract. See specs/source-resolution.md.
//
// Two modes, one binary: "types" writes the declarations an editor needs and returns no
// code; "build" typechecks and bundles. A separate types hook would mean a second `tsc`
// over the same project.

import { dirname, join, relative } from "node:path";

type Schema = Record<string, any>;

type Site = {
  resolver: string;
  process: string;
  task?: string;
  pointer: string;
  path: string;
  input?: Schema;
  output?: Schema;
};

type Manifest = {
  mode: "types" | "build";
  root: string;
  schemas: Record<string, Schema>;
  sites: Site[];
};

function die(message: string): never {
  console.error(message);
  process.exit(1);
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
  if (Array.isArray(s.anyOf)) return union(s.anyOf.map((a: Schema) => tsType(a, used)));
  if (Array.isArray(s.oneOf)) return union(s.oneOf.map((a: Schema) => tsType(a, used)));
  if (Array.isArray(s.allOf)) {
    return s.allOf.map((a: Schema) => tsType(a, used)).join(" & ") || "unknown";
  }

  const types: string[] = s.type === undefined ? [] : Array.isArray(s.type) ? s.type : [s.type];
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
    const doc = typeof sub.description === "string" ? `  /** ${sub.description} */\n` : "";
    lines.push(`${doc}  ${propKey(key)}${required.has(key) ? "" : "?"}: ${tsType(sub, used)};`);
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
const identifier = (n: string) => (IDENT.test(n) ? n : `Def_${n.replace(/[^A-Za-z0-9_$]/g, "_")}`);

function deref(s: Schema | undefined, defs: Record<string, Schema>): Schema | undefined {
  let cur = s;
  for (let i = 0; cur && typeof cur.$ref === "string" && i < 16; i++) {
    cur = defs[cur.$ref.replace(/^#\/\$defs\//, "")];
  }
  return cur;
}

/** The manifest's `input` is the type of the whole ACTION input — for /eval that is
 *  `{code, input, timeout_ms, …}`, and only its `input` field is bound as the script's
 *  parameter. genroc cannot know that; this file owns the evaluator's wire contract, so
 *  the navigation belongs here rather than in genctl. */
function scriptInput(site: Site, defs: Record<string, Schema>): Schema | undefined {
  const action = deref(site.input, defs);
  const bound = action?.properties?.input;
  return bound ?? site.input;
}

/** Emits one named type per reachable $def rather than inlining: a task output may
 *  reference itself (specs/recursive-type-inference.md) and inlining would not terminate. */
function declarations(site: Site, defs: Record<string, Schema>): string {
  const used = new Set<string>();
  const input = tsType(scriptInput(site, defs), used);
  const output = tsType(site.output, used);

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
    `// ${site.process}${site.task ? ` / ${site.task}` : ""}  (${site.pointer})`,
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
async function nearestTsconfig(from: string, root: string): Promise<string | null> {
  for (let dir = from; ; dir = dirname(dir)) {
    const candidate = join(dir, "tsconfig.json");
    if (await Bun.file(candidate).exists()) return candidate;
    if (dir === root || dirname(dir) === dir) return null;
  }
}

async function typecheck(root: string, sites: Site[]): Promise<void> {
  const dir = join(root, ".genroc");
  await Bun.write(join(dir, ".gitignore"), "*\n");

  // One tsc per distinct base config: `extends` takes a single base, so merging two would
  // check each script under the other author's options.
  const groups = new Map<string, Site[]>();
  for (const site of sites) {
    const base = (await nearestTsconfig(dirname(site.path), root)) ?? "";
    const group = groups.get(base);
    if (group) group.push(site);
    else groups.set(base, [site]);
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
        // a Bun worker has no document, whatever an author's config claims.
        lib: ["esnext", "webworker"],
        // `types` is the author's, and it is how a script opts into node and Bun globals —
        // the worker realm has them, so refusing the declarations would only lie. With no
        // base config there is nothing to opt in with, so the default stays none.
        ...(base ? {} : { types: [] }),
      },
      files: group.flatMap((s) => [relative(dir, s.path), relative(dir, typesPathFor(s.path))]),
      // `files` overrides the base's, but a base `include` survives beside it and would
      // drag the author's whole tree in, to be checked under the worker lib.
      include: [],
    };
    const configPath = join(dir, groups.size === 1 ? "tsconfig.json" : `tsconfig.${n++}.json`);
    await Bun.write(configPath, JSON.stringify(config, null, 2));
    await runTsc(root, configPath);
  }
}

async function runTsc(root: string, configPath: string): Promise<void> {
  const tsc = Bun.fileURLToPath(import.meta.resolve("typescript/bin/tsc"));
  const proc = Bun.spawn(["bun", tsc, "--noEmit", "-p", configPath], {
    cwd: root,
    stdout: "pipe",
    stderr: "pipe",
  });
  const [out, err, code] = await Promise.all([
    new Response(proc.stdout).text(),
    new Response(proc.stderr).text(),
    proc.exited,
  ]);
  if (code !== 0) {
    // tsc reports on stdout; the exit code IS the type check, so this is the diagnostic
    // genctl surfaces and the reason a failed import never produces a string.
    die([out, err].filter(Boolean).join("\n").trimEnd());
  }
}

// ── bundle ─────────────────────────────────────────────────────────────────────

/** Bundles to CJS and wraps it as an async function BODY, which is what /eval compiles.
 *  The runtime stays unchanged: bundling is entirely the importer's job, and the string it
 *  produces is self-contained, so a definition version pins its code forever. */
async function bundle(site: Site): Promise<string> {
  const built = await Bun.build({
    entrypoints: [site.path],
    format: "cjs",
    // Builtins are EXTERNALISED as `require` calls that worker.ts satisfies. Under
    // `browser` they were rewritten to `{}` instead — a script importing `node:fs` bundled
    // clean and then failed at runtime with no diagnostic. Not `bun`: that emits Bun's
    // `@bun-cjs` form, a function expression nothing here calls.
    target: "node",
    minify: false,
  });
  if (!built.success) {
    die(`${site.path}: ${built.logs.map((l) => String(l)).join("\n")}`);
  }
  const cjs = await built.outputs[0]!.text();
  return [
    "var module = { exports: {} }, exports = module.exports;",
    cjs,
    "var __genroc_main = module.exports.default ?? module.exports;",
    'if (typeof __genroc_main !== "function") {',
    `  throw new Error(${JSON.stringify(`${site.path} has no default export function`)});`,
    "}",
    "return await __genroc_main(input);",
  ].join("\n");
}

// ── main ───────────────────────────────────────────────────────────────────────

const manifest = (await Bun.stdin.json()) as Manifest;
if (!manifest || !Array.isArray(manifest.sites)) die("stdin is not a genroc resolver manifest");

// One script at two sites with different input types is a refusal, not a union: the union
// is sound and would typecheck a body that is wrong at one of the sites.
const byPath = new Map<string, Site>();
for (const site of manifest.sites) {
  const seen = byPath.get(site.path);
  const defsOf = (x: Site) => (manifest.schemas[x.process]?.$defs ?? {}) as Record<string, Schema>;
  if (
    seen &&
    JSON.stringify(scriptInput(seen, defsOf(seen))) !==
      JSON.stringify(scriptInput(site, defsOf(site)))
  ) {
    die(
      `${site.path} is imported at ${seen.pointer} and ${site.pointer} with different input types.\n` +
        "Split it into two scripts, or make the two call sites pass the same shape.",
    );
  }
  byPath.set(site.path, site);
}

for (const site of byPath.values()) {
  const defs = (manifest.schemas[site.process]?.$defs ?? {}) as Record<string, Schema>;
  await Bun.write(typesPathFor(site.path), declarations(site, defs));
}

if (manifest.mode === "types") {
  process.exit(0);
}

await typecheck(manifest.root, [...byPath.values()]);

const code: string[] = [];
for (const site of manifest.sites) {
  code.push(await bundle(site));
}
process.stdout.write(JSON.stringify({ code }));

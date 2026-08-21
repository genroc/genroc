# Source resolution: a definition source file is not a definition

Status: **PROPOSAL 2026-08-20; the code phase BUILT 2026-08-21.** What ships is the
project config, value-position `$<resolver>: <path>` directives, the batched manifest,
`genctl types`, and `bun-runtime/import.ts` as the first resolver
(`cmd/genctl/sources.go`, `tests/cli/imports_test.ts`). **Unbuilt: the structural phase**
— no phase-1 resolver exists, `$infer` is not implemented, and `phase: structural` in a
config is refused rather than ignored. The key-position merge form (§Directive syntax
mentions only the value position) is also unbuilt.

[script-tasks.md](script-tasks.md) argued for a single-phase import directive; that section
is superseded by this doc, which owns the resolution model outright. The TypeScript
toolchain is one *client* of what follows — nothing here is about TypeScript.

Names the config file `genroc.yaml` rather than extending `genctl config`, and calls itself
*source resolution* rather than "config and imports", because `config` already denotes the
runtime `config.*` namespace resolved from `GENROC_<proc>_` every tick.

## Thesis

A definition **source file** is resolved into a **definition** by binaries the project
registers. genroc contributes the phase rule, the directive syntax and the manifest; a
bundler, a type generator and a YAML fragment loader are all clients of one mechanism.

Two properties fall out, and they are the reason for the shape:

- **A stored definition cannot hold code that failed to typecheck.** The typechecker is the
  resolver's exit code, and a failed resolution never produces the string. An ordering
  property, not an enforced rule — nothing checks it, and nothing needs to.
- **The server has no resolver.** Resolution is source-level and client-side, so no
  directive reaches the wire to be tricked into executing. That null implementation *is* the
  security answer: the mechanism is Makefile-tier — a binary named in your own repo, on your
  own machine — and needs no defense beyond not building one into the server.

It also keeps the property worth keeping: a definition version pins byte-identical code
forever. No runtime fetch, no dependency drift, an old instance finishes against the code it
started with. Do not later trade this for loading a script from a URL.

## The problem: resolution needs types, types need resolution

A script wants generated `Input`/`Output` declarations to typecheck against. Those come from
inference over the definition. The definition is what resolution produces. Circular.

It breaks on one fact: **the code string is opaque to inference.** Nothing genroc infers
anywhere reads it. So resolution splits at exactly the line where that stops being true.

## The two phases

Named by permission, never by content — what a resolver *may do* is what decides its phase:

- **Phase 1 — `structural`.** May change what the typechecker sees. Runs before validation.
  Output may be any value: a YAML fragment, a JSON Schema, a number.
- **Phase 2 — `code`.** May not. Runs after validation, with inferred types in hand. Output
  **must be a string**, enforced by genctl — which is what makes "cannot invalidate phase 1"
  structural rather than a promise a resolver author has to keep.

The sequence, per `genctl apply`:

1. Parse the source files. Phase-1 directives resolve; phase-2 sites hold a placeholder.
2. `POST /definitions/validate` → a `SchemaFile` per definition.
3. Phase 2, batched — manifest in, code strings out. One call per registered phase-2
   resolver, carrying every site that named it.
4. Splice each string into its slot, `$`-escaped (below).
5. `POST /definitions`.

`genctl validate` stops after step 3 in `types` mode, and never reaches step 5.

### Two roundtrips, and why it is not "validate then apply"

The first call is a **type query**, not a verdict: apply revalidates, so the definition that
is checked for real is the one that is stored. Stating it the other way — validate, then
mutate, then apply — describes a system where what you checked is not what you shipped, and
invites someone to "optimise" the second validation away.

### Why the placeholder is sound

Inference collapses every literal to its base type, so the placeholder and the real code are
*indistinguishable to it*: `"sent"` infers as `string` today.
[literal-types.md](literal-types.md) changes that — both sides become `enum`, both still fit
a `string` target, so nothing breaks, but the guarantee drops from proof to argument. **This
paragraph is the signal to re-read when literal types land.**

### Why two phases and not N

Two phases with a rule about what each may do is a design. A configurable N-stage pipeline
is a plugin system with no rule: nothing says whether stage 4's output can invalidate stage
2's validation, so the answer gets discovered per resolver, in production, once.

## The project config

`genroc.yaml`, discovered upward from each source file. Deliberately **not**
`os.UserConfigDir()`, where `genctl config` writes `server`: which bundler builds this repo
is the repo's property and belongs in the repo; the server URL is the operator's and does
not. Different owners, different lifetimes, different files.

```yaml
resolvers:
  import: { phase: code,       ext: .ts, command: [bun, run, tools/genroc-import.ts] }
  infer:  { phase: structural, ext: .ts, command: [bun, run, tools/genroc-infer.ts] }
```

The directive names the resolver, so `ext` is **an assertion, not a dispatch key**: it makes
a `.py` path handed to the TypeScript toolchain fail at genctl with a sentence, instead of
failing inside `tsc` with a stack.

## Directive syntax

    code: "$import: ./summarize.ts"

General form `"$<resolver>: <path>"`, path relative to the file the directive appears in.
`$$import:` is a literal, by the escape rule [typed-values.md](typed-values.md) already
defines — no new grammar.

A YAML `!import` tag was rejected, and not for the reason it first looks like. Reading it in
Go is *easier*: the tag survives on the `yaml.Node`, `Decode` on the scalar yields the bare
path, and `node.Tag == "!import"` is a fact from the parser rather than a prefix match on a
value. It also needs no escape rule, and extends to options (`!import {path, mode}`) where a
string form would smuggle a second grammar into a string.

It loses on reach. **A tag is a parser-level fact; a `$` prefix is an application-level
one** — and resolution is an application concern that happens in exactly one program, so
encoding it in the syntax hands an opinion about it to every YAML reader that ever touches a
definition source file. This repo has three besides genctl: `js-yaml` throws outright
(`unknown scalar tag !<!import>`) in `tests/helpers/compat-fixtures.ts` and four
`examples_*_test.ts` files, Bun's native `.yaml` import backs `tests/bench/run.ts`, and
`yaml-language-server` needs `yaml.customTags` configured per workspace before it stops
flagging the node. `.json` sources cannot carry a tag at all, and `readFile` accepts them.

A string reaches all of them untouched. The tag's honesty is bought by making the directive
visible to every tool, when the property wanted is the opposite.

## Escaping on splice — the thing the feature is *for*

[bun-runtime/README.md](../bun-runtime/README.md) promises the directive removes `$${`. It
does not come free. Splice a file's text in as a plain string and the Shape layer reads
`${` in it exactly as before. **genctl doubles every `$` on splice** — `escapeDollars`.

Not "escape `${` and a leading `$:`", which is the rule that suggests itself and is wrong:
`scanTemplate` collapses `$$` to a literal `$` **unconditionally**, in every string leaf,
whether or not it holds a marker — and every leaf reaches it, because `internal/shape`
calls `template.Get` on all of them. So a script containing a literal `$$` would be
corrupted by the selective rule. Doubling round-trips any byte sequence and has no cases.

Recorded because omitting it fails silently — the definition applies, the script runs, and
the interpolation that was supposed to be JavaScript was read by genroc.

## The manifest — phase 2's interface

One call per phase-2 resolver per apply, **not one per site**: N scripts must not mean N
`tsc` invocations, and a shared project is what a type checker wants anyway.

stdin:

```jsonc
{
  "mode": "build",                       // or "types" — see below
  "root": "/abs/path/to/project",        // the directory holding genroc.yaml; also the cwd
  "schemas": {
    "weather-logger": { /* SchemaFile: process_input, process_output, tasks{}, $defs */ }
  },
  "sites": [
    {
      "process": "weather-logger",
      "task": "summarize",
      "pointer": "/tasks/5/action/input/code",   // JSON pointer to the slot
      "path": "/abs/path/to/summarize.ts",       // absolute; genctl resolved it
      "input":  { "$ref": "#/$defs/summarize_input" },
      "output": { /* result_schema, or responses.200 */ }
    }
  ]
}
```

stdout: `{"code": ["<string>", …]}` — parallel to `sites`, by position. Non-zero exit aborts
the apply with stderr as the diagnostic; stdout carries nothing else.

Exact rules, each removing a convention someone would otherwise have to guess:

- **`path` is absolute.** genctl resolved it against the source file that held the
  directive; sites in one call can come from different files, so no single cwd would do.
  The subprocess cwd is `root` instead, which is what a `tsconfig` wants.
- **`pointer` identifies the site**, not `task` — a task may hold two directives.
- **`$ref` in `input`/`output` points into `schemas[<process>].$defs`**, that process's pool
  and no other.
- **`input` is `SchemaFile.Tasks[<task>].Input`** — the inferred type of the action's input
  shape, already computed by `buildInputs` ([infer.go:16](../internal/validation/infer.go#L16))
  and already returned by `/definitions/validate`. **No server change was required.**
- **`input` is the whole action input, not the resolver's parameter type.** For `/eval` that
  is `{code, input, timeout_ms, …}`, of which only `input` is bound as the script's
  argument. genroc cannot know which slot a resolver's runtime binds, so **the resolver
  navigates** — `scriptInput` in `import.ts` does it, and it is the right place because that
  file owns the evaluator's wire contract. Getting this backwards is not a subtle failure:
  the first build reported `Property 'amount' does not exist on type 'price_input'`.
- **`output` is declared, never inferred**: `result_schema` on a child, `responses.200` on a
  fetch, absent when the task declares neither. A generator with no `output` emits the top
  type.

### genctl passes the sites; a resolver never re-detects them

Walking the definition for directives is easy — that is not the reason. The reason is
**drift**: two parsers for one syntax, and the day they disagree you get types for a file
nobody imports, or an import with no types, and it presents as a `tsconfig` fault. genctl
already parses the syntax in order to resolve it, so it is the one authority and hands over
what it found.

### `mode: "types"`

Writes the type files and returns no code. `genctl types -f process.yaml` is that call, and
it exists because **the editor needs the declarations to exist before an apply ever runs** —
without it the author's file is red until they apply once, which is the wrong order.

Same binary, same manifest, two modes: a separate types *hook* would mean a second
subprocess and a second `tsc` over the same project.

## `$infer` — the other direction

    result_schema: "$infer: ./summarize.ts"

Phase 1. Extracts the script's return type into a JSON Schema, so the definition picks the
type *up* instead of handing it *down*.

**It requires an explicitly annotated return type.** A file that is both `$infer`'d for its
output and `$import`ed for its code needs its return type extracted in phase 1 — before
phase 2 has generated the `Input` type that TypeScript's own return inference would read
(`function f(input) { return input.x }`). The annotation cuts the cycle and reduces
extraction to reading a declaration. Refuse the unannotated case by name; do not fall back
to whole-program inference, which is how the cycle comes back.

Why it is safe: resolution is source-level, so the **stored** definition carries the
extracted schema. [`genctl compat`](../internal/validation/compat.go#L169) then sees a
changed `.ts` return type as a real contract break — the type escaped into the definition
and versioning still works on it. A design where the schema stayed in the `.ts` file would
lose exactly that.

What it is, stated so it is not built twice: [unknown-type.md](unknown-type.md)'s unbuilt
**Infer** result-typing mode, reached at author time. It skips cross-process resolution,
cycle handling at process granularity, `(process, version)` memoization and the
registration-ordering rule — most of the payoff, none of the engine build. That makes it
also the argument for not scheduling the engine-side version.

## What a type generator owes

Client-side detail, recorded because each is a silent failure rather than a compile error:

- **One named interface per `$def`, never inlined.**
  [recursive-type-inference.md](recursive-type-inference.md) is implemented, so a task output
  can reference itself and an expanding generator does not terminate. Named TypeScript
  interfaces handle recursion natively — it is the one place TS is a better target than the
  schema.
- **Key type files by the script's path, not the task id.** `summarize.ts` →
  `summarize.genroc.d.ts` beside it. Keyed by task, renaming a task breaks the author's
  `import type` line, and the error lands nowhere near the rename.
- **One script at two sites with different input types is an error, not a union.** The union
  is sound and would typecheck a body that is wrong at one of the sites. Same script, same
  type, is reuse — allow it.

## This is not the plugin door

[custom-tasks.md](custom-tasks.md) rules out dynamically loaded code, and a resolver
registry looks like precisely that. It is not: resolvers run at author time, on the author's
machine, named by a file in the author's own repo, and produce bytes that reach the wire as
ordinary data. The engine's no-plugins guarantee is spent nowhere.

Stated explicitly because unstated it reads as a violation the first time someone audits it.

## Open questions

- **Where the types are computed.** The server, because `/definitions/validate` already
  returns them and genctl stays a thin gateway. But `Generate` and `TaskContexts` are pure
  over `*model.ProcessDefinition` — only `ValidateChildProcessRefs` needs the DB — so genctl
  could compute them locally and drop to one roundtrip. **The signal to flip is the editor
  loop**, not apply: `genctl types` on every edit, against a server that may be down, is a
  worse property than a genctl that links `internal/validation`. Deferred because the phase
  design is identical either way and the swap is a refactor, not a redesign.
- **Stale generated files.** Nothing removes a `.d.ts` whose script was deleted. Cheapest
  fix if it matters: the resolver prints what it wrote and genctl reports it.
- **Caching.** Resolvers run on every apply. Content-hash the manifest if it becomes slow —
  not before, and never in a way that can serve a stale string.
- **Phase-1 batching.** Per-file, file-in/stdout-out, since there is no shared project to
  check. If `$infer` sites grow it takes the same manifest with `mode: "infer"`.
- **The structural phase.** Nothing registers one yet. `findProjectConfig` refuses
  `phase: structural` outright rather than accepting and ignoring it, so the day it lands
  the config that anticipated it fails loudly instead of having silently done nothing.

# Source resolution: a definition source file is not a definition

Status: **PROPOSAL 2026-08-20; the code phase BUILT 2026-08-21; the types moved into genctl
2026-09-04.** What ships is the project config, value-position `$<resolver>: <path>`
directives, the batched manifest, `genctl types`, and `eval-node/import.ts` as the first
resolver (`cmd/genctl/sources.go`, `tests/cli/imports_test.ts`). **Unbuilt: the structural phase**
— no phase-1 resolver exists, `$infer` is not implemented, and `phase: structural` in a
config is refused rather than ignored. The key-position merge form (§Directive syntax
mentions only the value position) is also unbuilt.

[script-tasks.md](script-tasks.md) argued for a single-phase import directive; that section
is superseded by this doc, which owns the resolution model outright. The TypeScript
toolchain is one *client* of what follows — nothing here is about TypeScript.

Names the config file `.genroc` rather than extending `genctl config`, and calls itself
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
2. `validation.Generate` **in genctl** → a `SchemaFile` per definition that carries a site.
3. Phase 2, batched — manifest in, code strings out. One call per registered phase-2
   resolver, carrying every site that named it.
4. Splice each string into its slot, `$`-escaped (below).
5. `POST /definitions`.

`genctl validate` stops after step 3 in `types` mode, and never reaches step 5. `genctl
compat -f` runs 1–4 and then compares instead of storing: a stored version holds the resolved
string, so an unresolved directive would compare as a literal against it and every imported
site would read as changed.

### One roundtrip: genctl computes the types, the server decides validity

**Built 2026-09-04**, replacing a `POST /definitions/validate` at step 2 — the shape the
Open question below argued for, and the reason is the one it named: `genctl types` runs on
every edit, and an editor loop that stops when the server is down is a worse property than a
genctl that links `internal/validation`. `Generate` is pure over a definition, so the query
was never a query.

The division that falls out is the one to keep: **step 2 produces types, step 5 is the
verdict.** Step 2 therefore runs neither the strict decode, nor `Validate`, nor
`ValidateChildProcessRefs` — a definition that is invalid for a reason inference does not
care about is refused by the apply, with the server's message, and a second gatekeeper in
genctl only creates a pair to keep in agreement. It types **only the definitions that carry
a directive**, so one broken file cannot stop the generation for every other script.

What this trades: the types now follow genctl's build rather than the server's. A genctl
older than its server infers from older rules — and the apply that follows is what refuses
the result, so the failure is loud rather than a script typechecked against a lie.

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

`.genroc`, discovered upward from each source file. Deliberately **not**
`os.UserConfigDir()`, where `genctl config` writes `server`: which bundler builds this repo
is the repo's property and belongs in the repo; the server URL is the operator's and does
not. Different owners, different lifetimes, different files.

```yaml
resolvers:
  import:
    phase: code
    ext: .ts
    command: [node, tools/genroc-import.ts]
    types: { Input: task.action.input.input, Output: task.action.result }
  infer:  { phase: structural, ext: .ts, command: [node, tools/genroc-infer.ts] }
```

The directive names the resolver, so `ext` is **an assertion, not a dispatch key**: it makes
a `.py` path handed to the TypeScript toolchain fail at genctl with a sentence, instead of
failing inside `tsc` with a stack.

**`types` is what the resolver wants typed, and genctl decides none of it.** The names are the
resolver's; the addresses are [`genctl schema type`](schema-command.md)'s, prefixed by the
**frame** they are relative to. Absent means it wants none, which is most resolvers.

| frame | resolves against | where there is none |
|---|---|---|
| `task.…` | `tasks.<id>` — the task the directive sits in | `null` |
| `process.…` | the definition: `input`, `output`, `raises`, `tasks` | resolves |

So `task.action.input.input` is the argument an evaluator binds out of the action's input, and
`task.action.result` is what it hands back — neither naming the action's TYPE, so one config
serves a `child` task in one definition and an `external` one in the next.

**The frame is named rather than inferred from the site**, because a directive can sit inside a
task or outside one and more than one frame carries an `input`: without it, `input.input`
answered with the action's argument at one site and the PROCESS input at the next — a
plausible-looking type from an unrelated schema, which is worse than no answer. An address naming
no frame is refused when the config is read, so a typo fails where it is written instead of
resolving somewhere unintended.

A requested type that is **not at a site comes back `null`, and is not fatal**. Null rather than
absent for the reason `raises: {code: null}` is a declaration: the name was asked for and nothing
is there, which is a different fact from not being asked for. Not fatal because what a site can
answer varies legitimately — a script taking no argument has no `input.input` — and whether that
matters is the resolver's to decide.

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
`examples_*_test.ts` files, `js-yaml` backs `tests/bench/run.ts`, and
`yaml-language-server` needs `yaml.customTags` configured per workspace before it stops
flagging the node. `.json` sources cannot carry a tag at all, and `readFile` accepts them.

A string reaches all of them untouched. The tag's honesty is bought by making the directive
visible to every tool, when the property wanted is the opposite.

## Escaping on splice — the thing the feature is *for*

[eval-node/README.md](../eval-node/README.md) promises the directive removes `$${`. It
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
  "processes": [
    {
      "name": "weather-logger",
      "dir":  "/abs/path/to/defs",       // the definition's directory: an argument's base
      "file": "weather.genroc.yaml",
      "sites": [
        {
          "task": "summarize", "action": "child", "child": "script-node",
          "pointer": ["tasks", "summarize", "action", "input", "code"],  // shaped like the YAML
          "argument": "./summarize.ts",                        // verbatim, after `$import:`
          "types": {                                           // what `types` in .genroc asked for
            "Input":  { "$ref": "#/$defs/input" },             // task.action.input.input
            "Output": { "type": "object", "properties": { "fee": { "type": "number" } },
                        "required": ["fee"] },                 // task.action.result, declared
            "Absent": null                                     // asked for, nothing at that address
          }
        }
      ],
      "$defs": { "input": { /* … */ } }    // only what the fragments above reach
    }
  ]
}
```

stdout: `{"code": ["<string>", …]}` — parallel to the manifest's own site order, by position. Non-zero exit aborts
the apply with stderr as the diagnostic; stdout carries nothing else.

Exact rules, each removing a convention someone would otherwise have to guess:

- **The argument travels verbatim.** genctl reads what follows `$<resolver>:` as a string and
  nothing more — not a path, and certainly not a file: a resolver may take a URL, a package
  name, or an identifier. `dir` is the base a resolver JOINS to when its argument is relative,
  and it is absolute because definitions in one call come from different directories, so no
  single cwd reads them all. The consequence to accept: a missing file is refused by the
  resolver rather than by genctl, which is where the knowledge that it is a file lives. What
  genctl still checks is the assertion the config made — `ext`, a suffix test on the argument.
- **There is no `root`.** The subprocess cwd IS the project root, so a resolver that wants it
  reads its own; a field repeating it would be a second thing to keep true.
- **`pointer` identifies the site**, not `task` — a task may hold two directives. It is shaped
  like the DEFINITION: the task by **id** rather than by index, then the document's own keys,
  `action` included — `["tasks", "price", "action", "input", "code"]`, not
  `/tasks/0/action/input/code`. Where the slot it names has a type, that IS its type address:
  `genctl schema type <process> tasks.price.action.input.code` answers about the very slot the
  directive fills, because both spaces name a slot the way the definition does
  (schema-command.md §2). Where the slot has none — `url`, a `raise` message — the pointer is
  just a location, which is all a pointer promises.
- **What the site IS travels beside it, as fields**: `action` (the action's type) and `child`
  (the process a child action calls). A task has exactly one action, so putting its type in the
  path would name no choice — and a resolver that wants to check where it landed would otherwise
  have to read the definition, which the manifest no longer carries.
- An ARRAY rather than an RFC 6901 string: a string makes every recipient unescape `~0`/`~1`,
  and cannot tell the object key `"0"` from index `0`. object-store.md made the same choice for
  the same reason.
- **A process is the outer structure, and a site sits under the definition it is in.** There is
  no `process` field to join on, and the pool a fragment resolves against is printed beside it.
- **`$defs` is narrowed to what the fragments reach.** It used to be the whole `SchemaFile`, then
  the whole pool; both shipped definitions no resolver opened, and the SchemaFile also carried a
  second copy of every fragment. Refs survive the narrowing rather than being inlined, because a
  task output may reference itself. A resolver that wants more than it asked for asks for more —
  `process` is an address, and answers with the whole type view.
- **`code` answers in the manifest's own order**: processes as listed, sites within each as
  listed. The splice reads it by position.
- **A fragment is the type view's answer at the address `types` named**, which is inference's —
  `SchemaFile` computed by `buildInputs` and friends
  ([infer.go:16](../internal/validation/infer.go#L16)). **No server change was ever required**,
  and since the local pass it is not a server's answer at all.
- **Only processes that have sites appear**, not every definition in the apply.
- **The resolver no longer navigates.** genctl used to send the whole action input — for `/eval`
  that is `{code, input, timeout_ms, …}` — and `import.ts` picked `input` out of it by hand,
  because genroc cannot know which slot an evaluator binds. It still cannot; the difference is
  that the resolver now SAYS which, as an address, and `scriptInput` is deleted. Getting this
  backwards was not a subtle failure: the first build reported `Property 'amount' does not exist
  on type 'price_input'`.
- **What is declared stays declared.** `result` is `result_schema` on a child, the accepted
  `responses` on a fetch, and absent where the task declares neither — inference never invents
  it from the script, which is the direction `$infer` runs.

### genctl passes the sites; a resolver never re-detects them

Walking the definition for directives is easy — that is not the reason. The reason is
**drift**: two parsers for one syntax, and the day they disagree you get types for a file
nobody imports, or an import with no types, and it presents as a `tsconfig` fault. genctl
already parses the syntax in order to resolve it, so it is the one authority and hands over
what it found.

### `mode: "types"`

Writes the type files and returns no code. `genctl types -f process.yaml` is that call, and
it exists because **the editor needs the declarations to exist before an apply ever runs** —
without it the author's file is red until they apply once, which is the wrong order. It
reaches no server, which is what lets it run on every edit.

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

- ~~**Where the types are computed.**~~ **Settled 2026-09-04: in genctl.** See §One
  roundtrip above; the phase design was identical either way, as this question predicted.
- **Stale generated files.** Nothing removes a `.d.ts` whose script was deleted. Cheapest
  fix if it matters: the resolver prints what it wrote and genctl reports it.
- **Caching.** Resolvers run on every apply. Content-hash the manifest if it becomes slow —
  not before, and never in a way that can serve a stale string.
- **Phase-1 batching.** Per-file, file-in/stdout-out, since there is no shared project to
  check. If `$infer` sites grow it takes the same manifest with `mode: "infer"`.
- **The structural phase.** Nothing registers one yet. `findProjectConfig` refuses
  `phase: structural` outright rather than accepting and ignoring it, so the day it lands
  the config that anticipated it fails loudly instead of having silently done nothing.

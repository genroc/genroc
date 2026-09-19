# Source resolution: a definition source file is not a definition

Status: **PROPOSAL 2026-08-20; the code phase BUILT 2026-08-21; the types moved into genctl
2026-09-04.** What ships is the project config, value-position `$<resolver>: <path>`
directives, the batched manifest, `genctl types`, and `eval-node/import.ts` as the first
resolver (`internal/sources/sources.go`, `tests/cli/imports_test.ts`).

**The structural phase, the spread form and `$process` BUILT 2026-09-17**
(`internal/sources/structural.go`, `tests/cli/spread_test.ts`), along with the config reshape this
doc describes: `resolvers` is an ordered first-match list and `ext` a suffix list. Still
unbuilt: **`$infer`**, and any structural resolver that is not the built-in — a registered one
is refused by name rather than run. Recursive spread typing is **declined, not missing**
(§Ordering).

[script-tasks.md](script-tasks.md) argued for a single-phase import directive; that section
is superseded by this doc, which owns the resolution model outright. The TypeScript
toolchain is one *client* of what follows — nothing here is about TypeScript.

Names the config file `.genroc` rather than extending `genctl config`, and calls itself
*source resolution* rather than "config and imports", because `config` already denotes the
runtime `config.*` namespace resolved from `GENROC_<proc>_` every tick.

## The editor's guess about a path

An argument is passed to its resolver **verbatim**: genroc does not know it is a path, and
`findSites` neither resolves nor stats it. That stays true. What the EDITOR does with it is a
separate, lower-stakes question, and the answer is a guess in the shell's shape — an argument
beginning `/`, `./` or `../` is one someone is typing a path into, so files and folders are
offered and the written path is clickable. A lone `.` does not qualify even though it begins
two spellings that do: it is a completion trigger, so accepting it listed a directory the
instant anyone typed a dot. Anything else is left alone, because a resolver's argument may be
a package name, a URL or a key, and offering files there would invent a meaning.

An argument with **nothing typed yet** is offered paths too, which the rule above cannot cover
on its own: there is no meaning to invent yet, and without it the first keystroke has to be
guessed blind. What is offered inserts a **`./` prefix** where the typed text has no directory
part — explicit is clearer, and a bare name is the one spelling a resolver may read as something
that is not a path at all.

The suffix filter is the registry's own (`resolvers[].ext`, plus the implicit `$process`), read
through one accessor so the editor cannot disagree with `matchResolver` about what a resolver
takes. A name the registry does not carry is left unfiltered rather than answered with nothing:
the registry is the reader's to fix, and an empty list at the moment they are typing reads as a
broken server. **BUILT 2026-09-18**, `internal/lsp/paths.go`, `tests/lsp/directive_path_test.ts`.

Hovering a directive shows what it is worth a popup for, as the YAML its author would have
written. A STRUCTURAL directive — `$process` or a registered one — shows what it yields: the
value it fills or spreads, from `sources.StructuralValueAt`, the pass's own call for one site, so
the popup cannot differ from what an apply merges. A key the mapping around a spread writes
itself stays in the picture with a note that the written one wins: the precedence is the one
fact a reader would otherwise get wrong. A CODE directive is never run by the editor (it shells
out, and its answer is a string), and the popup says only that: showing the types its resolver
would be handed was built and taken out, because it reproduced `genctl types` under a hover for
an answer the directive's own line already gives. **BUILT 2026-09-18/19**,
`internal/lsp/directive.go`, `tests/lsp/directive_hover_test.ts`.

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

`genctl types` stops after step 3, in `types` mode. `genctl apply --check-only` runs 1–4 and
asks for the verdict without storing it; `genctl compat -f` runs 1–4 and then compares instead
of storing: a stored version holds the resolved string, so an unresolved directive would compare
as a literal against it and every imported site would read as changed.

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
  - name: import
    phase: code
    ext: [.ts]
    command: [node, tools/genroc-import.ts]
    types: { Input: task.action.input.input, Output: task.action.result }
  - { name: infer, phase: structural, ext: [.ts], command: [node, tools/genroc-infer.ts] }
```

**`resolvers` is an ordered list, and a directive takes the first entry that matches.** A match
is the name AND an accepted suffix; walking stops there. A map cannot express order, and order is
what makes the two rules below mechanical rather than special-cased.

`ext` is **a list of suffixes matched as suffixes** — any one accepts the path, an empty list
accepts everything. Both halves are forced by a two-part convention: `filepath.Ext` answers
`.yaml` for `weather.genroc.yaml`, and one string cannot also admit `.genroc.yml`. A list even at
one entry follows `definitions` in the same file and beats a scalar-or-list union, which is two
spellings and a custom unmarshaler for one idea. A one-part `.ts` compares the same under a
suffix test as under `filepath.Ext`.

**The name still dispatches; `ext` only narrows within it.** `import` and `infer` both take
`.ts` and do unrelated things — one bundles code, the other extracts a return type — so
suffix-first dispatch has no answer for `./x.ts` and the directive's name would be decoration.
What the list adds is one name over several file types: two `import` entries, `.ts` and `.py`,
each with its own command.

So `ext` stays **an assertion, not a dispatch key**, and the property it was for survives: a
`.py` handed to a name that claims only `.ts` matches nothing and fails at genctl with a
sentence, instead of inside `tsc` with a stack. Two errors, not one — no entry carries the name
at all, or some do and none accept the suffix.

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

## The spread form — a directive as a `<<` value

    type: child
    <<: "$process: ./billing.yaml"
    name: billing

Same string, same resolver, same phase rule. **Position decides where the answer lands**: as a
value it fills that slot, as a `<<` value it pre-fills the mapping around it. The resolver name
says nothing about which, so one config entry serves both and a project's own `$xyz` spreads
without registering twice.

A resolver reached this way **must return a mapping**. The rest of what phase 1 may return — a
number, a schema, a fragment — is unspreadable, so one resolver can succeed at one site and fail
at the other. The error names the position, not the type.

### Why `<<` and not a `$` key

The resolver's own name as a bare key (`$process: ./billing.yaml`) reads better and is wrong. It
makes the registry a **namespace over object keys**, and object keys in a definition are user
data: `properties` is keyed by property name, `$defs` by def name, `raises` by error code,
`responses` by status. A schema with a property named `$process` would be captured by a walk that
consumes registered names, and adding a resolver to `.genroc` would retroactively change what an
existing file means. `$ref` and `$defs` are the visible half and could be blocklisted; the
user-keyed half cannot.

**Value position never had this problem** — a `$ref`'s value is `#/$defs/<name>`, which is not of
the form `$<name>: <params>`. The hazard is created entirely by making a bare key meaningful,
which is why no form here does.

`<<` costs none of it, and it is already this grammar's spread: `defdoc` implements it
([defdoc.go:329](../internal/defdoc/defdoc.go#L329)) with the precedence this form wants. One
construct, no new rule, and a source file means the same thing in every repo.

### Precedence and merge depth

**An explicit key beats a merged one** — defdoc's rule already. It cannot be positional:
resolution walks `map[string]any`, a Go map has no key order, and `orderKeys` sorts on output
regardless. Import-wins was never a candidate, because it makes the text the author typed dead.

The merge is **shallow, at the mapping the `<<` sits in**. Deep-merging two schemas is ambiguous
between narrowing and widening, and a per-key depth rule is one the call site cannot show.

`raises` is the key that tests this and still replaces wholesale. Per-code merging is what an
author reaches for, and it loses twice: merge depth becomes key-dependent and invisible, and the
spread already makes the set complete — `Raises()` is a syntactic scan over every clause — so the
only reason to write `raises` by hand is to **narrow** it, which is replacement. Restating a set
to tighten one payload is the cost, and it is the rare half.

### The split defdoc keeps

`defdoc` runs before resolution — `sourceDoc.doc` is the `map[string]any` it produced and
`findSites` walks that, not the node tree — so a `<<` defdoc resolves is gone before phase 1
looks. The alias form has to stay there: it is pure syntax, the LSP parses with defdoc and runs no
resolvers, and moving it behind phase 1 turns every anchor in a repo red until an apply runs.

So the seam is the value kind, which is what `mergeTarget` already is:

| `<<` value | resolved by |
|---|---|
| an alias to a mapping | `defdoc`, unchanged |
| a directive string | passed through as a literal `<<` key; phase 1 consumes it |

defdoc's change is to stop claiming a `<<` it cannot resolve. Where phase 1 does not run, the
leftover key then surfaces as an unresolved directive instead of a parse error — the diagnostic
the editor wants anyway.

**One spread per mapping**, refused with a message otherwise: the pass-through carries a single
literal `<<` value and a second would silently overwrite the first. The sequence form stays
refused for defdoc's own reason — YAML gives its earlier entries precedence, so it reads
backwards.

Precedence now has two implementations. **One `merge(base, overrides)` called by both** is the
whole mitigation, for the reason genctl already owns directive detection (§genctl passes the
sites): two of anything for one rule drift, and the day they disagree the report lands somewhere
unrelated.


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
          "level": "action",                 // process | task | action
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
- **What the site IS travels beside it, as fields**: `level` — which namespace the directive
  sits in — plus `action` (the action's type) and `child` (the process a child action calls). A
  task has exactly one action, so putting its type in the path would name no choice; it travels
  as a field because a resolver that wants to check where it landed would otherwise have to read
  the definition, which the manifest no longer carries. **`action` and `child` appear only at
  `level: "action"`**: a `switch` case is the TASK's, so reporting the action's type there would
  describe a slot the directive is not in. `task` is absent at `level: "process"`, where there is
  no task to name.
- An ARRAY rather than an RFC 6901 string: a string makes every recipient unescape `~0`/`~1`,
  and cannot tell the object key `"0"` from index `0`. object-store.md made the same choice for
  the same reason.
- **A process is the outer structure, and a site sits under the definition it is in.** There is
  no `process` field to join on, and the pool a fragment resolves against is printed beside it.
- **`$defs` is narrowed to what the fragments reach.** It used to be the whole `SchemaFile`, then
  the whole pool; both shipped definitions no resolver opened, and the SchemaFile also carried a
  second copy of every fragment. Refs survive the narrowing rather than being inlined, because a
  task output may reference itself — though a definition that is only a `$ref` is collapsed into
  what it names, since a generator turns each one into a type alias that says nothing
  (schema-command.md §4). A resolver that wants more than it asked for asks for more —
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

## Registered structural resolvers — phase 1's interface

**BUILT 2026-09-19.** A `.genroc` entry with `phase: structural` and a `command` runs on the same
manifest as phase 2, `mode: "structural"`, minus `types` and `$defs`: it runs before inference,
so there is nothing to hand it, and an entry that asks for `types` is refused when the config is
read rather than answered with null at every site. It answers `{"values": [<any>, …]}`, parallel
to the sites as `code` is, and a value is any JSON: a slot site takes it whole, a spread site
takes a mapping and refuses anything else by name. Values are decoded exactly
([number-precision.md](number-precision.md)), so a `default` in a fragment reaches the type view
as written — a resolver that parses its file into a double has already lost it, which is its own.

Batched like phase 2, one call per entry carrying every site that named it, for the same reason:
N directives must not mean N processes. What a value CONTAINS is not resolved again. A directive
inside it is found by the code phase's re-walk, so a structural answer may carry a `$import`,
but not another structural directive — one pass, no fixpoint, the line §Ordering draws for the
spread graph. `genctl schema` and the editor both run this phase, which is what the hover above
reads. `tests/cli/structural_test.ts`, `tests/lsp/directive_hover_test.ts`.

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

## `$process` — another definition's types, spread

    type: child
    <<: "$process: ./billing.yaml"

Phase 1, spread form (§The spread form). Returns the call-site pre-fill for a child of that
definition: `name`, `result_schema`, `raises`. It exists because those are otherwise copied by
hand from a file in the same repo, and a copy is what drifts.

**It is genctl's first built-in resolver, and has to be.** Two of the three are not fields to
read: a definition carries `Output *Shape` and no output schema, and `raises` is
`ProcessDefinition.Raises()`, a scan over every raise clause. The answer is genroc's own inferred
view — [`genctl schema type`](schema-command.md) §7's — so an external binary could produce it
only by re-entering genctl. The mechanism stays open to registered resolvers; this instance
cannot be one. It reaches no server.

Not spelled `$infer`: that name is a script's return type at `ext: .ts`, and `ext` is an
assertion, so reusing it would make one word mean two file types.

**The child's `output` is the parent's `result_schema`** — a rename, not a copy, which is the
other reason this is not a generic fragment loader. The one copy, `input_schema`
([declared-slot-schemas.md](declared-slot-schemas.md)), is NOT canonicalized on the way:
`Canonicalize` drops `description`, and the child author's prose is what the copy is worth
having for — it is what the caller's key hover shows. `version` cannot come from the file at all: a
source file is not a version and `Version: 0` means latest, so the spread fills the types and not
the pin, exactly where hand-writing already stood.

### Built-in, and overridable

Registered as if `.genroc` ended with:

```yaml
- name: process
  phase: structural
  ext: [.genroc.yaml, .genroc.yml, .genroc.json]
```

No `command`, because it runs in genctl; no `types`, because nothing external needs declarations.
Everything else is the resolver rules unchanged — the phase rule, both directive positions, the
escape on splice.

`.genroc.yaml` is the convention `genctl init` writes and every example uses, so the assertion
catches the real mistake — a path to a script, or to a YAML that is not a definition — before the
parse has to word it. It is also what forced `ext` to be a suffix list (§The project config):
`filepath.Ext` answers `.yaml` here, and the other two spellings parse just as well.

**`genctl schema` runs this phase and not the code phase.** A structural resolver moves the types
the command reports, so skipping it would answer about a definition nobody applies; a code
resolver splices a string, which moves nothing, and shells out, which the editor loop cannot
afford. The same split is what lets the LSP stay useful on an unresolved file.

**Built-ins are appended after everything in `.genroc`**, so overriding needs no rule of its own:
first match wins and a local entry is always earlier. An ordered list already says it, which is
why there is no "shadowing" rule here to learn separately.

It also makes an override **per suffix**. An entry named `process` claiming only `.genroc.yaml`
leaves `.genroc.json` to the built-in — the honest reading of a first-match table, and the one
surprise worth stating: to take the name outright, claim every suffix.

Allowed for the reason that inverts the risk: **an overridable built-in namespace is a
non-breaking one.** Reserve the names and every built-in a later genroc adds breaks the repo that
already used that name; let the local entry win and adding one is invisible to whoever had their
own. The accident case is the weak one — shadowing takes `process:` written under `resolvers:`,
which is not reachable by typo.


### Where it goes per action type

The spread sits where the fields are, so it needs no rule of its own — but they are not in the
same mapping for all three:

| action | fields live on | the `<<` goes |
|---|---|---|
| `child` | the action | in the action |
| `child_list` | the action | in the action |
| `child_map` | each `ChildEntry` ([definition.go:33](../internal/model/definition.go#L33)) | in each entry |

```yaml
type: child_map
children:
  billing:
    <<: "$process: ./billing.yaml"
  shipping:
    <<: "$process: ./shipping.yaml"
```

`child_list` needs no array wrapping: its single `result_schema` types **one element**
([infer.go:732](../internal/validation/infer.go#L732)), which is exactly what the child's
`output` is. Filling it is also what makes the array exportable — without it there is no
permissive fallback.

In `child_map` the entry key is the child **key** (`child_key`) and the spread fills `name`, the
**process** name. Often the same word, never the same thing — an entry whose key differs from its
spread `name` is correct and is not to be "fixed".


### It is a Pin, not an Infer

The schema lands in the **stored** definition, so [`genctl compat`](../internal/validation/compat.go#L169)
reads a changed child type as a real contract break — the same property `$infer` is built on. That
is the whole argument for doing this at author time: [unknown-type.md](unknown-type.md)'s
engine-side **Infer** buys the same ergonomics and needs cross-process resolution at runtime,
`(process, version)` memoization and a registration-ordering rule. Still not scheduled, and this
is why.

### Ordering, and the recursion it cannot type

A source's own spreads resolve before its types are inferred, so a spread always reads a resolved
definition. The dependency is a **static file graph**: no versions, no registry lookup, and a
cycle in it is refused with the path.

**That graph is not the call graph, and only one of the two may cycle.** A recursive child is
legal and expected — it "terminates under R5's syntactic scan"
([child-error-handling.md](child-error-handling.md) §8) — so a process that spawns itself, or a
mutually recursive pair, is an ordinary definition. What it cannot do is type itself by
reference. `<<: "$process: ./a.yaml"` inside `a.yaml` is a cycle, and it is the case an author
reaches for first, because at a self-recursive call the types are identical by construction.

The fixpoint that solves this *inside* a definition is not reached from here:
[recursive-type-inference.md](recursive-type-inference.md)'s `solveCluster` works symbolically,
during inference, while a spread is a textual pre-fill in phase 1 — strictly before
`validation.Generate`, so the copy must already be a concrete schema at a point where the solver
has not run.

**One edge of every cycle is therefore written by hand**, and [unknown-type.md](unknown-type.md)
holds both spellings: a Pin, or `{}` where the parent only forwards the value. That is what the
Unknown row is for, and it is the sharpest price of resolving at author time.

**Deferred, not impossible — recorded so it is not re-derived [2026-09-17].** The cross-file case
reduces to the solver's own problem: a spreading process's output type is one more computed
definition and a spread is a `$ref` to it, so cycles collapse on contact and the existing
converge / degenerate / productive / no-base-case outcomes apply unchanged. What is not free is
cross-file member naming against the two `$defs` pools, and synthesizing `$defs` into the parent
— a kept recursion is a `$ref` and a stored definition is self-contained. `$process` would also
stop being a pre-pass and become a participant in inference, which is engine-side **Infer** minus
the engine. Declined against a one-line annotation at the recursion point.


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
- ~~**Phase-1 batching.**~~ **Settled 2026-09-19: the same manifest, batched like phase 2.**
  §Registered structural resolvers.
- ~~**The structural phase.**~~ **Built 2026-09-19** for registered resolvers; `$infer` is what
  remains, and it is one such resolver with `mode: "infer"` reading of the argument.

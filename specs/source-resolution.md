# Source resolution: a definition source file is not a definition

Status: Built, except `$infer` (§Open).

A `.genroc` in the repo registers resolver binaries, a `"$<resolver>: <argument>"` leaf names one,
and genctl resolves the source file into the definition it sends. A TypeScript bundler, a type
generator and a YAML fragment loader are all clients; nothing here is about TypeScript. The file is
`.genroc`, not part of `genctl config`, because `config` already names the runtime `config.*`
namespace.

## The editor's guess about a path

An argument reaches its resolver **as words** (§Directive syntax); genroc never treats one as a
path. The editor guesses, shell-style (`internal/lsp/paths.go`), word by word: an empty first word,
or one that begins `/`, `./` or `../`, is offered files and folders, inserted with a `./` prefix where no directory is typed (a bare name may
be read as something that is not a path). Anything else may be a package, URL or key, and is left
alone. A lone `.` does not qualify: it is a completion trigger, so every typed dot would list a
directory. The suffix filter is the registry's own (`sources.Suffixes`), applied to the first word as
`matchResolver` applies it. A name is written so it splits back to itself: quoted where it holds a
blank, and closing a `'` left open. A name the registry lacks is left unfiltered, because an empty list mid-typing
reads as a broken server.

Hover on a structural directive shows the value it yields, as YAML, from
`sources.StructuralValueAt` (the pass's own call), marking keys the surrounding mapping writes
itself as the ones that win. A typed directive is never run by the editor (it shells out). Its hover
says only that, and does not show the types its resolver would get: that is `genctl generate`
(`internal/lsp/directive.go`).

## Thesis

Two properties are the reason for the shape:

- **A stored definition cannot hold code that failed to typecheck.** The typechecker is the
  resolver's exit code, and a failed resolution never produces the string. It is an ordering
  property; nothing enforces it.
- **The server has no resolver.** Resolution is client-side, so no directive reaches the wire to be
  tricked into executing. That null implementation is the security answer.

A definition version therefore pins byte-identical code forever. Do not trade this for loading a
script from a URL.

## The problem: resolution needs types, types need resolution

A script typechecks against `Input`/`Output` inferred from the definition, and the definition is
what resolution produces. The cycle breaks because **a code string is opaque to inference**, so
resolution splits at exactly that line.

## The two phases

Named by permission, never by content:

- **`structural`** may change what the typechecker sees. It runs before inference and may return
  any value.
- **`typed`** may not. It runs after inference, with types in hand, and **must return a string**.
  genctl enforces that, which makes "cannot invalidate phase 1" structural rather than a promise.

`genctl apply`: (1) parse, and resolve structural directives; (2) splice an empty-string
placeholder at each typed site and type the definitions that carry one; (3) call each typed resolver
entry once, with every site that named it; (4) splice each string, `$`-escaped; (5)
`POST /definitions`. `genctl generate` stops after step 3 in `generate` mode. `apply --check-only` and
`compat -f` run steps 1–4: a stored version holds the resolved string, so an unresolved directive
would compare as changed.

### One roundtrip: genctl computes the types, the server decides validity

Step 2 runs in genctl (`Generate` is pure), so `genctl generate` needs no server and can run on every
edit. It runs neither the strict decode, nor `Validate`, nor `ValidateChildProcessRefs`. Step 5 is
the only verdict, and a second gatekeeper in genctl would be a pair to keep in agreement. It types
only the definitions that carry a typed directive, so one broken file cannot stop generation for the
rest. The cost: types follow genctl's build, so a genctl older than its server infers from older
rules, and the apply refuses the result loudly.

### Why the placeholder is sound

Inference collapses every literal to its base type, so the placeholder and the real code are
indistinguishable to it. [literal-types.md](literal-types.md) would make both an `enum`. Both
would still fit a `string` target, but the guarantee drops from proof to argument. **Re-read this
when literal types land.**

### Why two phases and not N

A configurable N-stage pipeline is a plugin system with no rule: nothing says whether stage 4 may
invalidate stage 2's validation.

## The project config

`.genroc` is discovered upward from the source file. It is not `~/.config/genroc`: which bundler
builds a repo belongs to the repo, and the server URL belongs to the operator.

```yaml
resolvers:
  - name: import
    phase: typed
    ext: [.ts]
    command: [node, tools/genroc-import.ts]
    types: { Input: task.action.input.input, Output: task.action.result }
  - { name: infer, phase: structural, ext: [.ts], command: [node, tools/genroc-infer.ts] }
```

- **`resolvers` is ordered.** A directive takes the first entry whose name matches AND whose `ext`
  accepts the first word. Order is what lets a local entry override a built-in with no rule of its
  own (§Built-in, and overridable).
- **`ext` is a list of whole suffixes on the FIRST word**, and an empty list accepts anything,
  no words included. Words after the first are the resolver's parameters. They are suffixes
  rather than extensions because `filepath.Ext` answers `.yaml` for `x.genroc.yaml`.
- **The name dispatches; `ext` only narrows within it.** `import` and `infer` both take `.ts`, so
  suffix-first dispatch would have no answer. `ext` is an assertion: a `.py` handed to a name that
  claims only `.ts` fails in genctl with a sentence, not inside `tsc`. There are two errors: no entry
  carries the name, or none accepts the suffix.
- **`types` is what the resolver wants typed**, and genctl decides none of it. Each name maps to a
  [`genctl schema type`](schema-command.md) address prefixed by its **frame**:

  | frame | resolves against | where there is none |
  |---|---|---|
  | `task.…` | `tasks.<id>`, the task the directive sits in | `null` |
  | `process.…` | the definition | n/a |

  The frame is written out because both an action and the process carry an `input`. Inferred from
  the site, the same address would answer with the action's argument at one site and the process
  input at the next. An address with no frame is refused when the config is read.
- **A requested type that is not at a site is `null`, not fatal.** Null rather than absent, because
  the name was asked for. Not fatal, because whether it matters is the resolver's call: a script
  that takes no argument has no `input.input`.

## Directive syntax

    code: "$import: ./summarize.ts"

The form is `"$<resolver>: <argument>"`, where the name starts with a letter (`defdoc.Directive`).
The argument may be empty (`"$now:"`). A relative path is relative to the file the directive is in.
`$$import: …` is a literal (§Escaping on the way IN).

**The argument is split into words as sh splits single quotes, and nothing else**
(`defdoc.SplitArgs`): blanks separate, `'…'` is literal and joins what touches it, so
`'./my file.sql' dialect=pg` is two words. `"` and `\` are refused outside single quotes rather
than read as text, so moving to sh's full quoting later changes no directive's meaning. Nothing
expands: `$`, `~`, `*` and `#` are ordinary. A literal `'` cannot be written. In YAML, quote the
directive with `"…"`, since inside `'…'` YAML spells our `'` as `''`. `TestSplitArgsAgreesWithSh`
checks the split against `/bin/sh`.

**The space after the colon is part of the form.** `$` is also the routing sigil, so without it
`goto: $a:b` reads as a directive named `a`. It also keeps `$scheme://host` a string. Task ids are C
identifiers too. Neither rule makes the other redundant, since a URL is not a task id.

A YAML tag (`!import`) was rejected. A tag is parser-level, so every YAML reader that touches a
source would have to know it (js-yaml throws in the tests, and yaml-language-server needs
configuration), and `.json` sources cannot carry one. A string reaches all of them untouched.

## The spread form — a directive as a `<<` value

    type: child
    <<: "$process: ./billing.yaml"
    name: billing

It is the same string, resolver and phase rule. **Position decides where the answer lands.** As a
value it fills that slot. As a `<<` value it pre-fills the mapping it sits in, and the resolver must
then return a mapping; the error names the position.

### Why `<<` and not a `$` key

A bare `$process: …` key would make the registry a namespace over object keys, and a definition's
object keys are user data (`properties`, `$defs`, `raises`, `responses`). Adding a resolver to
`.genroc` would change what an existing file means. `<<` is already the grammar's spread, with the
precedence wanted.

### Precedence and merge depth

**An explicit key beats a merged one.** That is defdoc's rule too, and it cannot be positional
because resolution walks a Go map. The merge is **shallow**, at the mapping the `<<` sits in:
deep-merging two schemas is ambiguous between narrowing and widening. So `raises` replaces
wholesale. The spread already makes the set complete (`Raises()` scans every clause), so writing it
by hand only ever narrows.

### The split defdoc keeps

| `<<` value | resolved by |
|---|---|
| an alias to a mapping | `defdoc` (`mergeTarget`) |
| a directive string | passed through as a literal `<<` key (`isDirectiveValue`); the structural phase consumes it |

The alias form must stay in defdoc. It is pure syntax, and the editor falls back to the text as
written when resolution fails, so moving it behind the structural phase would turn every anchor red
whenever resolution fails. The sequence form is refused because YAML gives its earlier entries
precedence, so it reads backwards. Explicit-key-wins is implemented twice, in defdoc's `mapping` and
in `applyStructural`, so keep the two agreeing.

**One spread per mapping.** The pass-through holds a single literal `<<`, so defdoc refuses a second
`<<` of either form in one mapping, naming both lines. Otherwise one would silently shadow the other.

## Escaping on the way IN — writing a leaf that only looks like a directive

The directive walk reads every string leaf, so a `default`, `description` or `enum` in a user schema
of the form `$name: x` is claimed, and an unregistered name is a hard error. The escape is
`$$name: x`. `defdoc.Directive` declines it, and `defdoc.UnescapeDirective` collapses it. A user
schema is not a Shape, so the template layer's `$$` collapse never reaches it. `UnescapeDirective`
is the inverse by construction (drop one `$`; the leaf was escaped iff the rest is a directive),
never a second regexp.

**Unescaping runs last, after every walk that looks for a directive.** The typed phase re-walks the
document after the structural one, so a leaf unescaped earlier would be claimed. That is why the
pass does not finalise and its callers do: the exported `ResolveStructuralPass`, the end of
`resolveDocs`, and `resolveProcessDirective`. `tests/cli/imports_test.ts` holds both halves.
`resolveProcessDirective` unescapes the child only to analyse it, so it re-escapes
(`defdoc.EscapeDirective`) what it copies into the parent, or the parent's walks claim the text.

## Escaping on splice — the thing the feature is *for*

genctl doubles every `$` in a spliced code string (`escapeDollars`), or the Shape layer reads `${`
in imported JavaScript. Do not narrow this to "escape `${` and a leading `$:`": `scanTemplate`
collapses `$$` unconditionally in every leaf, so the selective rule corrupts a literal `$$`, while
doubling round-trips any bytes. Omitting it fails silently, because the script then runs with
genroc's interpolation applied to it.

## The manifest — phase 2's interface

genctl makes one call per resolver entry per apply, carrying every site that named it: N scripts
must not mean N `tsc` runs. The subprocess runs in the project root (the `.genroc` directory).

stdin:

```jsonc
{
  "mode": "resolve",                     // or "generate"
  "processes": [
    {
      "name": "weather-logger",
      "dir":  "/abs/path/to/defs",       // the definition's directory: an argument's base
      "file": "weather.genroc.yaml",
      "sites": [
        {
          "level": "action",                 // process | task | action
          "task": "summarize", "action": "child", "child": "script-node",
          "pointer": ["tasks", "summarize", "action", "input", "code"],
          "args": ["./summarize.ts"],                          // the argument's words
          "types": {                                           // what `types` in .genroc asked for
            "Input":  { "$ref": "#/$defs/input" },
            "Output": { "type": "object", "properties": { "fee": { "type": "number" } },
                        "required": ["fee"] },
            "Absent": null                                     // asked for, nothing at that address
          }
        }
      ],
      "$defs": { "input": { /* … */ } }    // only what the fragments above reach
    }
  ]
}
```

stdout is `{"values": ["<string>", …]}`, in the manifest's own site order (processes as listed, then
sites within each). A non-zero exit aborts the apply, with stderr as the diagnostic.

- **`mode` is whether genctl uses the answer**, never the phase, which the resolver's own entry
  fixes. A structural resolver always gets `resolve`, even under `genctl generate`, because
  inference needs its answer.
- **`args` are words, never paths**, since a resolver may take a URL, a package name or nothing
  (`[]`, never `null`).
  `dir` is absolute because one call spans directories, and it is what a relative argument joins
  to. A missing file is the resolver's to report; genctl checks only `ext`. There is no `root`,
  because the cwd is the root.
- **`pointer` identifies the site**, since a task may hold two directives. It is an array, so no
  recipient unescapes `~0`/`~1` and key `"0"` stays distinct from index `0`. It names the task by
  id, then the document's own keys, `action` included. Where the slot has a type, the pointer IS its
  `genctl schema type` address (schema-command.md §2).
- **`level`, plus `action` and `child` only at `level: "action"`.** A `switch` case belongs to the
  task, so naming the action's type there would describe a slot the directive is not in. `task` is
  absent at `level: "process"`.
- **Sites nest under their process**, and only processes that have sites appear.
- **`$defs` is narrowed to what the fragments reach.** Refs survive because a task output may
  reference itself, but a definition that is only a `$ref` collapses into what it names
  (schema-command.md §4).
- **A fragment is the type view at the address `types` named**, computed by genctl's inference.
- **What is declared stays declared.** `result` is `result_schema` on a child, the accepted
  `responses` on a fetch, and absent where the task declares neither. It is never inferred from the
  script; that direction is `$infer`.

### genctl passes the sites; a resolver never re-detects them

The reason is drift, not difficulty. Two parsers for one syntax eventually disagree, and the
disagreement presents as a `tsconfig` fault.

### `mode: "generate"`

genctl reads no answer in this mode, so a resolver may skip the work behind one; one that ignores
`mode` and always answers is still correct. `genctl generate` is that call. It exists because the editor needs the declarations before any apply, and it reaches no
server. It uses the same binary and manifest as `resolve`, so there is no second `tsc` over the project.

## Registered structural resolvers — phase 1's interface

A `.genroc` entry with `phase: structural` and a `command` gets the same manifest, minus `types`
and `$defs`. It runs before inference, so an entry that asks for
`types` is refused when the config is read. It answers `{"values": [<any>, …]}` like the typed
phase, minus the string rule. A slot site takes the value whole, and a spread site requires a
mapping. Values decode exactly ([number-precision.md](number-precision.md)). Calls are batched like
phase 2.

There is one pass and no fixpoint. A `$import` inside a returned value is found by the typed phase's
re-walk, but **a structural directive inside one is refused** (`refuseNestedStructural`), naming the
resolver and the path; left alone it would reach the server as a literal string. `genctl schema` and
the editor run this phase. `tests/cli/structural_test.ts`, `tests/lsp/directive_hover_test.ts`,
`internal/sources/structural_test.go`.

## `$process` — another definition's types, spread

    type: child
    <<: "$process: ./billing.yaml"

`$process` is structural and used in the spread form. It returns the pre-fill for a child call of
that definition, so the fields are not copied by hand and left to drift:

- `name`
- `input_schema`, where declared
- `result_schema`, the child's published output
- `raises`, from `Raises()`

**It is built into genctl, and has to be.** The output is inferred unless `output_schema` declares
it, and `raises` is a scan over every raise clause, so only genroc's own inferred view can answer.
It reaches no server. It is not spelled `$infer`, because that name is a script's return type at
`.ts`, and `ext` is an assertion.

The child's `output` lands as the parent's `result_schema`, which is a rename. Inferred schemas are
canonicalized and made self-contained. `input_schema` is a **copy** of the authored schema and is
deliberately *not* canonicalized: `Canonicalize` drops `description`, and that prose is what the
caller's key hover shows. The registration check against the child then confirms the copy is still
current (declared-slot-schemas.md §5). `version` is not filled, because a source file is not a
version and `0` means latest.

### Built-in, and overridable

Registered as if `.genroc` ended with:

```yaml
- name: process
  phase: structural
  ext: [.genroc.yaml, .genroc.yml, .genroc.json]
```

It has no `command` because it runs in genctl. `.genroc.yaml` is what `genctl init` writes and
every example uses, so the assertion catches a path to a script, or to YAML that is not a
definition. It takes exactly one word, and refuses a second rather than drop it.

**`genctl schema` and the editor run the structural phase and not the typed phase.** A structural
resolver moves the types they report. A typed resolver splices a string, which moves nothing, and
shells out, which the editor loop cannot afford.

**Built-ins are appended after everything in `.genroc`**, so first-match lets a local entry override
one. The override is **per suffix**: a local `process` claiming only `.genroc.yaml` leaves
`.genroc.json` to the built-in, so taking the name outright means claiming every suffix. Built-ins
are overridable rather than reserved, so a built-in added later never breaks a repo that already
used the name.

### Where it goes per action type

| action | fields live on | the `<<` goes |
|---|---|---|
| `child` | the action | in the action |
| `child_list` | the action | in the action |
| `child_map` | each `ChildEntry` | in each entry |

`child_list`'s `result_schema` types **one element**, which is exactly what the child's `output`
is. In `child_map` the entry key is the child key, while the spread fills `name`, the process name.
An entry whose key differs from its spread `name` is correct and must not be "fixed".

### It is a Pin, not an Infer

The schema lands in the **stored** definition, so `genctl compat` reads a changed child type as a
contract break. Engine-side **Infer** is argued in [unknown-type.md](unknown-type.md).

### Ordering, and the recursion it cannot type

A source's own spreads resolve before its types are inferred: `resolveProcessDirective` runs the
child's structural pass first. **The spread graph must be acyclic, though the call graph need not
be.** A self-recursive or mutually recursive child is an ordinary definition. But
`<<: "$process: ./a.yaml"` inside `a.yaml` is a spread cycle, and it is refused with the path.

[recursive-type-inference.md](recursive-type-inference.md)'s fixpoint cannot reach a spread. A
spread is a concrete copy made in phase 1, before the solver runs. **So one edge of every cycle is
written by hand**: a Pin, or `{}` where the parent only forwards the value
([unknown-type.md](unknown-type.md)).

Typing a cross-file spread cycle through the solver (a spread as a `$ref` to a computed
definition) was declined: `$process` would join inference, against a one-line hand annotation.

## What a type generator owes

A type generator must get these client-side rules right (`eval-node/import.ts`). Each fails
silently rather than as a compile error:

- **One named type per `$def`, never inlined.** A task output can reference itself, so an expanding
  generator does not terminate.
- **Key type files by the script's path, not the task id.** `summarize.ts` gets
  `summarize.genroc.d.ts` beside it. Keyed by task, renaming a task would break the author's
  `import type` far from the rename.
- **One script at two sites with different input types is an error, not a union.** The union would
  typecheck a body that is wrong at one of the sites. Same script with the same type is reuse.

## This is not the plugin door

[custom-tasks.md](custom-tasks.md) rules out dynamically loaded code, and a resolver registry looks
like exactly that. It is not one: resolvers run at author time, on the author's machine, named by a
file in the author's repo, and the bytes they produce reach the wire as ordinary data.

## Open

- **Stale generated files.** Nothing removes a `.d.ts` whose script was deleted. Cheapest
  fix if it matters: the resolver prints what it wrote and genctl reports it.
- **Caching.** Resolvers run on every apply. Content-hash the manifest if it becomes slow —
  not before, and never in a way that can serve a stale string.

### `$infer` — the other direction

    result_schema: "$infer: ./summarize.ts"

A registered structural resolver that extracts a script's return type
into a JSON Schema, so the definition picks the type *up* instead of handing it *down*:
[unknown-type.md](unknown-type.md)'s **Infer**, reached at author time. The stored definition
carries the result, so [`genctl compat`](../internal/validation/compat.go#L456) still sees a changed
return type as a contract break. Trigger: a hand-written `result_schema` drifting from the script
it types.

**It requires an explicitly annotated return type.** A file both `$infer`'d and `$import`ed would
otherwise need phase 2's `Input` type in phase 1. Refuse the unannotated case by name; whole-program
inference is how the cycle comes back.

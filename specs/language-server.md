# `genctl lsp`: the definition language, in the editor

**PROPOSAL 2026-09-08. Phases 0 and 1 BUILT 2026-09-08.** Five phases in order (§7).

`genctl apply` prints `file:line:col: message`, one line per broken slot;
`POST /api/definitions/validate` returns `fields[]` for a type failure as it always did for a
struct-tag one; and `genctl lsp` publishes diagnostics over stdio for `*.genroc.yaml`. Phase 2 — hover and completion, the reason to build it — is next, and §7b is
what it has to clear first.

[schema-command.md](schema-command.md) §1 refused this on purpose — "not an editor protocol:
it has no positions and no lenient parse, so it cannot underlie completion or diagnostics."
That was right about the command and wrong as a permanent boundary: the analysis is all
there, and what is missing is a location. This spec is the argument that the location is
worth building for its own sake, and that the editor is what it unlocks second.

## 1. What is already the whole engine

Diagnostics for a definition are two local calls — no server, no database:

    def.Validate()                 // internal/model/validate.go — structural
    validation.Generate(&def)      // internal/validation/generate.go — inference

and the editor's other questions are answered by APIs that shipped with `genctl schema`:
`SlotContexts` (address → what is readable there), `TypeSlots` / `TypeDocument` (address →
what shape it is), `Navigate` and `Schema.At` (walk a dotted prefix), `Schema.Infer` (the
type of an expression). Multi-file resolution and the `.genroc` project config are
`cmd/genctl/sources.go`.

Structural checking is the same two calls: `numeric.DecodeStrict` rejects a key with no home
and `Validate` covers the cross-field rules, so the editor's structural answers are the
server's own (§5) — not a JSON Schema's reading of them.

So the server is a shim. The work is §2, §3 and the reflection walk in §5.

## 2. A location is the missing primitive — and it is not only the editor's

Today a definition failure can be located three incompatible ways:

| Where | Spelling | Structured? | Plural? |
|---|---|---|---|
| struct-tag rules (`model.FieldError`) | `tasks[0].id` | yes | yes — all at once |
| inference (`validation`, 25 sites in `infer.go`) | `task "fetch" output.total` — **inside the message** | no | no — first error, then `return` |
| `genctl schema` addresses | `tasks.fetch.output` | yes | n/a |

The second row is the defect, and it is not an editor problem. It is why
`POST /api/definitions/validate` hands a client prose it cannot attribute to a field while
the row above it returns `fields[]`; it is why `genctl validate` cannot print a line number;
it is why a UI cannot highlight anything. **The editor is the fourth consumer, not the
reason.**

### The vocabulary is the one that already exists

Errors carry a **slot address** — the grammar in [schema-command.md](schema-command.md) §2,
`tasks.<id>.output`, `output`, `tasks.<id>.on_error.0`. Not a new one, because a second
address grammar is a second thing that can be wrong, and because `genctl schema context
tasks.fetch.output` should name the same place the error does. That is the test: a
diagnostic's address, pasted into `schema context`, answers *what could I have written here*.

`tasks[0].id` is a **physical** path — index-based, so it moves when a task is inserted
above it, and no human types it. It stays the index's internal key (it is what the YAML tree
has); the id→index map is built once per document and the conversion lives in one place.

### Collect, do not return first

`Generate` stops at the first inference failure, so an editor would show one squiggle at a
time. Inference is sequential — a later task's context is built from an earlier task's
inferred output — so recovery is not free. Recover with the type the language already has
for *value present, shape unavailable*: **`{}`**, the unknown ([unknown-type.md](unknown-type.md)).
A task whose output failed contributes `{}` downstream, and the address is marked poisoned so
diagnostics *derived from* it are suppressed rather than reported as a cascade of second
failures.

The collecting form becomes the primitive; `Generate` stays as it is, a wrapper returning the
first diagnostic as an `error`, so the two callers that only gate on it (`handlers_definitions.go`,
`sources.go`) do not change.

### A diagnostic code

Each diagnostic carries a dotted code (`def.expression.unknown_field`), so an editor can
suppress a class and `genctl validate --json` is machine-readable. A **new namespace, not
`errcode`**: those codes are runtime faults an `on_error` can catch, these are things that
make a definition unregistrable and can never be caught. Sharing the type would put
uncatchable codes in a `case:` author's autocomplete.

## 3. `internal/defdoc` — parse once, keep the positions

`yamlToAny` (`cmd/genctl/yamlnum.go`) already walks the `yaml.Node` tree, already reads
`n.Line` for one error message, and throws the rest away. The index is that same walk
emitting a second output.

    defdoc.Parse(data) → (value any, index Index, err error)
    index.Range(addr) → (line, col, endLine, endCol, bool)

It moves to `internal/defdoc` rather than being written again in the server, which is the
whole point: genctl gets line numbers for free the moment it stops importing `yamlToAny`
from `main`. The exact-numeric behaviour (a 54-digit id must not become `1.2e+53`) travels
with it and keeps its tests.

## 4. Not a module — `genctl lsp`

`internal/lsp`, behind a `genctl lsp` subcommand, fenced by `archtest` exactly as `genctl` is.

**This reverses the first draft of this section**, which put the server in its own
`genroc/lsp` module. That was built, then measured, and the measurement went the other way:

- **It fenced nothing.** The argument was that a language server accumulates editor
  dependencies which must not reach the server binary. The built one had **27 external
  dependencies and none of its own** — every one inherited from `genroc` (yaml, expr, apd,
  validator, mimetype, x/*), because JSON-RPC framing is 130 lines and was written by hand.
  The fence guarded a hypothetical.
- **It could not reach the project config.** `.genroc` discovery, `definitionPaths` and the
  resolver table live in `cmd/genctl/sources.go`, which is `package main` and therefore
  importable by nothing. Cross-file navigation (§7 phase 3) needs exactly that file set, so
  the module would have forced the move into `internal/` anyway — after which it bought only
  the hypothetical above.

This is the reasoning CLAUDE.md already applies to `genctl` itself: it shares its whole
internal surface with the server, so **a module boundary would relocate the dependency rather
than remove it**. The language server shares that same surface, and one binary people already
have beats a second to install and keep in step.

**Measured, not assumed** — and worth keeping, because it is the correction the first draft
turned on: a separate module *can* import `genroc/internal/...`. Go's internal rule is
path-prefix, not module-scoped, so any module named `genroc/...` clears it. The comment in
`internal/archtest/imports_test.go` ("`ui` and `jwks` … cannot reach `genroc/internal` at
all") was therefore wrong about the mechanism — `ui` cannot reach it because `ui/go.mod` does
not require the root module, not because the rule forbids it. Fixed in phase 0.

The trigger to reopen this: the server growing a dependency of its own that the engine has no
use for — an incremental parser, a fuzzy matcher. Then the fence stops being hypothetical, and
the project config has by then moved out of `package main` regardless.

## 5. The structural layer is ours too

The `# yaml-language-server: $schema=` comment in the templates is not a working baseline to
build on top of. The published schema is a lossy projection of the model, and it is **looser
than the server on exactly the mistakes people make** — measured 2026-09-08, ajv against
`docs/public/process-schema.json`, the same document the editor loads:

| Written | What the editor says | What `genroc` says |
|---|---|---|
| `on_eror:` on a task | *accepted* | `unknown field "on_eror"` |
| `tsaks:` at the root | *accepted* | `unknown field "tsaks"` |
| `ur1:` in a `fetch` | 9 errors, naming `children`, `for`, `until` | `unknown field "ur1"` |
| `switch: $nope` | *accepted* | `goto "$nope" is not a known task` |

Four rows, four ways to be wrong, and three distinct causes:

- **No `additionalProperties: false` above the action.** Only the six action variants are
  strict; `ModelTask` and the root are not, so a misspelled key at the two levels people write
  most is silent. The server rejects it — `decodeBody` is `numeric.DecodeStrict`, and
  `DisallowUnknownFields` is recursive.
- **`discriminator` is an OpenAPI keyword, not a JSON Schema one.** `ModelAction` is
  `{discriminator: {propertyName: type}, oneOf: [...6]}`; a JSON Schema validator ignores the
  first half and reports the union of six failed branches. Hence a `fetch` typo demanding
  `children`.
- **Cross-field rules are not expressible at all.** `goto` naming a real task, catch-all-last,
  `until` only on an `external`, the `only_once` retry restrictions — all of it is in
  `Validate()`, which the schema cannot see.

### So the LSP does not consume the schema

Consuming our own JSON Schema would import all three defects; the schema is the artifact, not
the source. **Structural diagnostics are `DecodeStrict` + `Validate()`** — the server's own
answer, verbatim, so the editor and the server can never disagree. That is the whole point of
§2: those answers already exist and only lack a location.

Unknown keys are the one case where the decoder is not enough — `DisallowUnknownFields`
returns prose and stops at the first. `defdoc` (§3) already holds the document tree, so the
check becomes a **reflection walk over the model beside it**: every key with no home is
reported, all of them, each with a range. The same walk answers the other two questions:

| Question | Answer from the same walk |
|---|---|
| is this key legal here? | the field set at this node |
| what keys *are* legal here? | completion |
| what does this key mean? | the `description:` struct tags, which already carry full prose |

At an `action:` node the walk reads `type:` first and descends into that variant only — which
is what `discriminator` meant and what JSON Schema could not say. The `fetch` typo becomes
"fetch has no field `ur1`", and completion offers fetch's fields rather than the union of six.
`goto` completion is the task-id list `Validate` already checks against.

### Fix the published schema anyway

`additionalProperties: false` on the task and the root is a **bug fix independent of this
spec** — the server already rejects those keys, so the schema is simply wrong, and it is what
an editor with no genroc extension will keep loading. Phase 0.

**Built 2026-09-08**, and closing it uniformly is what found the next hole: the generator has
no way to express "closed, except `raise`", and `Fault` was the one definition struct with no
unknown-key check — reached through `ErrorCase`'s own `UnmarshalJSON`, where
`DisallowUnknownFields` does not propagate. An exemption in the generator would have recorded
the bug instead of the rule, so `Fault` was closed too. A user schema (`input_schema`,
`$defs`) is the remaining divergence: `schema.Schema`'s node is private, so it reflects to an
opaque object and the editor accepts a keyword the server refuses by allowlist. Recorded by a
test rather than left to be rediscovered.

### What happens to yaml-language-server

The `$schema` comment stays: it is the degraded path, and after the fix above it is an honest
one. When `genctl lsp` is attached it is the only server that should be answering for
`*.genroc.yaml`, so the VS Code client ships that setting rather than leaving two servers to
double-report. This reverses the earlier draft of this section, which accepted the overlap —
the measurements above are why.

**What must not regress:** anchors and aliases, merge keys (`<<:`, already handled by
`mergeSource`), multi-document files, and exact numeric literals. These are YAML-level and
`yamlToAny` is where they live, so moving it to `defdoc` keeps them by construction — with
their tests.

## 6. What the expression parser still cannot do

`syntax.Parse(src) (Node, error)` has no offsets and stops at the first error, so a
half-typed expression has no AST and a diagnostic inside one can only underline the whole
scalar.

Completion does not need it: scan back from the cursor over `[A-Za-z0-9_.]`, take the dotted
prefix, hand it to `Navigate`. That is phase 2 and it is enough. Offsets in the AST are a
real change to `parser.go` and are deferred until something needs a range *inside* an
expression — precise squiggles, or semantic highlighting.

## 7. Phases

| # | What | Payoff without the next phase |
|---|---|---|
| 0a ✅ | `internal/defdoc`; `additionalProperties: false` on every reflected struct, and the drift test | the published schema stops accepting what the server rejects |
| 0b ✅ | slot addresses on every diagnostic; collect-with-recovery; codes; `Check` beside `Generate` | API returns `fields[]` for inference errors; `genctl` prints `file:line:col` |
| 1 ✅ | `genctl lsp`: stdio JSON-RPC, document store, didOpen/didChange, publishDiagnostics — structural *and* inference | squiggles that agree with the server |
| 2 | completion + hover: keys and action variants from the reflection walk; context and types inside `$:` / `${ }` | the reason to build it |
| 3 | goto-definition on `goto:` and child `process:`; code actions | — |
| 4 | VS Code client — spawn `genctl lsp`, activate on `**/*.genroc.yaml`, disable YLS for them | distribution |

Phase 0 is the one that can be got wrong, and it is the one whose value does not depend on
any of the others landing. Phase 2 is where the reflection walk of §5 and the slot contexts of
§1 meet: one completion request answers with keys or with context, decided by whether the
cursor sits in an expression.

## 7b. What phase 0 left for phase 2

Two limits, both found by building it, and both squarely in the editor's way:

- **`SlotContexts` answers nothing about a document that does not infer.** It goes through
  `Generate`, so a single broken expression costs the whole context view — and a document
  mid-edit is the normal case for completion. Now that `Check` recovers per slot, the fix is
  to give it the partial `SchemaFile` rather than `SchemaFile{}`. schema-command.md §1 already
  promises this ("as far as inference gets"); it is not true yet.
- **A diagnostic underlines its slot, not its sub-slot.** The address is `tasks.a.action`
  because that is the grammar's granularity, so a bad `url` underlines the whole action even
  though the message names the url and `defdoc` indexes `tasks.a.action.url`. A second, finer
  field on Diagnostic — the location, beside the context address — would close it without
  touching the grammar.

## 8. Testing

Phase 0 is Go: a table of broken definitions against expected `(address, code)` sets, and the
addresses asserted to round-trip through `genctl schema context`. That round-trip is the
assertion that keeps the two grammars one grammar.

The §5 table is itself the other test. It landed in Go rather than in `tests/` where ajv is —
`gojsonschema` is already a test dependency, so the comparison runs in `go test` against
`ProcessSchema()` with no server and no node: `internal/api/processschema_test.go`. Every row
is a definition the server rejects, asserted to be rejected by the published schema too. Drift
is the failure mode — the schema is generated, the rules are hand-written, and nothing
compared them.

Phases 1–3 drive the server over a pipe with recorded JSON-RPC sessions. No editor in the
loop. Built that way, plus the claim §5 rests on as a test of its
own — a table of documents run through both the editor's path and the server's two calls,
asserting they refuse the same set.

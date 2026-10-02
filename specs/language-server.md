# `genctl lsp`: the definition language, in the editor

**Status:** Built.

`genctl lsp` speaks LSP over stdio for `*.genroc.yaml` and provides:

- **diagnostics**
- **hover**: the type of the symbol, expression or slot under the cursor, or else what the key
  means; over a structural directive, what it yields
- **completion**: scope members inside an expression, legal keys elsewhere (discriminated, so a
  `fetch` is offered fetch's keys), and values for task ids, action types, error codes and
  directive paths
- **go-to-definition**: a `goto` to its task, and a child action to the file defining its process
- **semantic highlighting**

The VS Code extension is a launcher for it and nothing else. The invariants that break silently live
in [internal/lsp/CLAUDE.md](../internal/lsp/CLAUDE.md).

## 1. What is already the whole engine

The analysis is the server's own, run locally with no database: `numeric.DecodeStrict` and
`Validate` for structure, and `validation.Check` for types. The editor's other questions use the
`genctl schema` APIs (`SlotContexts`, `TypeSlots`/`TypeDocument`, `Navigate`, `Schema.At`,
`Schema.Infer`), plus `internal/sources` for `.genroc` and the structural phase. What this server
adds is **locations** (§2, §3).

## 2. A location is the missing primitive — and it is not only the editor's

Every definition failure carries a **slot address**. It uses the grammar of
[schema-command.md](schema-command.md) §2 (`output`, `tasks.<id>.output`, `tasks.<id>.on_error.0`)
rather than a second grammar, because a second grammar is a second thing that can be wrong. The test
is that a diagnostic's address, pasted into `genctl schema context`, answers *what could I have
written here*. The same address serves three consumers:

- `genctl apply`, which prints `file:line:col`
- `POST /api/definitions/validate`, which returns `fields[]` for a type failure as for a struct-tag
  one
- the editor

`defdoc` indexes every node under both its physical spelling (`tasks[0].on_error[1]`) and its logical
one (`tasks.fetch.on_error.1`), so no conversion between them can be wrong.

**The decoder's spelling is translated.** `encoding/json` locates a failure by the Go type and the
OUTERMOST slot, and says nothing inside a custom unmarshaler, which every schema has. So:

- `internal/schema` reads a schema document's shape before decoding it, reporting with `CheckDoc`'s
  prose-plus-`AtPath`.
- Elsewhere, the decoder stack's LAST segment is the field that failed.
- A schema slot that is itself not an object is placed by finding the one schema position that is
  not an object, and declined when two are.

**Collect, do not return first.** Inference is sequential: a task's context is built from earlier
outputs. So a failed task output recovers as `{}`, the unknown ([unknown-type.md](unknown-type.md)),
and its address is marked poisoned. `def.unknown_read` diagnostics derived from that output are then
suppressed, but only in tasks that can SEE the poison (`bag.sees`), because a blanket rule hides real
findings. `Check` returns every diagnostic, and `Generate` wraps it as the registration gate.

**Codes** are `def.expression`, `def.unknown_read` and `def.structure`, plus `def.syntax` and
`def.resolve` in the editor. They are a new namespace, **not `errcode`**: those are runtime faults an
`on_error` can catch, and sharing the type would put uncatchable codes in a `case:` author's
autocomplete.

## 3. `internal/defdoc` — parse once, keep the positions

`defdoc.Parse` walks the `yaml.Node` tree once, producing both the value and a span index
(`Doc.Span`, and `Doc.Locate`, which falls back to the nearest enclosing path for a missing required
field). genctl and the server share it. It owns the YAML-level behaviour, each piece with its own
tests:

- exact numeric literals (a 54-digit id must not become `1.2e+53`)
- anchors and aliases
- `<<` merges (`mergeTarget`)
- multi-document files

A `.json` source has no index.

## 4. Not a module — `genctl lsp`

`internal/lsp` sits behind a `genctl lsp` subcommand. `archtest.TestBinariesKeepTheirImportBoundaries`
refuses it `db`, `api`, `engine` and `transport`, exactly as for genctl. **It is not a module of its
own**: every dependency it has is inherited from `genroc` (JSON-RPC framing is hand-written), so a
module boundary would relocate the dependencies rather than fence them. A separate module could
still import `genroc/internal`, because Go's internal rule is path-prefix and not module-scoped.
`ui` is fenced by its go.mod, which requires nothing from here. Reopen this if the server grows a
dependency of its own that the engine has no use for.

**The editor runs the structural phase, through `internal/sources`**, the same code genctl runs. A
`<<` spread changes which keys a document has, so skipping it reports `unknown field "<<"` on text
that applies. `document.resolve` is the one place text becomes a document and resolution runs, and
a handler must not parse for itself. The code phase never runs in the editor.

**Cross-file navigation searches the `workspaceFolders` sent in `initialize`**, open buffers before
disk. It does not use `.genroc`'s `definitions:`, which says what an apply deploys rather than what
exists.

### Or the one the extension carries: `genctl.wasm`

Where no genctl with an `lsp` command is on PATH, the extension runs a bundled `GOOS=wasip1` build.
That is a plain `go build`, since genctl has no cgo and opens no sockets, and it is one artifact
rather than one per platform. It is the **fallback**, not the default. It is slower (about 110 ms
to start against 6 ms), and it is the version the extension shipped rather than the genctl that will
`apply`.

- VS Code's Electron runs as Node (`ELECTRON_RUN_AS_NODE=1`), where `node:wasi` lives, so nothing
  extra is installed.
- Each workspace folder is preopened at its own absolute path, so `file://` URIs need no
  translation.
- wasip1 has no sockets and no subprocesses. A registered structural resolver cannot run there,
  and a server that grows either stops being portable this way.
- Node may hand the guest non-blocking stdio (it does on Linux), so under wasip1 the server first
  clears the flag (`blockStdio`). Without that, a session ends one message in with EAGAIN.

## 5. The structural layer is ours too

**Diagnostics never consult the published JSON Schema**; they are `DecodeStrict` + `Validate` +
`Check` verbatim (`analyseDoc`). JSON Schema cannot express the cross-field rules: a `goto` naming a
real task, catch-all-last, `for`/`until` only on a `delay`, the `only_once` restrictions. Its
`discriminator` is an OpenAPI keyword that a JSON Schema validator ignores, so a `fetch` typo would
be reported as six failed branches. `DecodeStrict` stops at the first unknown key, so a document
with two typos shows them one at a time. `TestTheEditorAgreesWithTheServerOnWhatIsRejected` pins
the agreement.

**Completion reads the generated schema (`defschema.Process`), not reflection.** Several definition
types decode by hand or describe themselves through a hand-written `JSONSchemaBytes` (`Action`,
`SwitchMap`, `Retry`, `Timeout`, `ErrorCase`, …). Reflection over their Go fields sees nothing, and
the schema is the only machine-readable description. The server interprets `discriminator` itself,
descending into the branch whose `type` const matches. `defschema` lives outside `internal/api`
because archtest forbids `api` to the server.

**The published schema is closed.** Every reflected struct has `additionalProperties: false`, and so
does `Fault`, whose `UnmarshalJSON` rejects unknown keys too. A user schema's meta-schema is built
by `Schema.JSONSchemaBytes` from `allowedKeywords`, so the allowlist and the vocabulary are one
table. `TestPublishedSchemaAgreesWithTheServerOnUnknownKeys` pins the schema to the server. It is
what an editor without the extension loads.

**yaml-language-server.** The `# yaml-language-server: $schema=` comment stays a spelling anyone may
add, as the degraded path, but `genctl init` does not write it. The extension gives `*.genroc.yaml`
its own `genroc` language id, so two servers do not double-report.

## 6. What the expression parser still cannot do

`syntax.Parse` has no offsets and stops at the first error, so a half-typed expression has no AST.
A diagnostic inside an expression underlines the whole scalar.

Completion and hover scan back from the cursor over `[A-Za-z0-9_.]` instead. `symbolUnder` truncates
the path at the segment the cursor is in, so `self.result.<discount>` types as `number|null` where
the whole leaf types as `number`. The scan cannot tell a member path from a word inside a string
literal, so a symbol that does not type is dropped rather than reported. The same lack of offsets
is why nothing can point at a piece of an expression (a binder, a sub-range). Semantic highlighting
uses the lexer's `syntax.Tokens` instead.

Offsets in the AST are still a real change to `parser.go`, deferred until something needs a
range *inside* an expression — precise squiggles.

## 7. Phases

All built; the feature list is at the top.

## 7b. Address and location; answering a document that would be refused

- **`Diagnostic.Location` sits beside `Address`.** The address is the scope `genctl schema context`
  answers about (`tasks.a.action`). The location is the field to underline
  (`tasks.a.action.url`). Checks that know the field (`url`, `method`, `headers`, `query`,
  `accepted_status`, `over`, `for`, `until`, `timeout`) set it, and the rest fall back to the
  address. The API's `fields[].field` carries the location.
- **`Check` returns the partial view it built**, and `SlotContexts` reads that view, not what
  `Generate` would accept. So `genctl schema context`, hover and completion answer over a document
  that would be refused. The strict decode is a verdict, and the diagnostics path is the only one
  that runs it.

## 8. Testing

- `TestEveryDiagnosticAddressIsASlotTheContextViewNames` keeps the diagnostic and context grammars
  one grammar.
- `TestPublishedSchemaAgreesWithTheServerOnUnknownKeys` (Go, gojsonschema) and
  `TestTheEditorAgreesWithTheServerOnWhatIsRejected` hold §5.
- `tests/lsp/` drives the real binary against one fixture, with positions marked inline: `<|>` for
  the cursor, `<|text>` for text not yet typed, and `<^text>` for a cursor inside text that stays.
- **Positions are swept, not sampled.** Every bug reported from a real editor was at a position
  nobody picked, so `sweep_test.ts` visits every column of every line and asserts on the completion
  KIND. Its differential checks avoid a list of which mappings are open maps, which would rot.
- `wasm_test.ts` is differential against the binary, because a fallback that quietly disagrees is
  worse than none.

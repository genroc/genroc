# internal/lsp

The language server behind `genctl lsp` — the definition language in an editor.
specs/language-server.md. A module of its own was measured and rejected (§4): it inherited
every dependency it would have fenced, and could not reach the project config in
`cmd/genctl`, which cross-file navigation needs.

**Every answer is the server's own.** Structural checking is `numeric.DecodeStrict` +
`ProcessDefinition.Validate`, types are `validation.Check` — so the editor and an `apply`
cannot disagree. Nothing here reimplements a rule, and nothing reads the published JSON
Schema, which is a lossy projection of exactly these calls (§5). `TestTheEditorAgreesWithThe
ServerOnWhatIsRejected` is that claim as a test; a new check belongs on the server side of it.

## Four things that are silent when broken

- **Full sync only.** `initialize` advertises `textDocumentSync: 1`, so every change carries
  the whole document. Advertising incremental without keeping the state would leave the
  server analysing text the editor no longer shows.
- **`didClose` publishes an empty set.** An editor keeps the last diagnostics it was given, so
  a closed file's errors outlive the file unless they are explicitly cleared.
- **Columns are UTF-16 code units, not bytes and not runes.** `defdoc` reports 1-based byte
  columns; the protocol counts from 0 in UTF-16, where an astral-plane character is *two*.
  Counting runes is the wrong answer that looks right in every ASCII test.
- **One goroutine.** The work is one file's parse and inference. Concurrency would buy latency
  nobody notices and a class of races nobody wants; `docs` is a plain map for that reason.

**stdout belongs to the protocol.** `genctl lsp` prints nothing; a stray line is a frame the
editor cannot parse.

## Hover decodes leniently; diagnostics do not

`decodeLenient` vs `DecodeStrict` is the whole difference, and it is deliberate: a reader
asking about one slot is not asking about a typo in another, so hover answers over a document
that would be refused. The strict decode is a **verdict**, which is the diagnostics path's job
alone. `SlotContexts` behaves the same way for the same reason — it reads what `Check` managed
to build, not what `Generate` would accept (specs/language-server.md §7b).

A `${ }` inside a longer string types as the string it renders into, so the leaf says nothing.
`interpolationUnder` reads the interpolation the CURSOR is in out of the raw source line —
raw, because a column is what the protocol hands over, and mapping it back through YAML's
escaping would be a second grammar to keep true.

**A hover is ONE line, and it always has one.** The answer order is: a type where the cursor is
on an expression or a slot, else **what the key means** — the prose the struct tag already
carries. Dropping the scope line without that fallback left hover silent on ten lines out of
twelve, which reads as a hover that does not work, and was reported as one.

**A hover is ONE line: the type of what the cursor is on.** It was three — the symbol, the
expression around it, and the slot's scope — and the two extra lines answered questions nobody
had asked at that moment. `genctl schema context` is where a scope is asked for.

`symbolUnder` is the same trick one level down: the member path the cursor sits in, truncated
at that segment, so walking `self.result.total` shows each level. It reads raw text because the
expression AST carries no offsets (specs/language-server.md §6), which is also why a symbol
that does not type is DROPPED rather than reported — the scan cannot tell a member path from a
word inside a string literal, and the leaf's own line answers either way.

## Completion reads the schema; diagnostics read the server

Not the reflection walk §5 first proposed. Seven model types decode by hand and carry their own
`JSONSchemaBytes` — reflection sees no fields on an `Action`, a `SwitchMap` or a `Retry`, which
are the nodes most worth completing. The schema describes them exactly, and phase 0 closed its
two defects (`additionalProperties`, and the drift test). `branchFor` reads each arm's
`type: {const: …}` and descends into one, which is what OpenAPI's `discriminator` meant and
what no JSON Schema validator will do for you.

`walk` tracks the ABSOLUTE document path as it descends, because the discriminator is read out
of the document at the node being entered. A relative path looks up `type` at the root and
silently offers the union — the first version did exactly that.

**Completion runs on text that does not parse**, which is the normal state of a buffer someone
is typing in. `expressionPrefix` scans the RAW line, and `parseRepaired` retries the parse with
only the cursor's line closed off (`"`, `}"`, `"}`, `: `, `]` — an unclosed `[` swallows every
line below it, so a half-written list stops the whole document parsing). Repairing more would answer about a
document the author is not looking at.

**A `case` is an expression written BARE.** It is an expression slot, not a Shape, so there is
no `$:` for the scan to find — and without `isBareExpression` the cursor reads as sitting on a
key: hover answered with what `case` means and completion offered the clause's own `goto` and
`raise`.

**Completion computes the scope from the document WITHOUT the leaf under the cursor.** A
half-typed expression does not type, its slot recovers as `{}`, and everything reading that
slot then offers nothing — which is worst exactly where help was asked for. `self.previous` in
a looping task is the case that proves it: the slot being written IS the one being read.

**On a line with nothing before the cursor, indentation decides — not `At`.** An empty line is
inside every ancestor at once, so `At` answers with the outermost container (the document
root), which is what completing inside `input_schema:` used to offer. `keyAbove` walks up to
the first line at the same indent (a sibling, so the mapping is its parent) or a smaller one
(the key whose value the cursor is inside).

**A cursor ON a key wants that key's siblings**, not its children — someone typing `respon` is
choosing among the action's keys. `completeKey` decides with `span.Key.Contains`, not by
guessing from the value's type.

**A key completion writes its colon.** The keystroke after choosing a key is the value, never
punctuation — so an item writes `only_once: ` where the value goes beside the key and `on_error:`
where a block opens below it; `canBeScalar` reads which from the schema, and takes the node's
declared `type` BEFORE any union, since an arm may carry a `oneOf` saying which of ITS keys go
together. Two things it must not do: write a second colon onto a line that already has one (a key
chosen over one that is written), or replace only as far as the cursor — a key chosen from inside
a word has to swallow the rest of it, which is what `replaceTo` is for.

**A value completion REPLACES what is typed.** `$` is not a word character, so an editor given
no range inserts beside it — choosing `$tick` after `goto: $` left `$$tick`. Every routing item
carries a `textEdit` whose range starts at the token, which the server builds from
`replaceFrom` once it has the line to convert columns against.

**An empty routing value resolves through the line's KEY.** `goto:` with nothing after it has a
zero-width value node, so a cursor past it lands on the sequence enclosing it — which is a
routing slot by name and holds clauses, not a name. `routingPath` tries the cursor first and
the key second, and rejects a `switch` whose value is a list either way.

**Two VALUE slots have a closed set**, and neither is declared as one: a routing slot is every
task in the document plus `end` and `next`, and a `type` is either the JSON type names (inside a
user schema) or the variants of the union it discriminates, read from the arms' own `const`s.
Without `routingValues` and `typeValues` the cursor in `goto: $` or `type: ` reads as sitting on
a key, and the node's own siblings come back.

A user schema's `type` takes a LIST of those names as well as one of them — which is how a
nullable property is declared — so `[]` is offered beside them, last, and dropped once the cursor
is already inside one. An empty flow list has a ZERO-WIDTH node, so that cursor does not resolve
through the index at all: `insideFlowList` reads the brackets off the raw line and the slot is
the line's key, the same fallback `afterKey` is.

Two things about that list depart from what the language ACCEPTS, on purpose. `array` is not
offered — beside `[]` it read as a second spelling of it, and the two mean opposite things — and
`null` is listed by name but WRITTEN as `"null"`, since bare null is YAML's null value and
`type: null` decodes as no type at all, silently. `insert` on a completion item is that
distinction between what a reader picks and what lands in the document.

**An array is INDEXED, and the dot a reader has just typed is not legal after one.**
`input.who.` offered nothing at all, which reads as a server that does not work; it offers `[0]`,
whose edit starts ON the dot so the result is `input.who[0]` rather than `input.who.[0]`.
`dottedTail` steps over a whole `[…]` group for the same reason: stopping at the bracket left the
container empty, so `input.rows[0].` answered with the ROOT scope — `input`, `self`, `outputs` —
in a position where none of them is legal.

**Hover is silent where a DIAGNOSTIC already speaks.** An expression that does not type gets
no hover line: the editor puts the diagnostic for that position at the top of the same popup,
and a reader sees it twice. The symbol inside it still types, and that is the part the
diagnostic does not say.

**An unlocated diagnostic underlines the value its message names, not the file.** The
hand-written rules in `model.Validate` and the decoders report prose with no path, and falling
back to the document root painted every line red for one bad word — while the reader was still
typing it. `nodeHoldingQuoted` looks for the sole node holding one of the message's quoted
words, LAST first: a message names its subject before its complaint, so `task "tick" switch:
goto "$" is not a known task` is about the `$`. Failing that, the first line.

**A schema reports where it failed relative to its OWN root**, because a sub-schema does not know
which slot of which document holds it — so `properties.who` is matched as a SUFFIX of a document
path, the same sole-match rule as an unknown key. The one failure it cannot place at all is a
slot that is not an object; `soleSchemaSlotNotAnObject` asks which schema position in the
document is a scalar, which is what `x-genroc-user-schema` makes answerable.

**A decode failure is rewritten before it is shown.** encoding/json names the Go type that could
not hold the value, in a field stack that skips list indices and map keys — `only_once: 5` read
as "cannot unmarshal number into Go struct field Task.tasks.only_once of type bool" and
underlined the whole `tasks:` block. The stack's LAST segment is the field that actually failed,
so it locates like an unknown key does, and `typeErrorMessage` says what that field takes in the
words the document is written in.

## The schema is repaired on load

`processSchema` puts back two things the published document cannot carry, and neither is a
guess — both are positions where a USER SCHEMA sits:

- **A user schema nests user schemas.** `properties`, `$defs`, `items`, `additionalProperties`,
  `oneOf`, `anyOf` all hold another one. The published schema leaves them permissive because
  openapi-typescript turns a self-`$ref` into a cycle tsc rejects (internal/schema/CLAUDE.md).
  Nothing here generates TypeScript.
- **`responses` values and `result_schema`** are user schemas too, hand-written as permissive
  objects in `model.Action`'s template.

Without the repair, hover and completion go silent the moment a reader is inside an
`input_schema` or a `responses` block — which is a third of a real definition.

`walk` returns the TERMINAL node as declared, union and all, and `choose` is the caller's to
apply: one caller wants the arm the document selects, another the variants it selects from.
`choose` picks among union arms by the document's `type` where there is one, else the arm that
can take the next step. A `switch` is a scalar shorthand OR a list of cases and nothing marks
which, so the INDEX decides — `switch.0` is a case.

## The one shortcut

`DisallowUnknownFields` reports prose and stops at the first unknown key, so `decodeDiagnostic`
reads the key out of the message and finds it in the index — matching by SPAN, since every node
is addressable twice (physically and logically) and counting paths finds two of everything.
§5's reflection walk is what replaces it: every unknown key, with a path, in one pass.

## The workspace comes from `initialize`, not from `.genroc`

`workspaceFolders` (or the older `rootUri`) is what a child action's process is resolved
against. `.genroc`'s `definitions:` answers a different question — which files an `apply`
deploys, not which exist — and reaching it would mean moving `cmd/genctl`'s project config out
of `package main` for nothing. specs/language-server.md §4.

Open buffers are searched **before** disk: the file on disk may be older than what the reader
is looking at, so renaming a process in the editor must not send them to the name it had. The
walk skips `.git`/`node_modules`/`dist`/`build`/`vendor` and caps at `maxScanned`, because a
workspace rooted somewhere enormous must not hang an editor.

## Positions are swept, not sampled

`tests/lsp/sweep_test.ts` is the file that would have caught what was reported from an editor.
The named tests beside it pick cursor positions by hand, and **every bug a real user found was
at a position nobody picked** — a `case`, a `goto`, a blank line under `input_schema:`. The
fixture already contained all three.

The sweep puts the cursor at every column of every line and asserts what must never happen. It
checks the completion **kind**, not labels: a name list cannot work, because `headers` is a key
on a fetch AND a member of that fetch's `self.result`, and a user's schema may name a field
anything. What is never ambiguous is which QUESTION the server answered — scope (Field), keys
(Property), or a closed set (Value).

Two of the sweeps are DIFFERENTIAL, and that is deliberate: an absolute assertion needs a list
of which mappings have keys to offer and which are open maps of the author's own names, and
that list would rot. Pressing Enter adds no key and removes none, so the new blank line must
answer as a sibling does; a list dash is beside its element's key, so it must answer as that
key does.

## The e2e suite is where the gaps showed up

`tests/lsp/` drives the real binary against one valid fixture, and names a position by quoting
the line it is on:

    <|>        the cursor is here
    <|text>    the cursor is here and `text` has NOT been typed yet — it is removed
    <^text>    the cursor is inside `text`, which stays

The fragment must appear exactly once in the document, or the helper refuses it: two
identically-written `goto: "$review"` lines are what that guard is for. `edit()` is the same
rule for diagnostics, which need a document that is wrong rather than a cursor.

Writing them found two things unit tests had not: a cursor on the blank line **below** the
last key (where the next key goes, and where nothing covers the position — `sameIndentAbove`),
and a task whose own poisoned output was re-reported through its own switch.

**A completion list is ordered by `sortText`, or it is alphabetical.** Alphabetical put
`$anchor` at the top of a list of JSON Schema keywords. Required keys come first, then
`schema.KeywordOrder` — the order a schema READS, which is what `genctl schema` prints in.

## Two editor defaults the protocol cannot set

An expression lives inside a quoted string, and VS Code suppresses suggestions there unless the
language says otherwise — so completion only appeared on Ctrl+Space. And word-based
suggestions rank beside the vocabulary the server actually knows. Both are
`configurationDefaults` in the extension, and `TestTheExtensionEnablesSuggestionsInsideStrings`
is what notices if they go.

## The extension is a launcher

`editors/vscode` starts `genctl lsp` and contributes a language id. It implements no analysis,
and must not: two implementations of "what is wrong with this file" is the defect this whole
spec was written against.

It launches one of two servers: the `genctl` on PATH, or — where there is none — the same
program as WebAssembly, bundled in the .vsix and started by `wasi.mjs` on VS Code's own Electron
(`ELECTRON_RUN_AS_NODE=1`, which is where `node:wasi` lives, so nothing extra is installed).
Three things make that work, and the third is the one to keep: genctl has no cgo and opens no
sockets, so `GOOS=wasip1` is a plain build; the server is one goroutine, and wasip1 is
single-threaded; and each workspace folder is preopened AT ITS OWN ABSOLUTE PATH, so a path
inside the module is the same string as outside it and the `file://` URIs need no translation.
`tests/lsp/wasm_test.ts` is differential against the binary — a fallback that quietly disagrees
is worse than no fallback. specs/language-server.md §4. `TestTheVSCodeExtensionMatchesTheFilesThisServerAnswersFor` is the
one thing holding the two halves together — they are different languages and neither imports
the other, so nothing else notices when one is edited and the other is not.

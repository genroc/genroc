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
only the cursor's line closed off (`"`, `}"`, `"}`, `: `). Repairing more would answer about a
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

`choose` picks among union arms: the document's `type` where there is one, else the arm that
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

## The extension is a launcher

`editors/vscode` starts `genctl lsp` and contributes a language id. It implements no analysis,
and must not: two implementations of "what is wrong with this file" is the defect this whole
spec was written against. `TestTheVSCodeExtensionMatchesTheFilesThisServerAnswersFor` is the
one thing holding the two halves together — they are different languages and neither imports
the other, so nothing else notices when one is edited and the other is not.

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

## The one shortcut

`DisallowUnknownFields` reports prose and stops at the first unknown key, so `decodeDiagnostic`
reads the key out of the message and finds it in the index — matching by SPAN, since every node
is addressable twice (physically and logically) and counting paths finds two of everything.
§5's reflection walk is what replaces it: every unknown key, with a path, in one pass.

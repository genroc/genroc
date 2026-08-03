# internal/schema

## The `unknown` type

"Unknown" — a value a process carries but never inspects — is spelled `{}`, the empty
schema. There is **no keyword**: `{}` is JSON Schema's top type and every code path
already handles it. Full design, ledger and open questions:
[specs/unknown-type.md](../../specs/unknown-type.md); worked example in
[examples/polling-task/](../../examples/polling-task/).

Three things to know before touching it:

1. **Do not add a spelling.** `type: unknown` was built and reverted. It would have
   been the only construct genroc accepts that a standard JSON Schema validator
   rejects — everything else here is *subsetting* (accepts less) plus one inert custom
   keyword (`secret`), which JSON Schema permits. That purity is what lets outside
   tooling generate and consume these schemas unmodified. And because such a keyword
   has to be erased at parse anyway, it never reaches the DB or the API, so it buys no
   explicitness where it counts. To say opacity is deliberate, use a YAML comment or a
   `description` (which `isEmptyNode` ignores, so `{"description": "…"}` is still the
   top type, and `canonicalizeNode` strips before comparison). The full argument is in
   the doc — read it before re-proposing.
2. **Only `result_schema` may narrow.** `Schema.NarrowsTo` (`accessors.go`) is `IsSubset`
   with the `isEmptyNode(sub)` rule flipped, and its sole caller is `checkChildOutputType`
   in `internal/validation`. That is sound only because collect conforms the child's actual
   output against the parent's `result_schema` (`resolveAndValidateChildOutput`) — a real
   runtime check standing behind the static claim. Do not reach for `NarrowsTo` in a slot
   with no such conform; an unknown flowing into a typed *input* is rejected on purpose.
3. **Unrecognised type names are rejected in `CheckDoc`, not the decoder** — the
   decoder also runs over rows already in the DB, and a legacy bad name must fail its
   own registration rather than become undecodable. `validTypes` is exactly the JSON
   Schema `simpleTypes` enum.

An **omitted** `result_schema` is a third state, not a synonym for unknown: the result
stays untyped and unexportable, so "I meant it to be opaque" stays distinguishable from
"I forgot". The `infer` mode in that doc (inherit the child's computed output) is
**not built**.

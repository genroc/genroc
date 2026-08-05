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

## `required` and nullable are independent, and one pair bridges them

`conformObject` rejects an absent required property whatever its type, while `evalMember`
returns null for a missing key. So `required` governs **documents at a boundary** and
nullability governs **reads** — two different questions, and nothing may quietly merge them.

One pair deliberately bridges the two, and it is a pair:

- `IsSubsetAbsentAsNull` decides a version gap is closable — super may require a nullable
  property that sub does not.
- `Validate(data, FillAbsentAsNull)` closes it, inserting the explicit null.

The second is a **mode on Validate**, not a function beside it, and it returns
`(any, error)` like every other conform. That matters: a migration that quietly handed back
the value it was given would let an upgrade report success over data that does not fit the
version it was moved to.

**They must accept exactly the same gaps.** A relation that tolerates more than the fill can
close promises a migration that then fails to conform; a fill that closes more is dead code.
`schematest/absent_test.go` asserts both directions over every gap shape — nested, array
items, open maps, `$ref`, recursive, inside a union variant — including that the filled value
passes a STRICT conform, which is the claim the whole thing rests on. It also pins the
properties that make a migration built on it safe: the fill is idempotent, only ever adds
(undeclared keys included, which a conform would strip), and preserves validity.

**There is ONE walk of schema-and-value, and the fill is a mode on it** (`ConformMode` in
`validate.go`), not a second traversal beside it. The rules about where a value lives
inside a schema — combinators before types, `$ref`s with a cycle guard, open maps versus
closed objects, unions picked by which branch the value actually satisfies — are subtle
enough that a parallel walker rediscovers them badly and then has to stay in step forever.
The first attempt here was exactly that, and it reached its type switch before consulting a
union's variants, so a union of objects silently filled nothing while the relation happily
accepted the gap.

The mode differs from `Strict` in three ways, all deliberate: an absent required nullable
is written in rather than rejected; undeclared keys are KEPT (stripping is a conform's job,
and a stale key from a dropped task is real data); and declared defaults are NOT filled, so
the walk closes exactly what the relation accepts and nothing more. Filling defaults would
unlock the required-with-default case — but only if the relation were taught to accept it in
the same change.

**A `required` name whose property is never declared** has no type to call nullable. Any map
index that misses hands `HasNull` the zero Schema, so `hasNullGuard` answers false rather
than panicking. Guarding at the call site instead relies on every future caller remembering,
and the one that did not crashed a live endpoint. Neither is a licence to relax
`required` anywhere else: at a slot with a runtime conform behind it, absence is still a
rejection.

An **omitted** `result_schema` is a third state, not a synonym for unknown: the result
stays untyped and unexportable, so "I meant it to be opaque" stays distinguishable from
"I forgot". The `infer` mode in that doc (inherit the child's computed output) is
**not built**.

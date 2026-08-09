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
- `Validate(data, ConformToSchemaExactly)` closes it, in both directions: inserting the
  explicit null, and removing a key whose stored null the target will not hold.

The second is a **mode on Validate**, not a function beside it, and it returns
`(any, error)` like every other conform. That matters: a migration that quietly handed back
the value it was given would let an upgrade report success over data that does not fit the
version it was moved to.

**They must accept exactly the same gaps.** A relation that tolerates more than the fill can
close promises a migration that then fails to conform; a fill that closes more is dead code.
`schematest/absent_test.go` asserts both directions over every gap shape — nested, array
items, open maps, `$ref`, recursive, inside a union variant — including that the filled value
passes a STRICT conform, which is the claim the whole thing rests on. `conform_exact_test.go`
does the same for the removal half. Both pin what makes a migration built on this safe:
idempotence, undeclared keys preserved (a conform would strip them), and validity after.

**Removal fires only where absence is a valid state and the null cannot stay** — an optional
declared property whose target type dropped `null`. Not on a required one (neither state is
valid, and the relation refuses); not on an array element (dropping shortens the array); and
never where the target is *also* nullable, since both states are valid, there is nothing to
reconcile, and removing would invent a canonical form the schema does not name. An open map's
key is removable in principle, but the rule only walks declared properties — so relation and
conform both refuse it, which is the safe direction of the pairing rather than a hole.

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
the walk closes exactly what the relation accepts and nothing more.

**`IsSubsetAsStored` is a third relation, not a loosening of the pair.** It reads both
schemas as descriptions of data a conform already produced, so a property the SUB side
declares with a default is guaranteed present — and that tolerates a gap with no fill behind
it, which sounds like exactly what the paragraph above forbids. It is not: there is nothing
to close, because creation filled the default into the row before anything read it. The case
that still needs a coordinated relation-and-fill change is the other side — a default only
the SUPER side carries, where a fill would have to write it — and `IsSubsetAsStored` refuses
that, pinned by `schematest/subset_stored_test.go`. Keep the two apart: folding the default
rule into `IsSubsetAbsentAsNull` would break its contract with the fill, which
`absent_test.go` exists to hold.

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

## Where sub-schemas live is a table, and it does not cover everything

`mapChildren` (`walk.go`) is the one enumeration of the keywords that hold sub-schemas;
`children` derives from it so the read and write sides cannot drift. Every purely
structural walk goes through it — `canonicalizeNode`, `stripDefsDeep`, `relaxToString`,
`checkDoc`, `collectBareRefs`, `dropBareSCCRefs` — and steers on `slotKind` rather than
re-deriving which slots matter: `slotBare` is the productivity rule (a union arm keeps the
value at this depth, so a `$ref` there makes no progress), `slotDefs` is a namespace and
not a position in the value at all. `walk_test.go` reflects over `node` and fails if a
sub-schema field is missing from the table, because every symptom of a missed slot is
silent — an unstripped `$defs` cycles the marshaler, an uncanonicalized subtree stops the
inference fixpoint converging.

**Three walks are deliberately outside it.** `conformGuard` and `subsetCtx.check` must pick
a union branch rather than descend into all of them, and `conformObject` sweeps
`Properties` hunting for what is *absent*, which no table-driven descent can see. Adding a
keyword means one edit in `walk.go` plus a decision in each of those three and in
`lookupPropertyGuard` — those four are semantics, not boilerplate. A schema-and-value walk
that is not one of them should descend via `lookupProperty`/`inferIndex` (as `redact` and
`collectSecrets` do) and never grow a keyword switch of its own.

`normalize`'s `walkTree` is **not** converted: it mutates nodes in place while holding
`*node` pointers into the tree across phases, and `mapChildren` copies.

**A `CheckDoc` error names the definition site, not an access path.** The location is
assembled from one `errLabel` per slot as the recursion unwinds, and a `$defs` entry is
checked once under its own name however many refs reach it — which is what keeps it finite.
Do not "improve" it into a path through the refs that reach a node: a recursive schema has
infinitely many, which is why `internal/validation`'s `explainer` needs `maxExplainDepth`.
An access path needs something to bound it, and each thing that can (a value, an
expression) already carries its own — `conformGuard`'s `path`, `Infer`'s `[]pathStep`.

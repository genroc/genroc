# Literal types

**Status: proposed, not implemented.** Prerequisite for
[discriminated-unions.md](discriminated-unions.md); useful alone.

Inference collapses every literal to its base type — `"sent"` infers as `string`; a
declared `enum` survives navigation but nothing ever produces one. Worth doing because:
it unblocks tagged unions; it catches provably-false comparisons
(`kind == "sucess"` against `enum: [success, failure]` silently never fires today); it
makes published schemas honest; and it is the precondition for any exhaustiveness
checking.

## Measured baseline

A spike (change the four literal cases in `inferNode` to carry their value): producing
literals is **4 lines**; 51 Go tests fail, mostly mechanical shape churn — but two
failures were substantive, and they are the actual content of this document.

## The blocker: unions of literals

`mergeSimpleVariants` refuses arms carrying enums, so the moment literals exist, arms
stop merging — and `?? false` infers
`oneOf[{boolean, enum:[false]}, {boolean}]`, where `false` matches **both** arms and
strict `oneOf` rejects the only interesting value it describes. The same defect
path-sensitive-output §3 fixed, reintroduced through a different door, landing on the
single most common idiom in the language. Also: `[1, 1.5]` fragments instead of
widening to `number` (documented behaviour with its own test). **Literal types cannot
ship without the merge rule; everything else is polish.**

## The merge rule

Admit enums into `isSimpleType` and merge enum-aware: if **every** arm carries an enum,
union the values (`{type: types, enum: dedupe(∪)}`); if **any** arm is bare, drop the
enums (`{type: types}`). The second branch closes the blocker — `?? false` comes out
`{type: boolean}`, today's answer — while `oneOf[{enum:[sent]}, {enum:[failed]}]`
merges to the tag type a discriminated union needs. Dropping enums is **widening**,
never narrowing (cannot accept a value the source could not be), and merge-widens is
already canonicalization's established rule (it drops `minimum`/`maxLength` when
folding). Enum values dedupe by canonical JSON and sort — or equal types stop comparing
equal and the recursive-output fixpoint loses its termination key.

## Number precision is not optional

Enum values decode via `numeric.Decode` precisely because a float64 collapse once
**inverted an enum** (a whitelist for 9007199254740993 rejected it and admitted its
neighbour). A literal type must carry `json.Number(n.Text)` — the exact source text,
never a parsed float; the spike erred the other way too (stored the text as a *string*
enum value, so numbers compared as strings). Both mistakes are easy; the tests pin the
representation. See [number-precision.md](number-precision.md).

## What needs no change (verified)

`IsSubset`/`NarrowsTo` (`checkEnum` already gets both directions right — a child's
literal output still satisfies a wider `result_schema`; the most reassuring fact here);
runtime enum enforcement; arithmetic/comparison (`concreteTypeOf` reads `Type()`);
`schemasEqual` (marshal-based). Also to touch, same pass: `inferNullCoalesce`'s numeric
branch (drops enums — precision only), and object/array literal inference (inherits the
merge automatically, owns most of the test churn).

## Sequencing

**Land the merge rule first, alone, before any literal is produced** — it is a latent
correctness fix today (hand-written overlapping unions canonicalize unsatisfiably), it
tests with zero churn, and it means the later 4-line flip produces pure shape churn
instead of churn mixed with breakage. Then flip `inferNode`, absorb, regenerate
published schemas.

## No widening rule needed

TypeScript widens `let x = "a"` because bindings mutate; genroc has no mutable bindings
— a literal re-evaluates identically on every run, so **every literal stays a
singleton** and none of the `const`/`as const` machinery exists. One of the few places
genroc's model is simpler. Consequence is churn, not risk: `{status: "ok"}` publishes
as `enum: ["ok"]` — more accurate, and visible.

## Test plan

Merge rule first and heaviest (all-arms-enum unions; any-arm-bare drops; `?? false`
stable; hand-written overlapping unions become satisfiable; dedupe/order stable). Then
the property that would have caught both this and the null bug: **for each inferred
union, every value it describes validates against it** — assert the property, not the
JSON. Precision round-trips past 2^53. Subset end to end. `[1, 1.5]` still widens.
Examples register with unchanged semantics.

## Open questions

Publish literals or widen at the boundary (accuracy + churn vs a published schema that
disagrees with the internal type)? Impossible comparisons: error or warning (erroring
is useful but fails currently-registering definitions)? Propagation: `"a" + "b"` as
`enum: ["ab"]` — constant folding is a slippery slope; default no.

## Prior art

TypeScript's literal types, with two simplifications here: no widening (no mutable
bindings), and singletons spelled `enum: [sent]` — `const` is not in the keyword
allowlist, keeping schemas plain JSON Schema outside tooling can consume, the
unknown-type discipline again.

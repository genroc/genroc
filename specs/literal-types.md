# Literal types

**Status: proposed, not implemented.** Unblocks discriminant narrowing (§9); useful alone.

Inference collapses every literal to its base type — `"sent"` infers as `string`; a
declared `enum` survives navigation but nothing ever produces one. Worth doing because:
it unblocks tagged unions; it catches provably-false comparisons
(`kind == "sucess"` against `enum: [success, failure]` silently never fires today); it
makes published schemas honest; and it is the precondition for any exhaustiveness
checking.

## 1. Where literals are typed

Three sites, and the use case lives in the two a spike would miss:

- `inferNode`'s four literal cases (`internal/schema/infer.go`) — `$:` expressions and `${}`
  interpolations only.
- `template.InferType`'s no-interpolation path (`internal/template/template.go`) — a shape string
  such as `output: {kind: sent}` lands here, via `shape.Infer`, and returns `string`.
- `shape.Infer`'s `bool` / `json.Number` / `nil` cases (`internal/shape/infer.go`) — shape scalars.

A spike that flipped only `inferNode` failed 51 Go tests, mostly mechanical shape churn — but two
failures were substantive, and they are §2 and §4. It typed no shape literal, so its churn count is
a floor.

## 2. The blocker: unions of literals

`mergeSimpleVariants` refuses arms carrying enums, so the moment literals exist, arms
stop merging — and `?? false` infers
`oneOf[{boolean, enum:[false]}, {boolean}]`, where `false` matches **both** arms and
strict `oneOf` rejects the only interesting value it describes. The same defect
path-sensitive-output §3 fixed, reintroduced through a different door, landing on the
single most common idiom in the language. Also: `[1, 1.5]` fragments instead of
widening to `number` (documented behaviour with its own test). **Literal types cannot
ship without the merge rule; everything else is polish.**

## 3. The merge rule

Admit enums into `isSimpleType` and merge enum-aware: if **every** arm carries an enum,
union the values (`{type: types, enum: dedupe(∪)}`); if **any** arm is bare, drop the
enums (`{type: types}`). The second branch closes the blocker — `?? false` comes out
`{type: boolean}`, today's answer — while `oneOf[{enum:[sent]}, {enum:[failed]}]`
merges to the tag type a discriminated union needs. Dropping enums is **widening**,
never narrowing (cannot accept a value the source could not be), and merge-widens is
already canonicalization's established rule (it drops `minimum`/`maxLength` when
folding). Enum values dedupe by canonical JSON and sort — or equal types stop comparing
equal and the recursive-output fixpoint loses its termination key.

## 4. Number precision is not optional

Enum values decode via `numeric.Decode` precisely because a float64 collapse once
**inverted an enum** (a whitelist for 9007199254740993 rejected it and admitted its
neighbour). A literal's enum value is the exact source text as a `json.Number`: `IntNode.Text` /
`FloatNode.Text` for an expression, and for a shape the `json.Number` it already holds (`Shape`
decodes through `numeric.Decode`, i.e. `UseNumber`). Never a parsed float, and never the text as a
*string* enum value (the spike did this, so numbers compared as strings). The tests pin the
representation. See [number-precision.md](number-precision.md).

## 5. What needs no change (verified)

`IsSubset`/`NarrowsTo` (`checkEnum` already gets both directions right — a child's
literal output still satisfies a wider `result_schema`; the most reassuring fact here);
runtime enum enforcement; arithmetic/comparison (`concreteTypeOf` reads `Type()`);
`schemasEqual` (marshal-based). Also to touch, same pass: `inferNullCoalesce`'s numeric
branch (drops enums — precision only), and object/array literal inference (inherits the
merge automatically, owns most of the test churn).

## 6. Sequencing

**Land the merge rule first, alone, before any literal is produced** — it is a latent
correctness fix today (hand-written overlapping unions canonicalize unsatisfiably), it
tests with zero churn, and it means the later flip produces pure shape churn instead of
churn mixed with breakage. Then flip all three sites of §1 together, absorb, regenerate
published schemas.

## 7. No widening rule needed

genroc has no mutable bindings — a literal re-evaluates identically on every run, so
**every literal stays a singleton** and no `const`/`as const` machinery is needed.
Consequence is churn, not risk: `{status: "ok"}` publishes as `enum: ["ok"]` — more
accurate, and visible. Singletons are spelled `enum: [sent]`: `const` is not in the keyword
allowlist, keeping schemas plain JSON Schema.

## 8. Test plan

Merge rule first and heaviest (all-arms-enum unions; any-arm-bare drops; `?? false`
stable; hand-written overlapping unions become satisfiable; dedupe/order stable). Then
the property that would have caught both this and the null bug: **for each inferred
union, every value it describes validates against it** — assert the property, not the
JSON. Precision round-trips past 2^53, through both an expression and a shape. Subset end
to end. `[1, 1.5]` still widens. Examples register with unchanged semantics.

## 9. What it unblocks: discriminant narrowing

A new guard for [guard-narrowing.md](guard-narrowing.md): `X.d == lit` keeps the arms of `X` whose
`d` admits `lit` (fall-through keeps the rest; `!=` is the negation). **The refinement applies to
`X`, not `X.d`** — refining the tag alone does nothing useful.

- **Usable only when** `X` is a `oneOf` of objects (after `$ref` resolution), every arm declares
  `d` required (an omitting arm survives every selection), and each arm's `d` is a single-valued
  scalar enum. Disjointness is not required — two arms with one tag both survive, still sound.
  Otherwise the guard narrows nothing, like any unrecognised guard.
- **Reading another arm's field becomes an error** after selection, where today it is
  `T|null`. An intended breaking change: it breaks only definitions that read across arms after
  proving which arm they hold.
- **Termination is bounded by height**, not the powerset: refinements only shrink along an edge
  and merges union subsets, so chains are ≤ n+1 for n arms.
- **Second use:** refining a fetch's `self.result` by `self.status` across status-keyed
  `responses` ([fetch-http-surface.md](fetch-http-surface.md) "Neither channel needs narrowing").

## Open questions

Publish literals or widen at the boundary (accuracy + churn vs a published schema that
disagrees with the internal type)? Impossible comparisons: error or warning (erroring
is useful but fails currently-registering definitions)? Propagation: `"a" + "b"` as
`enum: ["ab"]` — constant folding is a slippery slope; default no. An unusable
discriminant: silent or loud (leaning loud — the author clearly tried)?

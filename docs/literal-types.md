# Literal types

**Status: proposed, not implemented.** Prerequisite for
[discriminated-unions.md](discriminated-unions.md), useful on its own.

Inference collapses every literal to its base type: the expression `"sent"` infers as
`{type: string}`, losing *which* string it was. A declared `enum` survives navigation — a
property node is returned as declared — but nothing in inference ever **produces** one.

```
"sent"                              →  {type: string}
kind == "sent"                       →  {type: boolean}
self.result.kind  (enum: [a, b])     →  {type: string, enum: [a, b]}   ← declared, carried
```

## 1. Why it is worth doing

- **It unblocks tagged unions.** A definition cannot currently build a narrowable
  discriminated union, because `output: {kind: sent}` types `kind` as plain `string`, so
  two coalesced branches produce arms tagged `string` and `string` — distinguishable by
  shape, never by tag. See [discriminated-unions.md §0](discriminated-unions.md).
- **It catches impossible comparisons.** `case: 'self.output.kind == "sucess"'` against a
  declared `enum: [success, failure]` is a condition that can never be true. With literal
  types that is a registration error; today it type-checks as `boolean` and silently never
  fires.
- **It makes published schemas honest.** A field that is always `"ok"` currently publishes
  as "any string".
- **It is the precondition for exhaustiveness checking** of any kind (§9).

## 2. Measured baseline

A throwaway spike — change the four literal cases in `inferNode`
([internal/schema/infer.go:259](../internal/schema/infer.go#L259)) to carry their value,
build, run the suite:

- **Producing literals is 4 lines.**
- **51 Go tests fail** across `expressiontest`, `schematest` and `validationtest`. Most are
  mechanical (expected JSON now carries an `enum`), but not all.
- **The integration suite and published schemas were not measured** — `openapi.json`, the
  editor JSON schema and `tests/generated/api.ts` all change too.

Two failures were substantive, and they are the actual content of this document.

## 3. The blocker: unions of literals

`mergeSimpleVariants` ([internal/schema/canonical.go](../internal/schema/canonical.go))
folds a union of primitive arms into one type array, but `isSimpleType` requires
`len(s.Enum) == 0`. So the moment literals carry enums, arms stop merging:

**Unsatisfiable schemas.** The spike produced this from `outputs.a.v ?? outputs.b.v ?? false`:

```json
{"oneOf": [{"type": "boolean", "enum": [false]}, {"type": "boolean"}]}
```

`false` matches **both** arms, and `oneOf` means exactly one
([validate.go](../internal/schema/validate.go), `conformUnion`), so this schema rejects the
only interesting value it describes. It is the same defect fixed in
[path-sensitive-output.md §3](path-sensitive-output.md), reintroduced through a different
door — and it lands on `?? default`, the single most common idiom in the language.

**Lost widening.** `[1, 1.5]` currently infers `items: {type: number}`; under the spike it
fragments into a two-arm union. That is documented behaviour with its own test.

**Literal types cannot ship without fixing this.** Everything else here is optional
polish.

## 4. The merge rule

Relax `isSimpleType` to admit `Enum`, and give `mergeSimpleVariants` an enum-aware result:

```
merge(arms):                       -- all arms primitive, no properties/items/$ref
  types := ∪ arm.Type
  if every arm carries an enum:
      return {type: types, enum: dedupe(∪ arm.Enum)}
  return {type: types}             -- one arm is unconstrained: the union is too
```

The second branch is what closes §3. `oneOf[{boolean, enum:[false]}, {boolean}]` has an arm
with no enum, so the enums drop and the result is `{type: boolean}` — satisfiable, and
identical to today's answer for `?? false`. Meanwhile
`oneOf[{string, enum:[sent]}, {string, enum:[failed]}]` merges to
`{type: string, enum: [sent, failed]}`, which is exactly the tag type a discriminated union
needs at a join.

Dropping the enums is **widening**, never narrowing, so it can only lose precision — it
cannot make an inferred type accept something the value could not be. Canonicalization
already works this way: `mergeSimpleVariants` silently drops `minimum`/`maxLength` and
friends when folding arms, so "merge widens" is the established rule here, not a new
concession.

Enum values must be **deduplicated by canonical JSON** (`jsonKey`, as `checkEnum` already
does) and sorted, or two equal types stop comparing equal and the recursive-output fixpoint
loses its termination key.

## 5. Number precision is not optional

Enum values are decoded with `numeric.Decode` specifically so they are **not** collapsed to
`float64`. The comment at [schema.go:155](../internal/schema/schema.go#L155) records why:

> a plain Unmarshal collapses them to float64 — which corrupted a default past 2^53, and
> **inverted an enum**: a whitelist declared for 9007199254740993 rejected that value and
> admitted its neighbour instead.

`IntNode` and `FloatNode` carry their exact source text (`Text string`) for the same reason.
A literal type built from them must preserve that text — `json.Number(n.Text)`, never a
parsed float — or literal types reintroduce the exact bug that comment exists to prevent.
See [number-precision.md](number-precision.md).

My spike got this wrong in the other direction (it stored the *text* `"1"` as a string enum
value, so a numeric literal compared as a string). Both mistakes are easy; the tests in §8
should pin the representation down.

## 6. What needs no change

Verified, not assumed:

| Machinery | Why it is unaffected |
|---|---|
| `IsSubset` / `NarrowsTo` | `checkEnum` ([subset.go:274](../internal/schema/subset.go#L274)) already gets both directions right: `enum:[sent]` **is** a subset of `{type:string}`; the reverse is refused. A child emitting a literal still satisfies a parent's wider `result_schema`. |
| Runtime validation | `enum` is already enforced ([validate.go:77](../internal/schema/validate.go#L77)). |
| Arithmetic / comparison | `concreteTypeOf` ([inferops.go:294](../internal/schema/inferops.go#L294)) reads `Type()`, which an enum-carrying node still sets. |
| `schemasEqual` | Marshal-based, so it distinguishes literals without help. |

That `IsSubset` needs nothing is the single most reassuring fact here: the compatibility
checks that gate child processes and `result_schema` narrowing keep working unchanged.

## 7. What else to touch

- **`inferNode`** — the four literal cases (§2).
- **`inferNullCoalesce`'s numeric branch** returns `Type(lct)`, discarding the enums, so
  `5 ?? 0` widens to `integer` rather than `enum: [5, 0]`. Precision only, not soundness;
  worth fixing in the same pass for consistency with §4.
- **Object and array literal inference** (`inferLiteral`) composes element types, so it
  inherits the merge rule automatically — but it has the largest test surface, and several
  of the 51 failures live there.

## 8. Sequencing

**Do §4 first, on its own, before any literal is produced.** The enum-aware merge is a
latent correctness improvement today: a hand-written schema can already contain
`oneOf[{string, enum:[a]}, {type:string}]`, and canonicalization currently leaves that
unsatisfiable union intact. Landing it separately means:

- it can be tested against hand-written schemas with no churn at all;
- when the four lines flip, the failure set is pure shape churn rather than a mix of churn
  and real breakage, which makes 51 test updates reviewable instead of dangerous.

Then flip `inferNode`, absorb the churn, regenerate the published schemas.

## 9. No widening rule needed

TypeScript widens `let x = "a"` to `string` because the binding is mutable and pinning it to
its initial value would be wrong. genroc has no mutable bindings: a literal in a definition
is constant and re-evaluates to the same value on every run, including every loop iteration.
So **every literal stays a singleton**, and none of TypeScript's `const`/`let`/`as const`
machinery is needed. This is one of the few places genroc's model is simpler.

The consequence is churn, not risk: `output: {status: "ok"}` publishes
`{type: string, enum: ["ok"]}` to every consumer of the generated schemas. More accurate,
and visible.

## 10. Test plan

- **Merge rule (heaviest, lands first)**: all-arms-enum unions their values; any-arm-bare
  drops enums; `?? false` stays `{type: boolean}`; a hand-written overlapping union
  canonicalizes to something satisfiable; dedupe and ordering are stable.
- **Satisfiability, differentially**: for each inferred union, every value it describes
  validates against it. This is the property that would have caught both the null bug and
  this one; assert the property, not the JSON shape.
- **Number precision**: a literal past 2^53 keeps its exact value through inference,
  canonicalization and marshalling; a numeric literal does not become a string enum value.
- **Subset**: `enum:[sent]` satisfies a declared `{type:string}` slot end to end — a child
  with a literal output against a parent's wider `result_schema`.
- **Widening preserved**: `[1, 1.5]` still infers `number`.
- **No regression**: every example still registers with unchanged *semantics*, and the
  path-sensitive output tests still pass.

## 11. Open questions

- **Should literals appear in published schemas at all, or be widened at the boundary?**
  Keeping them is more accurate and costs churn; widening only at publication keeps the API
  stable but means the published schema and the internal type disagree, which has its own
  cost when debugging.
- **Should an impossible comparison be an error or a warning?** `kind == "sucess"` against
  `enum: [success, failure]` is provably always false. Erroring is the useful behaviour, but
  it turns a class of currently-registering definitions into failures.
- **How far do literals propagate?** `"a" + "b"` could infer `enum: ["ab"]`. Constant
  folding in the type system is a slippery slope with little payoff; the default answer
  should be no.

## 12. Prior art

TypeScript's literal types are the model, and the two differences both simplify things here.

TypeScript needs `const` vs `let` widening because bindings are mutable; genroc does not
(§9). TypeScript spells a singleton as its own type (`"sent"`); genroc has no `const`
keyword in the supported subset — the allowed-keyword whitelist
([schema.go:78](../internal/schema/schema.go#L78)) admits `enum` but not `const` — so a
singleton is `enum: [sent]`, a one-element whitelist. That is a spelling difference, not a
semantic one, and it keeps the schema a plain JSON Schema that outside tooling can consume
unmodified — the same discipline as [unknown-type.md](unknown-type.md).

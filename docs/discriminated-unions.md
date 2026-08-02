# Discriminated unions and discriminant narrowing

**Status: deferred — blocked on literal types.** Not scheduled. The mechanism below is
sound and would work against a hand-declared union, but the use case that justifies it
(§6) is unreachable until inference carries literal values, so building it now would ship
an island of precision with nothing to point it at. See §0 before reading further.

Depends on [guard-narrowing.md](guard-narrowing.md) — that document owns the mechanism
(edges carry refinements, merged per reference); this one adds **one guard shape** and
**one refinement kind**, and answers the questions that only arise for union types.
Guard narrowing itself is *not* blocked by this: null narrowing needs no literal types.

## 0. Why this is deferred

Inference has no literal (singleton) types. Measured:

| Expression | Inferred today |
|---|---|
| a property declared `enum: [success, failure]` | `{type: string, enum: [success, failure]}` — carried |
| the literal `"success"` in an output | `{type: string}` — literal-ness lost |
| `kind == "success"` | `{type: boolean}` |

A declared enum **survives** navigation, because a property node is returned as declared.
But inference never **produces** one: a string literal in a task output is just `string`.

The consequence is not that narrowing cannot work — it could, by matching the guard's
syntactic literal against each arm's declared `enum`. It is that a definition can never
*build* a narrowable union. `output: {kind: sent, …}` types `kind` as plain `string`, so
coalescing two branches (§6) yields arms tagged `string` and `string`, discriminated by
shape rather than by tag, and no tag guard can select among them. Only a hand-written
`result_schema` would ever be narrowable.

**Prerequisite:** literal/singleton types in inference — a string literal inferring as
`{type: string, enum: [sent]}`, and unions of them merging rather than fragmenting.
Specified in [literal-types.md](literal-types.md); it is a change to the type system's
precision generally, not a sub-task of this feature. Revisit this document once that lands.

## 1. Where we stand today

The type is expressible. Consuming it is not.

```yaml
result_schema:
  oneOf:
    - type: object
      properties:
        kind: { type: string, enum: [success] }
        data: { type: string }
      required: [kind, data]
    - type: object
      properties:
        kind: { type: string, enum: [failure] }
        error: { type: string }
      required: [kind, error]
```

That registers. Reading through it also works — `outputs.a.r.data` does **not** error, because
`lookupPropertyGuard` walks the variants, finds `data` in one and misses it in the other, and
returns the union of what it found *plus null* ([navigate.go](../internal/schema/navigate.go),
the `hadMiss`/`hadNull` path). The inferred type is `string|null`.

And a `case` testing the discriminant changes nothing:

```yaml
switch:
  - case: 'self.output.r.kind == "success"'   # ignored
    goto: $b
```

Task `b` still sees `string|null` and its author still writes `?? ""` — a fallback for a value
the definition just proved is present. That is the same complaint as guard-narrowing §1, one
level up: the union tells you *which shape you have*, and nothing reads the answer.

**`const` is not available.** The allowed-keyword whitelist
([schema.go:78](../internal/schema/schema.go#L78)) admits `enum` but not `const`, so a
discriminant is spelled `enum: [success]` — a single-element enum. This is a deliberate
subset, not an oversight; the spec below is written against it.

## 2. The guard shape

One new catalogue entry for guard-narrowing §3:

| Guard | On the taken edge | On the fall-through |
|---|---|---|
| `X.<d> == <literal>` | `X` keeps only the arms whose `<d>` admits the literal | `X` keeps only the arms whose `<d>` does *not* admit it |
| `X.<d> != <literal>` | the negation of the above | |

Note what is refined: the guard talks about `X.<d>`, but the refinement applies to **`X`**.
That is the difference from null narrowing, where the subject of the guard and the subject of
the refinement are the same reference. An implementation that refines `X.kind` and leaves `X`
alone has done nothing useful.

The refinement kind is **arm selection** — a subset of the declared `oneOf` variants —
rather than "remove null".

## 3. What makes a discriminant usable

`X.<d>` is a usable discriminant when, for the declared union:

1. **`X` is a `oneOf`** (after `$ref` resolution) whose variants are objects.
2. **Every variant declares `<d>`**, and declares it `required`. A variant that omits it can
   never be excluded by the guard, so it must survive every selection — which makes the
   narrowing useless rather than unsound, and is better refused with a message.
3. **Each variant's `<d>` is a single-valued `enum`** of a scalar.

Disjointness across variants is *not* required. Two variants tagged `success` simply both
survive the guard, and the refinement is the two-arm union. That is still sound and still an
improvement; requiring disjointness would reject a legitimate type for no safety gain.

When any condition fails the guard narrows nothing, exactly like an unrecognised shape.
Whether that should be silent or a registration warning is open (§8).

## 4. What narrowing buys, including one stricter outcome

After `X.kind == "success"` selects the first arm:

- `X.data` infers as `string` — the null contributed by the *missing* arm is gone, because
  that arm is gone.
- `X.kind` infers as the single-valued enum, not `string`.
- **`X.error` becomes an error** — "field not found" — where today it is `string|null`.

That last one is a genuine behaviour change on the narrowed path, and it is the *point*:
reading the failure arm's field after proving you are on the success arm is a bug, and the
narrowed type is what lets the checker say so. But it means adding this feature can make a
previously-accepted definition fail to register. It is a breaking change for any definition
that reads across arms after a discriminant check — which is exactly the class of definition
that was already wrong. Worth a release note, not a compatibility shim.

## 5. Strict `oneOf` is an asset here

genroc's validator implements `oneOf` as **exactly one match**
([validate.go](../internal/schema/validate.go), `conformUnion` with `exactlyOne`). For a
discriminated union with disjoint tags that is precisely right: one arm matches, the value
conforms, and a value carrying an unknown tag is rejected at the boundary rather than
silently taking a branch.

It is also why the overlapping-union bug fixed in
[path-sensitive-output.md §3](path-sensitive-output.md) mattered — an inferred union whose
arms overlap is unsatisfiable under these semantics. Discriminated unions are the shape that
*works* with strict `oneOf`, which is an argument for supporting them properly rather than
for loosening the validator.

## 6. The producer side — the use case, and why it does not work yet

**This section is the reason the document is deferred (§0).** It describes what *should*
be the natural way to produce a discriminated union, and does not currently hold.

The idea is to let two branches produce differently-shaped outputs and coalesce them:

```yaml
- id: sent
  output: { kind: sent,       delivery_id: "$: self.result.delivery_id" }
  switch: end
- id: unsendable
  output: { kind: unsendable, reason: "$: error.code" }
  switch: end

output:
  result: "$: outputs.sent ?? outputs.unsendable"
```

Two of the three pieces are already there. `??` of two object types builds `oneOf[A, B]`
(objects do not merge into a type array), and since
[path-sensitive output inference](path-sensitive-output.md) landed, that union has **no null
arm** when the two tasks cover every terminal.

The missing piece is the tag. `kind: sent` infers as `{type: string}`, not
`{enum: [sent]}` (§0), so the union comes out as two arms whose tags are **both** plain
`string`. They remain distinguishable by *shape* — arm A requires `delivery_id`, arm B
requires `reason`, so `conformUnion` still picks exactly one at runtime — but they are not
distinguishable by *tag*, and a `kind == "sent"` guard cannot select between them: both arms
admit every string.

So a definition can emit a union, but not a **discriminated** one. Only a hand-written
`result_schema` carrying `enum: [sent]` per arm is narrowable, which is a much narrower
feature than this section was written to describe.

The destination is still worth recording, because it is what makes the whole thing worth
building. It is the TypeScript answer to the correlation problem, expressed in genroc:
instead of the consumer coalescing two independently-optional fields, the producer emits one
tagged value and the consumer switches on the tag.
[examples/batch-invoices](../examples/batch-invoices/) is written the first way; once literal
types exist the child could be rewritten the second way, and the `{ok, reason}` convention
from [child-error-handling.md](child-error-handling.md) §0 becomes a tagged union rather than
a pair of parallel fields.

**Sequencing, then:** literal types → this. Not this on its own.

## 7. Termination

guard-narrowing §3 argues that the refinement lattice must stay short. Arm selection widens
it: a reference's refinement is now a *subset* of its declared arms, so the domain is a
powerset.

The **height** is what bounds the fixpoint, not the size, and height here is the number of
arms in the declared type — refinements only ever shrink along an edge, and merges take unions
of subsets, so any chain is at most `n+1` long for an `n`-arm union. `n` is fixed by the
declaration and small. Termination is safe; the note in guard-narrowing §3 should be read as
"justify the height", which this does.

## 8. Open questions

- **Silent or loud when the discriminant is unusable?** A guard on a property that is not a
  valid discriminant (§3) narrows nothing today. Saying so at registration would help the
  author who thought they had written a tagged union; staying silent matches how every other
  unrecognised guard behaves. Leaning loud, because the author has clearly *tried*.
- **Exhaustiveness.** Should a `switch` covering every arm of a union be checkable — the
  TypeScript `never` trick? [error-extensions.md §X3](error-extensions.md) already argues
  opt-in exhaustiveness for a child's raise set and declines it by default; the same argument
  probably applies here, and the two should be decided together rather than acquiring
  different opt-in spellings.
- **Should the narrowed type appear in the published schema?** Same question as
  guard-narrowing §15, same reason it is hard: one task's context would differ per incoming
  edge.
- **Nested discriminants** (`X.meta.kind == "…"`). The mechanism generalises — the refinement
  subject is still `X` — but the well-formedness check in §3 has to walk a path rather than a
  property, and every variant must agree on the path's shape.

## 9. Non-goals

- **No inference of a discriminant.** The author names it in the guard; genroc does not go
  looking for which property happens to be a tag.
- **No narrowing on non-literal comparison.** `X.kind == input.wanted` proves nothing
  statically.
- **No new syntax.** This is a type-system feature over `oneOf` + `enum`, both of which are
  already in the supported subset — the same discipline as
  [unknown-type.md](unknown-type.md), where the answer was "`{}` already means this".

## 10. Prior art

TypeScript's discriminated unions are the direct model: a union of object types sharing a
literal-typed property, narrowed by `===` against a literal, with `never` for exhaustiveness.

Two differences worth carrying into the design:

TypeScript requires the discriminant to be a **literal type** in each arm, which
`enum: [value]` reproduces exactly — so §3's rules are that requirement transcribed into the
supported subset, not an invention.

TypeScript narrows within a lexical scope; here the guard and its consumer are in different
tasks, so everything in guard-narrowing §4 (frame translation) and §8 (loop kills) applies
unchanged. In particular a discriminated union read from `config` cannot be narrowed, for the
same reason nothing from `config` can be: it is re-resolved every tick.

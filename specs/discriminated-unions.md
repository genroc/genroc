# Discriminated unions and discriminant narrowing

**Status: deferred — blocked on literal types.** Not scheduled. The mechanism is sound
against a hand-declared union, but the use case that justifies it (§6) is unreachable
until inference carries literal values — building now ships an island of precision with
nothing to point it at. Depends on [guard-narrowing.md](guard-narrowing.md) (which owns
the edge/refinement mechanism and is NOT blocked by this); this adds one guard shape and
one refinement kind.

## 0. Why deferred

A declared `enum` survives navigation, but inference never *produces* one: the literal
`"sent"` infers as plain `string`. So `output: {kind: sent}` coalesced across branches
yields arms tagged `string` and `string` — distinguishable by shape, never by tag — and
no tag guard can select among them. Only a hand-written `result_schema` is narrowable.
Prerequisite: [literal-types.md](literal-types.md) (a general precision change, not a
sub-task of this feature).

## 1. Where we stand

The type is expressible today (a `oneOf` of objects whose `kind` is a single-element
`enum` — `const` is deliberately not in the keyword allowlist); reading through it works
(`lookupPropertyGuard` unions across arms plus null); and a discriminant `case` changes
nothing — the author writes `?? ""` for a value the definition just proved present.
Guard-narrowing §1's complaint, one level up.

## 2–3. The guard, and what makes a discriminant usable

New catalogue entry: `X.<d> == <literal>` keeps only the arms whose `<d>` admits the
literal (fall-through keeps the rest; `!=` is the negation). **The guard talks about
`X.<d>` but the refinement applies to `X`** — refining `X.kind` and leaving `X` alone
does nothing useful. The refinement kind is arm selection, a subset of declared arms.

Usable iff: `X` is a `oneOf` of objects (after `$ref` resolution); every variant
declares `<d>` **required** (an omitting variant survives every selection — useless
rather than unsound, better refused); each variant's `<d>` is a single-valued scalar
enum. Disjointness is NOT required — two arms tagged the same both survive, still sound.
Any failed condition narrows nothing, like any unrecognised guard.

## 4–5. What it buys, and strict `oneOf`

After selection: the missing-arm null is gone, the tag becomes its singleton — and
**reading the other arm's field becomes an error** where today it is `string|null`.
That last is the point and a deliberate breaking change: it only breaks definitions
that read across arms after proving which arm they hold, i.e. the ones already wrong.
Release note, not a compatibility shim. genroc's strict exactly-one `oneOf` is an asset
here — disjoint tags conform cleanly and unknown tags are rejected at the boundary —
an argument for supporting unions properly, not loosening the validator.

## 6. The producer side — the blocked use case

Two branches producing differently-shaped outputs, coalesced by `?? `, should be the
natural way to build a tagged union — and two of three pieces exist (`??` of two
objects builds `oneOf[A, B]`; path-sensitive inference removes the null arm when the
branches cover every terminal). The missing piece is the tag (§0). The destination is
worth recording: the producer emits one tagged value and the consumer switches on the
tag — the TypeScript answer to the correlation problem, and what turns
child-error-handling §0's `{ok, reason}` convention into a real union. Sequencing:
literal types → this. Never this alone.

## 7. Termination

Arm selection widens guard-narrowing's lattice to a powerset, but the fixpoint is
bounded by **height**, not size: refinements only shrink along an edge and merges union
subsets, so chains are ≤ n+1 for an n-arm declared union. Safe; guard-narrowing §3's
note reads "justify the height", which this does.

## 8–9. Open, and non-goals

Open: silent vs loud when a discriminant is unusable (leaning loud — the author clearly
tried); exhaustiveness over arms (decide together with error-extensions §X3, not with a
second opt-in spelling); publishing narrowed types (same per-edge problem as
guard-narrowing); nested discriminants (`X.meta.kind` — mechanism generalises, the
well-formedness check must walk a path all variants agree on). Non-goals: no
discriminant inference (the author names it); no narrowing on non-literal comparison;
**no new syntax** — a type-system feature over `oneOf` + `enum`, the unknown-type
discipline again.

## 10. Prior art

TypeScript's discriminated unions are the model. `enum: [value]` transcribes its
literal-discriminant requirement into the supported subset; and unlike TypeScript's
lexical scope, guard and consumer live in different frames, so guard-narrowing's
translation and loop kills apply unchanged — including `config` being unnarrowable.

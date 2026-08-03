# Guard narrowing

**Status: proposed, not implemented.** Nothing in this document describes current
behaviour. Companion to [path-sensitive-output.md](path-sensitive-output.md), which is
implemented.

## 1. The problem

A definition can prove a value is non-null and route on that proof, and the type system
still refuses to let the routed task use it:

```yaml
- id: a
  action:
    type: fetch
    url: "…"
    result_schema:
      type: object
      properties: { v: { type: [integer, "null"] } }
      required: [v]
  output: { v: "$: self.result.v" }
  switch:
    - case: "self.output.v != null"     # the proof
      goto: $b
    - goto: end
- id: b
  output: { doubled: "$: outputs.a.v * 2" }
  switch: end
```

```
task "b" output.doubled: operator requires non-nullable operands
```

The `case` expression is discarded once it has selected an edge. Its information — the only
reason task `b` is running at all — never reaches `b`'s context. The author's recourse is
`outputs.a.v ?? 0`, a fallback that provably cannot be evaluated, written only to satisfy
the checker.

This is an odd asymmetry in a language that type-checks everything else: a `switch` case is
the one expression whose meaning the type system ignores.

## 2. What this proposes

Carry a **refinement** — a narrowed type for one reference — along the edge that a `switch`
case selects, and apply it in the target task's context.

Scope discipline is the whole design. A refinement is about **one reference at a time**.
Relating two references ("a is set here, b is set there") is a different and much harder
problem, deliberately out of scope: see [path-sensitive-output.md §5](path-sensitive-output.md).

## 3. The guard catalogue

Only a fixed, small set of `case` shapes narrow. Everything else routes as it does today
and narrows nothing. TypeScript works the same way — a closed list of narrowing constructs,
not general reasoning about predicates.

| Guard | On the taken edge | On the fall-through |
|---|---|---|
| `X != null` | `X` loses null | `X` is exactly null |
| `X == null` | `X` is exactly null | `X` loses null |
| `!G` | negation of `G` | `G` |
| `G1 && G2` | both refinements | nothing (see below) |

`G1 \|\| G2` narrows nothing on the taken edge in v1: soundly it is the *join* of the two
refinements, which is only useful when both refine the same reference, and it is not worth
the code in a first cut.

`X.<d> == <literal>` — the **discriminant** guard, which selects arms of a `oneOf` rather than
removing a null — is specified separately in
[discriminated-unions.md](discriminated-unions.md) and is **deferred**: it reuses everything
here, but a definition cannot yet build a union that a tag guard could select among, because
inference has no literal types. Nothing in this document waits on it — null narrowing needs
no literal types — so v1 proceeds independently.

`&&` yields nothing on the fall-through: the negation of a conjunction is a disjunction, and
"at least one of these is null" is not a per-reference fact. Refusing to refine there is the
conservative, sound choice.

**The lattice must stay small.** With this catalogue every reference is in one of three
states — unrefined, non-null, exactly-null — so the refinement lattice has height 2 and the
fixpoint of §7 terminates trivially. Any future extension must justify its effect on
termination, not just its expressiveness.

## 4. Frame translation

A guard is written in the *guarding* task's frame; it has to be read in the *target's*.
A guard term is usable only if its subject survives that translation.

| Guard subject | Translates to | Why |
|---|---|---|
| `self.output.v` | `outputs.<guarding task>.v` | only when the task exports an `output` |
| `outputs.b.y` | unchanged | already frame-invariant |
| `input.x` | unchanged | set at instance start, never rewritten |
| `self.result.…` | **dropped** | the raw action result is not exported; the target cannot name it |
| `self.previous.…` | **dropped** | not exported |
| `error.…` | **dropped** | `$error` is rebuilt per error edge, not carried |
| `config.…` | **dropped — see below** | |

**`config` is frame-invariant in name but not in value.** Config variables are resolved from
the environment on *every tick* and never persisted —
[internal/model/config.go:16](../internal/model/config.go#L16), "Never persisted; runs at
start and every tick". A guard proving `config.mode != null` in one task says nothing about
the value the next task will read. Narrowing on `config` would be unsound, and this is the
non-obvious trap in the whole design.

## 5. Refinements live on edges, not tasks

`predEdge` ([internal/validation/context.go](../internal/validation/context.go)) is today:

```go
type predEdge struct {
    idx   int  // predecessor task index; -1 = process start
    isErr bool // true = on_error route
}
```

It records which task an edge came from, not which **case**. Two cases in one task routing
to the same target are currently indistinguishable, and they carry different refinements, so
`buildPreds` must emit **one edge per case** and stop deduplicating.

That cardinality change is safe for the existing analysis: `computeContextSets` intersects
over predecessors for `must` and unions for `may`, and both are idempotent, so extra edges
from the same predecessor do not perturb either result.

## 6. Merging at a task

A task with several incoming edges takes, for each reference, the **union of its refined
types across every edge** — the ordinary control-flow merge, and the same rule the `must`
set already follows.

The practical consequence: **a refinement survives only if every incoming edge establishes
it.** An edge that says nothing about `X` contributes `X`'s declared type, and the union is
the declared type. This is what keeps the analysis honest without any special-casing — a
task reachable both from a null-check and from somewhere else gets no refinement, which is
correct.

## 7. Ordered cases, and the negation of earlier ones

`switch` is first-match-wins, so reaching case *k* means every earlier case was false. The
edge for case *k* carries:

```
refine(case_k)  ∧  ¬refine(case_1)  ∧  …  ∧  ¬refine(case_{k-1})
```

Negations follow the table in §3 and are simply dropped where a guard has no useful
negation. Where two terms refine the *same* reference they are intersected (a meet); where
they refine different references they coexist independently. Neither operation relates two
references, so neither escapes the per-reference domain.

**This is not an optional refinement of the feature — it is most of the feature.** Compare
the two ways an author writes the same logic:

```yaml
# positive form — the refinement comes from the case itself
switch:
  - case: "self.output.v != null"
    goto: $use              # narrowed with or without negation
  - goto: end

# guard-clause form — handle the bad case, fall through to the good one
switch:
  - case: "self.output.v == null"
    goto: $missing
  - goto: $use              # NO case expression of its own
```

In the second, the arm carrying the main work has no `case:` at all: every refinement it
could have comes from negating case 1. That is the early-return idiom — deal with the
exception, continue with the good path — and it is at least as common as the positive form.
A version that narrows only on the matched case would narrow nothing on precisely the arm
that matters most.

It is also the part most likely to harbour bugs (§11 explains why that is expensive here),
and the three ways to get it wrong are all silent:

- **Off-by-one** — negating case *k* itself, or a case *after* it. Both invent proofs the
  definition never made.
- **Distributing the negation of a conjunction.** `¬(A ∧ B)` is a disjunction — "at least one
  of these is null" — which is not a per-reference fact. §3 says drop it; an implementation
  that helpfully splits it into two refinements is unsound.
- **Leaking a negation across edges.** A task reachable from case 2 of task A *and* the
  catch-all of task B has different negation sets per edge; they must merge per §6, not
  accumulate.

## 8. Loops invalidate refinements

**Task outputs are not SSA.** A re-entered task overwrites its own `outputs.<id>` slot —
that is exactly what `self.previous` reads
([internal/engine/advance.go:232](../internal/engine/advance.go#L232), "the value from the
last loop iteration"), and the polling example's `backoff` task counts its polls that way.
So a refinement about `outputs.T.v` is invalidated when `T` runs again.

The rule is a standard dataflow kill:

> When computing task *i*'s in-refinements, **kill every refinement whose subject is
> `outputs.i.…`** before task *i*'s own contribution.

Going around a loop, each task in the loop kills its own refinement, so a refinement about a
loop-internal output dies after one trip and the fixpoint converges. Refinements about
`input.…` and about outputs of tasks outside the loop survive, which is both sound and the
useful case.

## 9. Why this is tractable, when §5 of the other doc is not

Both look like "be smarter about branches", and the difference decides feasibility.

Guard narrowing refines **one reference**: at a merge, each reference's type is computed
independently, the lattice is 3 states tall, and cost is linear in
`references × edges × iterations`.

The deferred mid-process case relates **two references** ("a is set on this path, b on that
one"), which requires tracking sets of alternative states — a disjunctive/relational domain
that is exponential in the worst case and needs widening to terminate.

TypeScript draws the identical line: it narrows references through control flow, and it does
not correlate distinct references (`if (c) { a = 1 } else { b = 2 }` leaves `a ?? b` possibly
undefined). Landing on the same side of the same line is reassuring.

## 10. Non-goals

- **No correlation between distinct references.** See above.
- **No narrowing from `on_error` codes.** `$error` *presence* is already computed by the
  `mustErr`/`mayErr` dataflow; refining a code to a literal is a separate feature.
- **No narrowing of `self.result`, `self.previous`, `error`, or `config`.** §4.
- **No general predicate reasoning.** The catalogue is the contract.
- **Nothing at runtime changes.** This is purely a registration-time analysis; the evaluator
  already behaves the way the refinements describe.

## 11. Soundness, and why the bar is higher than it looks

The cost of being wrong is **asymmetric**, and that is what sets the bar.

A *missing* refinement costs an author a `?? 0` — annoying, harmless, visible. A *wrong*
refinement makes the checker accept `outputs.a.v * 2` on a path where `v` really is null.
That fails at runtime as `engine.expression`
([internal/errcode/errcode.go:117](../internal/errcode/errcode.go#L117)), and internal
`engine.*` failures are **terminal and are not routed through `on_error`** (see the `code`
description on `ErrorCase` in [internal/model/wire.go](../internal/model/wire.go)). So an
unsound narrowing does not merely fail to help: it converts a registration-time type error
into an uncatchable instance failure. The type system would have actively made things worse
than having no narrowing at all.

That asymmetry is the argument for the small catalogue (§3), for dropping every
untranslatable subject rather than guessing (§4), and for the test weighting in §13.

The positive argument is correspondingly short — which is itself the point of keeping the
catalogue small enough to state in a table.

A `case` expression is evaluated at runtime, by the same evaluator, over the same context,
immediately before the edge is taken. A refinement derived from case *k* is therefore backed
by a real test that the engine actually performed. The only ways it can go wrong are:

1. **The value changes between the test and the use** — handled by excluding `config` (§4)
   and by the loop kill (§8). These are the two ways a value can change in genroc, and both
   are closed explicitly.
2. **The refinement does not follow from the guard** — bounded by the catalogue (§3) and the
   ordered-case negation (§7), both of which are small enough to test exhaustively.
3. **The edge is taken for another reason** — impossible: an edge belongs to exactly one case
   once `buildPreds` stops deduplicating (§5).

Note that `MemberNode` already documents optional-chaining semantics ("property access on a
null base yields null"), so a *missing* intermediate never throws at runtime; narrowing only
ever removes a null that the guard proved absent.

## 12. Implementation sketch

1. **`predEdge` gains the guard.** One edge per switch case; carry the case index and the
   task index. Stop deduplicating `next` edges.
2. **A guard extractor** — `syntax.Node` → `[]refinement{path, state}` — recognising exactly
   §3, plus its negation form. Pure, table-testable, no schema or DB dependency (the
   `internal/delayspec` precedent).
3. **Frame translation** (§4), rejecting untranslatable subjects rather than guessing.
4. **A refinement fixpoint** beside `computeContextSets`, over the same `preds` graph: merge
   by union across edges, meet within an edge, kill `outputs.i` at task *i*.
5. **Apply in `contextSchema`** — the refined type replaces the declared one for that
   reference. The existing `absent` category from path-sensitive output is the precedent for
   a third per-reference state.

Steps 1–3 are independently testable before any of this affects a definition, which is how
I would sequence it.

## 13. Test plan

Weighted by §11: a wrong refinement is far more expensive than a missing one, so the
negation logic — the likeliest source of a wrong one — is tested first and hardest.

- **Ordered cases (heaviest)**: the guard-clause shape from §7 narrows the catch-all arm;
  case 3 sees the negation of cases 1 and 2 and *not* of itself; a *later* case never
  affects an earlier edge; an earlier case about an unrelated reference does not leak;
  `¬(A ∧ B)` narrows nothing; two edges into one task do not accumulate each other's
  negations.
- **Catalogue**: each guard shape narrows on the taken edge and on the fall-through; every
  unrecognised shape narrows nothing.
- **Merge**: two edges both proving non-null → narrowed; one proving, one not → not
  narrowed; error edge into the same task → not narrowed.
- **Loops**: a refinement about a loop-internal output dies on re-entry; one about `input`
  survives; the polling example still type-checks.
- **Frame translation**: `self.output.v` translates; `self.result.v`, `error.code`,
  `self.previous.x` and `config.x` do not — `config` with a test naming the every-tick
  resolution as the reason.
- **Soundness pairing**: for each narrowing case, a runtime test that the routed branch
  really does receive a non-null value.
- **No regression**: every existing definition and example still registers, and the
  batch-invoices / order-fulfilment / polling examples keep their current inferred types.

## 14. Rejected alternatives

**Narrow inside the expression, `if`-style.** Introduce a conditional expression that
narrows its own branches (`x != null ? x * 2 : 0`). This is real TypeScript-style narrowing
but solves a different problem — it works *within* one expression, while the pain here is
*across* tasks, which is where genroc's control flow actually lives. Worth having
eventually; not a substitute.

**Let the author assert it.** A `non_null:` annotation on a task input. Cheap, but it is a
claim rather than a proof — precisely the thing `not_reached: true` had to be restricted for
in `only_once` validation. A checker that accepts assertions it cannot verify teaches authors
to write them reflexively.

**Infer guards from `on_error` structure instead.** Narrower and less useful: error routing
already carries its facts through `$error`, and the null-check case — the one that hurts —
is a `switch`, not an `on_error`.

## 15. Decided, and open

**Decided: build the negation of earlier cases (§7), in v1.** It was briefly an open
question here — a version narrowing only on the matched case is simpler and still handles §1's
example. That reasoning was wrong, or at least misleading: §1's example is the *positive*
form, and the guard-clause form, where the working arm has no `case:` of its own, is at least
as common and gets nothing without negation. Deferring it would ship the feature without the
half that carries most of the value.

The risk it carries is real but belongs in the tests, not in the schedule — §13 is weighted
accordingly, and §11 says why the bar is where it is.

Still open:

- **Should a refinement be visible in the published schema?** Task input schemas are
  generated and published; a refined type is arguably more accurate, but it makes the same
  task's context schema differ by incoming edge, which the current SchemaFile shape cannot
  express.
- **How is a refined type reported in errors?** "`outputs.a.v` is integer here (narrowed by
  the case on task `a`)" is far more useful than "integer", and the provenance has to be
  carried to say it.

## 16. Prior art

TypeScript's control-flow analysis is the reference implementation, and the comparison cuts
both ways.

TypeScript types `a ?? b` as `NonNullable<A> | B` — the same rule genroc now implements — and
narrows references through a CFG, merging at join points by unioning the types on incoming
edges, which is the same merge as §6.

Where genroc has it *easier*: TypeScript spends much of its complexity on invalidation,
because assignments, function calls and closure captures can all clobber a narrowing (the
familiar `if (o.a) { cb(() => o.a.b) }` failure). genroc has exactly two invalidation
sources, both enumerated in §11, and both closable by construction.

Where genroc has it *harder*: TypeScript's guards and their uses sit in one lexical scope,
so no frame translation is needed. genroc's guard and its consumer are in different tasks
with different context shapes, which is the work of §4 — and is why `config`, harmless in a
TypeScript-shaped world, is a soundness hazard here.

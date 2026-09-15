# Guard narrowing

**Status: implemented 2026-09-15.** `computeRefinements` (`internal/validation/guards.go`)
carries a `switch` case's proof along the edge it selects; `guardFacts`
(`internal/schema/infer.go`) is the catalogue, shared with the expression-level narrowing that
shipped first. `||` still narrows nothing across an edge; the discriminant guard still waits on
literal types. Companion to [path-sensitive-output.md](path-sensitive-output.md).

Three things are load-bearing and none of them are where the sketch below expected:

1. **A fact carries no schema**, only the comparison it came from. A refinement about
   `self.output.v` that carried a type would need that task's output inference, which depends
   on the context the fixpoint is computing.
2. **Refinements are applied as guards, not by rewriting the context** (`Schema.WithGuards`,
   consulted by `Infer`). They ride on the context VALUE, so every caller that already threads
   a context inherits them — and because a guard is keyed by the path a read uses, an element
   path narrows that element rather than `items`, which every element shares.
3. **`WithProperty` and the `WithDefs` family therefore carry guards; navigation must not.**
   The first two return the same context with more on it (`self` is added between the base
   scope and the slot); navigation returns a different value, and guards are keyed from the
   root. Getting this wrong is silent — the refinement simply stops applying.

## The problem

A definition can prove `self.output.v != null` in a `switch` case and route on the
proof, yet the routed task still cannot use `outputs.a.v` — the case expression is
discarded once it selects an edge, and the author writes `?? 0` fallbacks that provably
never evaluate. The one expression whose meaning the type system ignores is the one that
decided control flow.

## Proposal

Carry a **refinement** — a narrowed type for one reference — along the edge a case
selects, and apply it in the target's context. Scope discipline is the whole design:
one reference at a time; correlating two references is the exponential problem
path-sensitive-output §5 deliberately avoids, and TypeScript draws the identical line.

**Guard catalogue (closed):** `X != null` / `X == null` (each exact on both edges),
`!G`, `G1 && G2` (both refinements on the taken edge, **nothing** on fall-through — the
negation of a conjunction is not a per-reference fact). `||` narrows nothing across an EDGE
in v1 — inside an expression it is exact, its right operand running only where the left
failed, which is why the shipped half carries it and this half does not. The
discriminant guard (`X.d == lit`) lives in
[discriminated-unions.md](discriminated-unions.md), deferred on literal types; nothing
here waits on it. The lattice is three states per reference (unrefined / non-null /
exactly-null), so the fixpoint terminates trivially — any extension must justify its
effect on termination, not just expressiveness.

**Frame translation.** A guard is written in the guarding task's frame and read in the
target's: `self.output.v` → `outputs.<task>.v` (only if exported); `outputs.*`/`input.*`
unchanged; `self.result`, `self.previous`, `last_error` **dropped** (not carried across).
**`config` is dropped, and this is soundness trap #1:** it is frame-invariant in name
but re-resolved from the environment every tick and never persisted, so a guard on it
proves nothing about the value the next task reads.

**The editor must see the same narrowing.** `SlotContexts` carries one context per switch
CASE wherever an earlier case narrows something — the same reason `on_error` is addressed per
rule — and only where it differs, so the `genctl schema context` listing is not padded with
rows repeating the switch context. Without it a hover reads `boolean|null` on an expression
registration just accepted, which is the editor contradicting the checker.

**Cases narrow each other, not only edges.** `evalSwitch` returns the first match, so case k
runs only when 1..k-1 were false — the same negation an outgoing edge carries, read in the
task's OWN frame (nothing translated, nothing dropped: every name a case can write is still in
scope for a later one, `config` included, since one pass reads one resolved value). Refusing
this is what sends an author to a `?? default` in the case right below their own null check.
NOT applicable to `on_error`: a rule there is skipped when its CODE does not match, before its
`case` is evaluated, so reaching rule k proves nothing about rule j's case.

**A guard landing on a `$ref` must MATERIALIZE before stripping null**
(`Schema.StripNullMaterialized`). A ref rides through `StripNull` untouched — deliberately,
since leaving refs symbolic is what keeps recursive types finite — so a null declared inside
the target survives, `HasNull` reports it, and `StripNull` is a no-op. Every guard on a whole
task output hits this, an output being carried as a ref by construction. It is not specific to
edges: the expression-level narrowing had the same hole. Making `StripNull` itself resolve was
tried and REVERTED — it inlines recursive definitions and the output solver stops converging;
resolving at the one call that narrows is the same trade `inferNullCoalesce` already makes.

**Edges, not tasks.** `predEdge` must become one edge per switch case (stop
deduplicating) — two cases routing to one target carry different refinements. Safe for
the existing analysis: must/may are idempotent under duplicate edges.

**Merge:** per reference, union of refined types across incoming edges — a refinement
survives only if every edge establishes it; an edge silent about `X` contributes the
declared type. Same rule the must-set already follows.

**Ordered-case negation is most of the feature, not a refinement of it.** Reaching case
k means cases 1..k-1 were false, so the edge carries `refine(k) ∧ ¬refine(1..k-1)`. The
guard-clause idiom — handle the bad case, fall through with **no** `case:` — gets all
its narrowing from negation; a matched-case-only version narrows nothing on exactly the
arm that matters. Three silent ways to get it wrong: off-by-one (negating k itself or a
later case); distributing `¬(A ∧ B)` into two refinements (unsound — drop it); leaking
negations across edges instead of merging per the rule above.

**Loops kill refinements — soundness trap #2.** Task outputs are not SSA: a re-entered
task overwrites `outputs.<id>` (that is what `self.previous` reads). Standard dataflow
kill: computing task i's in-refinements, kill every refinement about `outputs.i.*`
first. A loop-internal refinement dies after one trip; `input.*` and outside-the-loop
outputs survive.

## Soundness bar

The cost is asymmetric: a missing refinement costs an author a visible `?? 0`; a wrong
one converts a registration-time type error into an uncatchable runtime
`engine.expression` failure — worse than no feature. Hence the small catalogue, dropping
untranslatable subjects rather than guessing, and negation-heavy test weighting. The
positive argument is short by design: a case is evaluated by the same evaluator over the
same context immediately before the edge is taken, so a refinement is backed by a test
the engine actually performed; the only failure modes are value-changed-since (closed:
config exclusion + loop kill), refinement-doesn't-follow (closed: catalogue + negation
rules, both exhaustively testable), and edge-taken-for-another-reason (impossible once
edges are per-case).

## Implementation sketch

(1) `predEdge` gains case index, stop deduplicating; **(2) DONE** — `guardFacts`
(`internal/schema/infer.go`) is the catalogue as a pure walk, returning what a condition
proves on each branch and leaving what it MEANS to the caller, which is what lets one walk
serve both features; **(3) DONE** — `translateGuard` (`internal/validation/guards.go`);
(4) a refinement fixpoint beside `computeContextSets` (union across edges, meet within, kill
`outputs.i` at i); **(5) DONE, and not where this said** — `Schema.InferWithGuards` seeds the
refinements into the guard map the expression narrowing already consults, rather than
rewriting `contextSchema`'s output. Two things fall out: the narrowing semantics are the ones
already tested, and because a guard is keyed by the rendered path a read uses, an element
path narrows THAT element — schema surgery would have had to narrow `items`, claiming a proof
about one element for all of them.

A fact carries NO schema, only the comparison it came from. That is what keeps (4)
non-circular: a refinement about `self.output.v` would otherwise need that task's output
type, which is inferred from the context the fixpoint is computing. Types are touched only
at (5), where `TaskSchemas` is already filled in.

Tests, weighted by the asymmetry — all present, and each verified by breaking the rule it
covers and watching it fail (`validationtest/guard_narrowing_test.go` unless noted):

| rule | broken by | caught by |
|---|---|---|
| the feature at all | never applying refinements | 12 cases |
| a refinement needs EVERY edge | meet → union | merge, error-edge, cross-edge |
| ordered-case negation | dropping it | the guard-clause row |
| k must not negate ITSELF | `j <= k` | 10 cases |
| `config` never travels | allowing it through | ConfigNeverTravels |
| an error edge carries no case | `edgeRefs` ignoring `sw == -1` | ErrorEdgeCarriesNothing |
| `¬(A∧B)` proves nothing | `&&` claiming its facts on false | NegatedConjunction |
| the catalogue itself | see `schema/guardfacts_test.go` | 4 rows |
| frame translation | see `validation/guards_test.go` | 15 rows |

Two things are NOT pinned, deliberately. The `outputs.<self>` kill is unobservable today —
`outputs.<own id>` is shadowed to `self.previous` in a task's own slots, so the stale
refinement it guards against has no way to be read; it stays as cheap insurance if the merge
rule ever loosens. And the process output is built from terminals rather than a task's entry
context, so refinements do not reach it — `ProcessOutputIsNotNarrowed` pins that as a LIMIT,
so lifting it is a deliberate flip.

End-to-end, over HTTP against a running engine: `tests/integration/guard_narrowing_test.ts`
registers, starts and completes the guard-clause shape, checks the value the proof made
legal, and takes the null edge at runtime.

## Rejected alternatives

- **Expression-level narrowing** (`x != null ? x*2 : 0`): a different problem — genroc's
  pain is across tasks — and no longer an alternative to anything: it SHIPPED, on the
  reasoning above. Kept here because the two are still easy to conflate.
- **Author assertion** (`non_null:`): a claim, not a proof — the same reason
  `not_reached` had to be restricted; teaches reflexive assertion.
- **Infer from `on_error` structure**: error routing already carries `last_error`; the
  case that hurts is a `switch`.

## Decided, and open

**Decided: negation ships in v1** — the simpler matched-case-only version handles the
positive form but not the guard-clause form, which is at least as common; the risk goes
into the tests, not the schedule. Open: should refined types appear in the published
schema (they differ per incoming edge, which SchemaFile cannot express)? How is
provenance reported in errors ("narrowed by the case on task a")?

## Prior art

TypeScript's CFG narrowing is the model (same `??` rule, same union-at-join merge).
genroc has it easier on invalidation (two sources, both closable, vs.
assignments/closures) and harder on scope: guard and consumer live in different frames,
which is why translation exists and why `config` — harmless in a lexically-scoped
world — is a hazard here.

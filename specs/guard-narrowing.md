# Guard narrowing

**Status: proposed, not implemented.** Companion to
[path-sensitive-output.md](path-sensitive-output.md) (implemented).

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
negation of a conjunction is not a per-reference fact). `||` narrows nothing in v1. The
discriminant guard (`X.d == lit`) lives in
[discriminated-unions.md](discriminated-unions.md), deferred on literal types; nothing
here waits on it. The lattice is three states per reference (unrefined / non-null /
exactly-null), so the fixpoint terminates trivially — any extension must justify its
effect on termination, not just expressiveness.

**Frame translation.** A guard is written in the guarding task's frame and read in the
target's: `self.output.v` → `outputs.<task>.v` (only if exported); `outputs.*`/`input.*`
unchanged; `self.result`, `self.previous`, `error` **dropped** (not carried across).
**`config` is dropped, and this is soundness trap #1:** it is frame-invariant in name
but re-resolved from the environment every tick and never persisted, so a guard on it
proves nothing about the value the next task reads.

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

(1) `predEdge` gains case index, stop deduplicating; (2) a pure guard extractor
`syntax.Node → []refinement` for exactly the catalogue + negations; (3) frame
translation, rejecting rather than guessing; (4) a refinement fixpoint beside
`computeContextSets` (union across edges, meet within, kill `outputs.i` at i); (5) apply
in `contextSchema` (the path-sensitive `absent` category is the precedent for a third
per-reference state). 1–3 are independently testable first.

Tests, weighted by the asymmetry: ordered-case negation heaviest (guard-clause shape;
k sees ¬1..k-1 and not ¬k; no cross-edge accumulation; `¬(A∧B)` narrows nothing), then
catalogue/merge/loop-kill/frame tables, a runtime pairing per narrowing case, and
no-regression over the examples.

## Rejected alternatives

- **Expression-level narrowing** (`x != null ? x*2 : 0`): real, but solves a
  different problem — genroc's pain is across tasks. Eventually worth having.
- **Author assertion** (`non_null:`): a claim, not a proof — the same reason
  `not_reached` had to be restricted; teaches reflexive assertion.
- **Infer from `on_error` structure**: error routing already carries `error`; the
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

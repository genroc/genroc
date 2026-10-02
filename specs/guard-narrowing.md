# Guard narrowing

Status: **Built, except the two catalogue items in Open** (`||` on a taken edge, the
discriminant guard). `computeRefinements` (`internal/validation/guards.go`) carries a `switch`
case's proof along the edge it selects; `guardFacts` (`internal/schema/infer.go`) is the
catalogue, shared with expression-level narrowing — `?:` narrows its branches and the left of
`&&`/`||` its right operand (the evaluator short-circuits), and there `X == lit` narrows to the
literal's type. Companion to [path-sensitive-output.md](path-sensitive-output.md).

A definition that proves `self.output.v != null` in a `switch` case and routes on it can then use
`outputs.a.v` in the routed task without a `?? 0` that provably never evaluates.

## Design

1. **A fact carries no schema**, only the comparison it came from. A refinement about
   `self.output.v` carrying a type would need that task's output inference, which depends on the
   context the fixpoint is computing. Types are touched only when a refinement is applied.
2. **Refinements are applied as guards, not by rewriting the context** (`Schema.WithGuards`,
   `InferWithGuards`, consulted by `Infer`). They ride on the context VALUE, so every caller that
   threads a context inherits them; and because a guard is keyed by the path a read uses, an
   element path narrows that element rather than `items`, which every element shares.
3. **`WithProperty` and the `WithDefs` family carry guards; navigation must not.** The first two
   return the same context with more on it (`self` is added between the base scope and the slot);
   navigation returns a different value, and guards are keyed from the root. Getting this wrong
   is silent — the refinement simply stops applying.

Scope discipline: one reference at a time. Correlating two references is the exponential problem
path-sensitive-output §5 avoids.

## Guard catalogue

Closed: `X != null` / `X == null` (each exact on both branches), `X == lit` (non-null on the equal
branch; `X != lit` proves nothing — not being one non-null literal says nothing about type),
`!G`, and `G1 && G2` (both refinements on the taken edge, **nothing** on fall-through — the
negation of a conjunction is not a per-reference fact). `G1 || G2` narrows nothing on its taken
edge (Open) — inside an expression it is exact, its right operand running only where the left
failed; falling past a `||` case does carry `¬A ∧ ¬B`, which is per-reference.

The lattice is three states per reference (unrefined / non-null / exactly-null), so the fixpoint
terminates trivially — any extension must justify its effect on termination, not just
expressiveness. A reference one predicate proves both null and non-null is dropped.

## Frame translation

A guard is written in the guarding task's frame and read in the target's (`translateGuard`):
`self.output.v` → `outputs.<task>.v` (only if the task exports an output); `outputs.*`/`input.*`
unchanged; `self.result`, `self.previous`, `last_error` and computed keys dropped. **`config` is
dropped — soundness trap #1:** it is frame-invariant in name but re-resolved from the environment
every tick and never persisted, so a guard on it proves nothing about the value the next task
reads.

## Cases and clauses

**Cases narrow each other, not only edges.** `evalSwitch` returns the first match, so case k runs
only when 1..k-1 were false — read in the task's OWN frame, nothing dropped (`config` included,
since one pass reads one resolved value).

**A switch case and an `on_error` rule are one thing: a guard, plus whether its falsity may be
read** (`clause.negates`). A rule's predicate is `(code == a || code == b) && case`, so falling
past one that names a code proves the negation of a conjunction — nothing: it may have been
skipped on the code before its `case` ran (`matchOnErrorWith`). So a coded rule has
`negates: false`; every switch case and every code-less rule negates.

Everything reads the one `clauseFacts` walk: the case expression's scope (priors only), its
clauses' (priors plus its own), and the edge it takes (the same, in the target's frame). Separate
walks would let one of them silently miss a negation the others have.

**A clause beside a case may assume it; the case may not.** A `panic`, `raise` or `retry` runs
only because its case matched, so it reads the case's `whenTrue` facts. The case expression must
never be given them — it is what establishes them, and handing them back is circular.

**An `on_error` rule's `goto` is an edge like a case's.** Its case travels, under a translation
that drops everything of the task that FAILED: it produced no output, so `self.*` has no
downstream name, and the `error` it caught is the target's `last_error`, a different value under a
similar path. What the failing task's entry context proved still travels.

## Edges, merge, loops

- **Edges, not tasks.** `predEdge` is one edge per switch case or rule — two cases routing to one
  target carry different refinements. Must/may analysis is idempotent under duplicate edges.
- **Merge** is a meet per reference: a refinement survives only if every incoming edge
  establishes it; an edge silent about `X` contributes the declared type.
- **Ordered-case negation is most of the feature.** The guard-clause idiom — handle the bad case,
  fall through with no `case:` — gets all its narrowing from negation. Three silent ways to break
  it: negating case k itself or a later case; distributing `¬(A ∧ B)` into two refinements;
  leaking negations across edges instead of merging.
- **Loops kill refinements — soundness trap #2.** A re-entered task overwrites `outputs.<id>`
  (which is what `self.previous` reads), so computing task i's in-refinements first kills every
  refinement about `outputs.i.*` (`killOutput`). A loop-internal refinement dies after one trip;
  `input.*` and outside-the-loop outputs survive.

## References and `StripNull`

**A `$ref` may not change what a type answers.** An output is carried as a ref by construction, so
`StripNull` follows references, as `HasNull` does — resolving only where the null actually is, so
a recursive object's nullable link resolves once and its `next` stays symbolic. Do not make it
resolve eagerly: inlining recursive definitions stops the output solver converging.

**The bound is the cycle, not a hop count**: `stripNullIn` follows a ref chain, a nullable inside
a union arm, and both composed; a hop count refuses a legal type for a reason the author cannot
see. Termination comes from the path set; the shape that needs it (`A = B|integer`,
`B = A|null`) is refused by `CheckDoc`, but dropping the guard costs a hung language server
(`TestStripNullThroughAReferenceCycle`).

**A running estimate is left alone** (`servesEstimate`): a ref onto a definition the solver is
still computing is served its estimate, nullable on purpose so `x ?? 0` takes its default on the
first pass. Stripping it stops the fixpoint converging.

## The editor

`SlotContexts` carries one context per switch case (`tasks.a.switch.1`), the same reason
`on_error` is per rule; without it a hover contradicts what registration accepted. A clause is
addressed one level below its case (`tasks.a.switch.1.panic`, `…on_error.0.retry`) because
`enclosingSlot` stops at the first slot walking up from the cursor: the `case` reads the scope
that proves the guard, the clause beside it the one that assumes it.

## Soundness bar

A missing refinement costs an author a visible `?? 0`; a wrong one turns a registration-time type
error into an uncatchable runtime `engine.expression`. Hence the small catalogue, dropping
untranslatable subjects rather than guessing, and negation-heavy tests. A refinement is backed by
a test the engine performed over the same context just before taking the edge; the failure modes
are value-changed-since (closed by the `config` drop and the loop kill), refinement-doesn't-follow
(catalogue and negation rules) and edge-taken-for-another-reason (impossible with per-case edges).

Two things are NOT pinned, deliberately. The `outputs.<self>` kill is unobservable today —
`outputs.<own id>` is shadowed to `self.previous` in a task's own slots, so the stale
refinement it guards against has no way to be read; it stays as cheap insurance if the merge
rule ever loosens. And the process output is built from terminals rather than a task's entry
context, so refinements do not reach it — `ProcessOutputIsNotNarrowed` pins that as a LIMIT,
so lifting it is a deliberate flip.

Tests: `validationtest/guard_narrowing_test.go`, `schema/guardfacts_test.go`,
`validation/guards_test.go`, `schematest/ref_nullable_test.go`, `schematest/seeded_guards_test.go`;
over HTTP `tests/integration/guard_narrowing_test.ts`, and the editor half
`tests/lsp/hover_guard_test.ts`.

Rejected: an author assertion (`non_null:`) — a claim, not a proof, and it teaches reflexive
assertion.

## Open

- `||` on a taken edge: each disjunct proves only "one of these", not a per-reference fact.
  Trigger: an author forced into `?? 0` after an `a != null || b != null` case.
- The discriminant guard (`X.d == lit` selecting a union arm) — needs
  [literal-types.md](literal-types.md). Trigger: literal types landing.
- Refined types in the published schema — they differ per incoming edge, which SchemaFile cannot
  express. Trigger: a consumer of the schema needing the narrowed type.
- Reporting which case narrowed in errors ("narrowed by the case on task a"). Trigger: an author
  confused by a type that differs from the declared one.

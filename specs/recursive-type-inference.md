# Recursive type inference: design

Status: agreed and implemented 2026-07-02. The solver lives in
`internal/schema/solver.go`; the symbolic typing rules in `infer.go`/
`inferops.go`/`navigate.go`; productivity in `checkdoc.go`. Tests:
`schematest/solver_test.go`, `schematest/refcycle_test.go`,
`validationtest/recursive_structural_test.go`.

> **Syntax note.** Examples below predate the `{{ }}` → `${ }` retarget ([typed-values.md](typed-values.md)): read `{{ x }}` as `${ x }` inside a string, or `"$: x"` where the whole leaf is one expression.

> Historical note: the Motivation below names the pre-redesign mechanisms
> (`inferOutputFixpoint` and `outputorder.go`'s hand-maintained syntactic
> graph), which no longer exist. The current joint fixpoint is `solveCluster`
> in `solver.go`, bounded by `maxSolvePasses` (pass cap) and `maxSolvedTypeBytes`
> (the 64KB widening cap).

## Motivation

Output-map type inference has two mechanisms that this design unifies and
generalizes:

1. **Ordering** — `internal/validation/outputorder.go` builds a *syntactic*
   dependency graph over output-map tasks (`template.OutputRefs` string
   analysis), runs Tarjan SCC on it, and infers SCCs in dependency-first order.
   The graph is a parallel re-implementation of "what does inference read" and
   must be kept in sync with the real inference by hand.
2. **Recursion** — mutually/self-recursive output maps are resolved by a joint
   fixpoint (`inferOutputFixpoint`): defs seeded `null`, re-inferred, joined and
   canonicalized until stable, bounded by `maxRecursivePasses` and a 64KB
   widening cap. The system can therefore only express recursion that
   *converges to a finite concrete type*; structural recursion (list/tree
   accumulators like `child: "{{self.previous}}"`) grows without bound and dies
   at the widening cap.

The redesign moves inference into the `schema` package, makes ref resolution
demand-driven (the dependency graph becomes exact by construction), and lets
inferred types *honor `$ref`s*, so recursion surfaces as circular definitions —
kept as genuine recursive types when legitimate, collapsed when they have a
finite form, and rejected precisely when degenerate.

## The five decisions

### 1. One solver, structured as Tarjan itself

No separate discover/SCC/infer phases — read-sets grow as estimates grow, so an
upfront graph is stale by construction. Instead `solve(def)` *is* the DFS:

- Inference runs demand-driven. Looking inside `$ref Y` where Y is pending
  triggers `solve(Y)` at that moment. Every such resolution records the edge,
  so the graph is what inference actually read, not an approximation.
- Re-entering an in-progress def is the cycle signal; the stack segment from
  the re-entered def to the top collapses into a cluster (Gabow-style SCC).
- A popped singleton without self-edge is final immediately (all deps solved
  first, by construction). A cluster runs the joint fixpoint, whose external
  deps are all final by reverse-topological emission.
- If a fixpoint pass demands a new def that reaches back into the cluster, the
  cluster expands and the fixpoint restarts. Membership only grows and is
  bounded by the def count — termination is structural.

`outputorder.go`'s syntactic graph is deleted (`OutputRefs` remains only for
its runtime consumers). Validation keeps what is task-shaped: context
construction, the `loops` gate, and mapping `<id>_output` + the solver's demand
chain back to task-attributed errors.

### 2. Two typing modes with a hard line

| fragment | constructs | treatment |
|---|---|---|
| union-shaped | whole-ref access, `??`, `?:` | symbolic — `$ref`s preserved in the result; never reads estimates |
| look-inside | `.x`, `[i]`, `+ - * / %`, comparisons, `&& \|\| !`, null-narrowing | resolves the operand via the solver; inside a cluster this means the running estimate (seeded null) |

Symbolic positions never consume estimates — a ref is the answer regardless of
fixpoint state — so their contribution is pass-stable by construction and
convergence only concerns look-inside results. Look-inside constructs produce
concrete scalars/shapes, so they cannot smuggle recursion into a result: all
structural recursion flows through the symbolic mode where SCC sees it as refs.

### 3. Nullability lives at the use site

The symbolic treatment of `??` rests on `stripNull(anyOf[$ref X, null]) =
$ref X` being structural (no deref). Mid-solve estimates are therefore served
through a nullable use-site wrapper (`withNull(est)`; the null seed before the
first pass), while the finalized definition is stored as the *exact* type — a
bare nullable output (`"{{input.opt}}"`) legitimately stores a nullable def, so
this is not an assertable "defs are never null" invariant. Instead, `??`
resolves a bare `$ref` operand for analysis (`resolveTolerant`), so nullability
declared inside a referenced definition is still seen; `Schema.HasNull` and
`Schema.IsType` are resolve-aware for the same reason.

### 4. Collapse-or-keep, with productivity enforced in CheckDoc

Resolution of a popped cluster, in order:

1. **Degenerate-cycle collapse.** A cycle whose every edge is a bare
   union-position ref is a coinductive tautology, not a recursive type: every
   member collapses to the union of the cycle's non-cyclic remainders
   (μX.(X ∨ I) = I; mutual cycles all collapse to the same combined remainder).
   So bare `{{self.previous ?? input}}` collapses to the input type exactly (in
   practice to `$ref input` — the input was itself passed through whole), and
   bare `{{self.previous}}` — X = X ∨ null via the no-previous wrapper —
   collapses to exactly `null`, the value it always holds at runtime.
   Implemented as `collapseDegenerateCycles`, run after each top-level solve so
   later readers see collapsed forms; mid-chain readers are protected by the
   union-walk cycle guards in `lookupProperty`/`inferIndex`.
2. **Productivity gate for kept recursion.** Refs that remain must have every
   cycle pass through `properties` or `items` (each unrolling consumes value
   depth). Productive → keep as a genuine recursive type. Example:
   `result: "{{self.previous ?? input}}"` → `X = {result: anyOf[$ref X, I]}` —
   the previously documented divergence case becomes a correct answer.
3. **A cycle with no remainder anywhere** (X defined only in terms of itself)
   errors: "recursion with no base case". Pass cap and the 64KB widening bound
   remain as backstops.

The productivity check lives in `CheckDoc` as a *document well-formedness
rule*, not only in the solver: `result_schema` is user-supplied, and a
hand-written degenerate recursive schema (`X: {oneOf: [{$ref: X}]}`) would loop
`conform` at runtime. One guard at one boundary covers solver output and user
input alike.

### 5. Algebra hardening where refs now flow

- `joinNodes` never resolves a ref: `join($ref A, $ref A) = $ref A`; otherwise
  fold into a canonicalized, deduped union. Cycle-safe because it never derefs.
- **Canonical ref form**: a value that is exactly a def is always the `$ref`,
  never an inline copy — prevents estimate flapping; `nodesEqual` stays textual
  and is correct because of this rule.
- `conform` gets a same-value-position cycle guard (defense in depth — stored
  schemas decode without CheckDoc).
- ~~`SecretAt`/`Redact`/`CollectSecrets` and `Taint($ref X)` and their visited sets~~ — the
  taint system was deleted 2026-09-04 with the redactor it fed (object-store.md §Redaction),
  so nothing here follows a ref for secrecy any more.
- `IsSubset` is already coinductive; `MergeInto`'s rename-normalized dedup
  already handles self-referencing defs. Both stay.

## Sequencing

| step | content | payoff if stopped here |
|---|---|---|
| 1 | Inference moves into `schema`: `Schema.Infer(expr)`, `Schema.ReferencesSecret(expr)`; `At(path)` keeps plain navigation; `expression` keeps `Eval` + refs analysis (op tables split; the `union_*` conformance tests guard drift) | ergonomics; prerequisite — the solver must own deref |
| 2 | Algebra hardening + productivity in `CheckDoc` + `conform` guard | fixes the latent user-schema hang |
| 3 | The Tarjan solver in `schema`; `outputorder.go`/`recursive.go` ported onto it; syntactic graph deleted | exact dependency graph, demand-chain errors, one recursion mechanism |
| 4 | Symbolic fragment + collapse-or-keep | recursive types become a feature |

## Verification plan

- The three-way conformance suite (our infer vs our eval vs real expr-lang)
  extended with ref-bearing contexts.
- Every historical divergence case as a golden test — each gets a *better*
  answer under this design (collapse, keep, or a precise error):
  - `count: "{{(self.previous.count ?? 0) + 1}}"` → `{count: integer}` (unchanged)
  - `result: "{{self.previous ?? input}}"` → recursive `X = {result: anyOf[$ref X, I]}` (was: widening error)
  - bare `"{{self.previous ?? input}}"` → collapses to the input type exactly (was: converged, but via fixpoint)
  - bare `"{{self.previous}}"` → "no base case" error (was: divergence error)
  - mixed `{count: …, child: "{{self.previous}}"}` → `{count: integer, child: anyOf[$ref X, null]}`
  - mutual structural recursion across two tasks → recursive pair of defs
- `CheckDoc` rejects degenerate user schemas; accepts productive ones.
- `conform` validates values against productive recursive schemas (gojsonschema
  as oracle — it handles recursive refs natively); errors (not hangs) on a
  degenerate schema constructed to bypass CheckDoc.
- Determinism: solve twice, byte-identical output. Demand order must not leak
  into results (declare defs in adversarial orders).
- Cluster expansion mid-fixpoint (A↔B where B also reads C and C reads A).

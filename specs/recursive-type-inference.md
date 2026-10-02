# Recursive type inference: design

Status: **Built.** The solver is `internal/schema/solver.go`; symbolic typing rules in
`infer.go`/`inferops.go`/`navigate.go`; productivity in `checkdoc.go`. Tests:
`schematest/solver_test.go`, `schematest/refcycle_test.go`,
`validationtest/recursive_structural_test.go`, `validationtest/recursive_test.go`.

Output types are inferred in the `schema` package with demand-driven `$ref` resolution, so the
dependency graph is exact by construction, and an inferred type may hold `$ref`s — recursion
surfaces as circular definitions: kept as genuine recursive types when productive, collapsed when
they have a finite form, rejected when degenerate.

## The five decisions

### 1. One solver, structured as Tarjan itself

No separate discover/SCC/infer phases — read-sets grow as estimates grow, so an upfront graph is
stale by construction. The solve *is* the DFS:

- Looking inside `$ref Y` where Y is pending solves Y at that moment, so the graph is what
  inference actually read.
- Re-entering an in-progress def is the cycle signal; the demand-stack segment from it to the top
  collapses into a cluster.
- A popped singleton without a self-edge is final immediately. A cluster runs the joint fixpoint
  (`solveCluster`, every member from the same null seed); its external deps are already final.
- A pass that demands a new def reaching back into the cluster expands it and restarts.
  Membership only grows, bounded by the def count.

Backstops: `maxSolvePasses` (16) and `maxSolvedTypeBytes` (64 KiB — divergence grows
exponentially, so the pass cap alone would build megabytes first). `internal/validation`'s
`inferOutputs` declares one member per `<id>_output` and maps the solver's errors back to tasks.

### 2. Two typing modes with a hard line

| fragment | constructs | treatment |
|---|---|---|
| union-shaped | whole-ref access, `??`, `?:` | symbolic — `$ref`s preserved in the result; never reads estimates |
| look-inside | `.x`, `[i]`, `+ - * / %`, comparisons, `&& \|\| !`, null-narrowing | resolves the operand via the solver; inside a cluster this means the running estimate (seeded null) |

Symbolic positions never consume estimates, so their contribution is pass-stable and convergence
concerns only look-inside results. Look-inside constructs produce concrete scalars/shapes, so all
structural recursion flows through the symbolic mode, where the SCC sees it as refs.

### 3. Nullability lives at the use site

The symbolic `??` rests on `stripNull(anyOf[$ref X, null]) = $ref X` being structural (no deref).
Mid-solve estimates are therefore served wrapped nullable at the use site (`estimateNode`: the
null seed before the first pass, then `withNull(est)`), while the finalized definition stores the
*exact* type. A def may legitimately be nullable (a bare `"$: input.opt"`), so `??` resolves a bare
`$ref` operand for analysis (`resolveTolerant`), and `HasNull`/`IsType` are resolve-aware.

### 4. Collapse-or-keep, with productivity enforced in CheckDoc

A solved cluster resolves, in order:

1. **Degenerate-cycle collapse** (`collapseDegenerateCycles`, after each top-level solve so later
   readers see the collapsed form). A cycle whose every edge is a bare union-position ref is a
   tautology, not a recursive type: every member collapses to the union of the cycle's non-cyclic
   remainders (μX.(X ∨ I) = I). So bare `"$: self.previous ?? input"` is the input type, and bare
   `"$: self.previous"` (X = X ∨ null) is exactly `null`, the value it always holds.
2. **Productivity for kept recursion.** Every remaining cycle must pass through `properties` or
   `items` (each unrolling consumes value depth): `result: "$: self.previous ?? input"` is
   `X = {result: anyOf[$ref X, I]}`.
3. **No remainder anywhere** (X defined only in terms of itself) is an error: "recursion with no
   base case".

Productivity is a `CheckDoc` rule, not only a solver one: `result_schema` is user-supplied, and a
hand-written `X: {oneOf: [{$ref: X}]}` would loop `conform` at runtime. One guard covers solver
output and user input alike.

### 5. Algebra where refs flow

- `joinNodes` never resolves a ref (`join($ref A, $ref A) = $ref A`; otherwise a canonical,
  deduped union), so it is cycle-safe.
- **Canonical ref form**: a value that is exactly a def is always the `$ref`, never an inline copy.
  That prevents estimate flapping, and it is why `nodesEqual` can compare canonical text.
- `conform` has its own cycle guard: stored schemas decode without `CheckDoc`.
- `lookupProperty`/`inferIndex` carry union-walk cycle guards for readers mid-chain.
- `IsSubset` is coinductive; `MergeInto`'s rename-normalized dedup handles self-referencing defs.

Recursive accumulation that grows without bound (`[outputs.c.n]`) hits the size cap with a message
naming the cause. A missing base case in a whole process (`outputs.c.n + 1`) is refused earlier, as
the nullability error an author meets (`TestRecursiveOutputWithNoBaseCaseIsRefused`).

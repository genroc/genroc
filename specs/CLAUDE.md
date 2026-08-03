# specs/

Specs and design records. **Not every doc here describes shipped behavior** — the ones
listed below are proposals. Do not cite them as current behavior.

## Design drafts (proposed, not implemented)

- [fetch-http-surface.md](fetch-http-surface.md) — two independent additions to `fetch`:
  response metadata (`self.status` / `self.headers`, which would retire the
  `http.202`-via-`on_error` trick in the polling example) and a structured `query` slot
  (string interpolation into a URL does no escaping). Each part carries its own
  compatibility argument; both are additive. Notes a blocker: hyphenated header names
  are unreadable because the parser accepts only integer literals in `[...]`.
- [typed-values.md](typed-values.md) — generalize `Shape` into a typed value authorable as
  literal YAML (expression leaves) **or** a single expression, checked against a schema via
  `inferShape`→`IsSubset`; applies to action payloads / `input`, with editor autocomplete
  from a generated JSON Schema.
- [error-extensions.md](error-extensions.md) — three considered-and-declined extensions to
  the child error model (batch-shape routing, a diagnostic payload on `raise`, opt-in
  exhaustiveness). Unlike the others these are **open questions, not intended work** — each
  records the case for and against and the signal that should reopen it.
- [custom-tasks.md](custom-tasks.md) — north-star: extend genroc **without plugins** —
  custom tasks are child processes, complex logic lives in an HTTP sidecar they call. Three
  tiers (engine / child process / sidecar), the poller & K8s-handler use cases, and the
  child-as-activity contract (idempotency, cancel, versioning).
- [lease-fencing.md](lease-fencing.md) — **partly implemented**, unlike the rest of this
  list: the stale-lease gate is live, the fence is not. Details in
  [internal/engine/CLAUDE.md](../internal/engine/CLAUDE.md).
- [literal-types.md](literal-types.md) — infer `"sent"` as `enum: [sent]` rather than
  `string`. Prerequisite for discriminated unions, and it catches provably-false comparisons.
  The feature is not the 4-line production change but the **enum-aware canonicalization** it
  forces: without it, `?? false` infers an overlapping `oneOf` that rejects `false`. Records
  the number-precision constraint (a literal must keep its exact source text) and that
  `IsSubset` needs no change.
- [discriminated-unions.md](discriminated-unions.md) — **deferred, blocked on literal
  types**, unlike the rest of this list, which is merely unscheduled. Narrowing a `oneOf` by
  a tag check would work against a hand-declared union, but inference does not produce
  literal types (`kind: sent` infers as plain `string`), so a definition cannot build a
  narrowable union and the use case that justifies the feature is unreachable. Read §0 first.
- [guard-narrowing.md](guard-narrowing.md) — refine a value's type from the `switch` case
  that routed to a task, so a definition that proves `x != null` can then use `x`. Records
  two soundness traps that are easy to miss: `config` is re-resolved every tick (so a guard
  on it proves nothing downstream), and task outputs are overwritten on loop re-entry (so
  refinements need a dataflow kill).

## Shipped behavior with a doc here

`pause-resume.md`, `only-once-interrupted.md`, `unknown-type.md`, `delay-syntax.md`,
`recursive-type-inference.md`, `resource-limits.md`, `retry-policy.md` describe code that
exists. The invariants extracted from them live in the `CLAUDE.md` of the owning package.

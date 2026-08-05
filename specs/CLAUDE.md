# specs/

Specs and design records. **Not every doc here describes shipped behavior** — the ones
listed below are proposals. Do not cite them as current behavior.

`specs/` and `docs/` are divided by what the text asserts, not by polish or audience.
**A spec records a decision** — why a design was chosen, what was rejected, what is still
unsettled — and stays internal. **A doc records behavior** that shipped, in the present
tense, for someone using the system. A spec is not a draft of a doc: nothing is promoted
between them, and when a feature lands its documentation is written against the shipped
behavior while the spec stays put, answering a different question. See
[docs-site.md](docs-site.md).

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
- [version-compatibility.md](version-compatibility.md) — decide whether an instance on one
  version could continue under another (`ctxOld(T) ⊆ ctxNew(T)`, reusing `computeContextSets`
  and `isSubset`), and move it if so. Records the result that makes it small — a task
  output's type is position-independent and the must-analysis is monotone, so checking the
  **one** context at the instance's current task is sound for the whole remaining run — and
  why there are two verdicts: the collapsed must/may sets over-approximate exactly as in
  path-sensitive-output, so the version-to-version comparison is the conservative **floor** —
  it reads two documents and never sees an instance — while the upgrade gate refines it
  monotonically (presence from the row, types from the analysis, no value ever loaded).
  Demand-pruning is filed as a gate refinement, not a comparison one: "is this output read
  from here on" is a question about a remaining run, which needs a position to mean anything.
  Upgrade writes one column, which is what buys reversibility and idempotency; the
  case that costs is a required-with-default input property. Takes a **set** of definitions,
  because one check is genuinely cross-process: whichever of a waiting parent / running child
  moves must still fit the one that did not, which needs both documents in the same frame.
  Carries a **prerequisite engine change** (§5a): drop `_spawn_result_schema`, the parent's
  `result_schema` copied onto every child row at spawn — redundant (the collector already
  holds `task`), inconsistent with the external path (which looks the schema up from the
  pinned definition instead), duplicated per child across a fan-out, and, because the conform
  normalizes, the cause of a silent strip under upgrade. Upfront that a shape check cannot
  see dollars→cents.
- [docs-site.md](docs-site.md) — the user-facing documentation site, and the only doc here
  about **tooling rather than the language**. The gap it fills is *reference*: nothing today
  says what `accepted_status` accepts or what `genctl promote` does. Draws the
  spec-vs-doc line quoted above, and follows it: `docs/` is shipped behavior only, the site
  never links into `specs/`, and the explanation a *user* needs lives in guides rather than
  being outsourced to a spec's argument. Records why plain Astro
  over Hugo (Chroma is vendored, so a genroc lexer is impossible), why no theme (its design
  system is the thing being replaced), Pagefind over any hosted search, and two deployment
  traps — `actions/deploy-pages` would delete the benchmark history on `gh-pages`, and
  GitHub Pages' one-custom-domain-per-repo rule blocks `v1.` subdomains. A playground is
  **not scoped**; it is mentioned only to record that islands are additive, so nothing in
  the architecture anticipates it — plus the one fact that makes it cheap if it ever
  happens: `internal/validation` has no `db`/`engine`/`api` dependency.

## Shipped behavior with a doc here

`pause-resume.md`, `only-once-interrupted.md`, `unknown-type.md`, `delay-syntax.md`,
`recursive-type-inference.md`, `resource-limits.md`, `retry-policy.md` describe code that
exists. The invariants extracted from them live in the `CLAUDE.md` of the owning package.

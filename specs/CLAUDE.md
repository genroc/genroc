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
- [error-extensions.md](error-extensions.md) — three considered-and-declined extensions to
  the child error model (batch-shape routing, a diagnostic payload on `raise`, opt-in
  exhaustiveness). Unlike the others these are **open questions, not intended work** — each
  records the case for and against and the signal that should reopen it.
- [custom-tasks.md](custom-tasks.md) — north-star: extend genroc **without plugins** —
  custom tasks are child processes, complex logic lives in an HTTP sidecar they call. Three
  tiers (engine / child process / sidecar), the poller & K8s-handler use cases, and the
  child-as-worker contract (idempotency, cancel, versioning).
- [script-tasks.md](script-tasks.md) — running user TypeScript needs **no new engine
  capability**: a script task is an `external` task whose input carries a code string, so
  the feature is a setup experience (`create-genroc-app` scaffolds the type generator,
  bundler, tsconfig and worker) rather than a subsystem. The sidecar tier of
  [custom-tasks.md](custom-tasks.md) made turnkey, spending none of its no-plugins
  guarantee — "plugin" here is an optional external component, never loaded code. genroc's
  one addition is a generic `genctl` **import directive** (a configured binary turns a file
  into a string, so the TS bundler is not a feature); resolution is source-level and
  client-side, and **the server having no resolver is the security answer** — no directive
  reaches the wire to be tricked into executing. Type checking is the importer's **exit
  code**, not a step: `tsc` runs before the bundle is emitted, so a type error is a failed
  import and a stored definition cannot hold code that failed to typecheck — an ordering
  property, not an enforced rule. Records what the template owns so it does not drift into
  the engine: types checked on the author's machine (schemas already validate both ends at
  runtime, so types were never the safety mechanism — which keeps definition validation
  offline), the tsconfig as sandbox, a **pinned rather than deleted**
  clock (retries re-execute; deleting `Date` leaves the generated types asserting what the
  runtime contradicts), and error codes split by honest retryability (a type error and a
  throw are permanent; folding them in with evaluator faults makes the retry budget worse
  than useless). Defers the `process_objects` work until bundles carry libraries — two
  blockers that are one change: definition-embedded values are never externalized, and
  ownership is `(instance_id, hash)` with instance-scoped GC, so code outlives its object;
  routing definition values through the store puts the object under the definition version,
  which *is* the retention rule. Constrained by migration 018's serving rule (unredacted
  context-only objects are never served).
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
- [compat-command.md](compat-command.md) — the compat **check**: two questions, not one.
  Can a running instance continue (**upgrade** — non-negotiable), and does the process still
  honour its contracts (**contract** — excusable with `--ignore contract`). Two shipped
  fixtures report the wrong thing because the two are folded into one word. Records the
  direction rule (**who submits the value**: what they submit may only widen, what we produce
  may only narrow; a verdict only where a conform stands between the parties), that a result
  schema is an *upgrade* concern wherever a task can park mid-flight (external and the child
  family — fetch is the one that cannot), and a three-level report: process, **the schema that
  was compared**, then what `isSubset` said about it. Its sharpest claim is §2e — **a schema
  denotes two different sets**, what may arrive and what is stored once the conform filled its
  defaults, so the same pair in the same direction answers differently; `Validate` already
  distinguishes the cases by mode and `IsSubset` gains the matching one, reading the sub side
  only. An implementation was written and rolled back; findings marked **[run]** came from
  running it, and three contradicted the design as written — chiefly that a slot that changed
  and a value that broke must never share a row, since that claims a cause no comparison can
  know. `config_schema` is deliberately outside the whole check: validation type-checks
  expressions against it, which catches more than compat could. **Demand pruning is required,
  not deferred** (§2f) and lands last: without it the report calls a dead output a break, and
  over-pruning it would promise an upgrade whose instance then reads a value that is not
  there — the same failure shape as a relation tolerating a gap the fill cannot close, so it
  gets the same test rigour.
- [compat-selection.md](compat-selection.md) — **deferred**, and the one piece of it that is
  built (`internal/selector`, the token lexer). The general form of `--ignore contract`: a
  token grammar where **colons scope and dots navigate**, so a build can gate on one member,
  process, task or field. Records why the member vocabulary is reserved in its position (a
  typo'd member must be a refusal, not a process scope matching nothing), why a token is
  validated for **existence and never occurrence** (an exclusion is a standing policy — silent
  while the slot is fine, failing once it is gone), and that an invalid selection degrades to
  gating *everything*. Predates compat-command's addressing and must be reconciled on landing.
- [durability-levels.md](durability-levels.md) — **one piece built** (`--sqlite-fullfsync`,
  which changes no default); the rest is proposal. Move the fsync off every persist onto a
  few boundaries, exposed as a tunable ladder (`none` → `accepted` → `only-once` →
  `terminal` → `strict`, defaulting to `only-once`). Opens with a measurement, not a
  design: macOS `fsync(2)` does not flush the drive cache, so **every benchmark number
  collected on a Mac is durability-blind** — honest `synchronous=FULL` is 183 inst/s on
  `bench-drain` against the 5,133 the same run reports today, and the 21× is the whole
  motivation. Records why boundaries suffice (prefix durability, stated backwards so
  correctness never depends on what commits *after* the one in question), why the set is
  **ingress rather than egress** (losing egress costs a replay; losing ingress is a
  permanent hang), and why `only_once` cannot be dropped from it — the durable claim is
  the evidence `interruptedOnlyOnce` reads, so without it recovery re-runs the task
  instead of reporting `interrupted`. Also records the engine asymmetry that reorders the
  work: group commit is Postgres-only and costs no durability at all, capped by
  `--pg-max-open-conns` rather than `--max-concurrent`, so the ladder is mostly a SQLite
  feature and should land *after* the two changes that spend no guarantee. Two open
  questions block it: whether the level is an operator flag or a per-definition field, and
  a `lease_epoch` reuse hazard a rewind widens. Names a live bench-harness bug it is not
  about: the failed-instance check in `tests/bench/run.ts` is unscoped, so the Postgres
  path aborts on any database that has run the test suite.
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
`recursive-type-inference.md`, `resource-limits.md`, `retry-policy.md`,
`lease-fencing.md`, `typed-values.md`, `map-expressions.md`, `number-precision.md`,
`error-handling-audit.md`, `child-error-handling.md` describe code that exists. The invariants extracted from them live in
the `CLAUDE.md` of the owning package.

`version-compatibility.md` is now **only** the upgrade half — the gate, the boundary rules,
the tree closure, the one-column write, the endpoints — and none of it is built. Everything
that defined the *check* moved to `compat-command.md`, which owns it outright; the two docs
divide at a sharp line (the check reads two documents and never an instance, the gate has the
row in hand). Its §3a prerequisite (dropping `_spawn_result_schema`) shipped, so the address
of a child's `result_schema` in `unknown-type.md` is historical — and that same change is why
a parked parent's `result_schema` is an upgrade concern, not only a contract one.

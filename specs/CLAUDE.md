# specs/

`specs/` is the internal design and its reasons; `docs/` is shipped behaviour in the present
tense, for someone using genroc. Nothing is promoted between them.

**A built spec says exactly what is built and why**: the current design, the reason for each
non-obvious choice, its invariants and traps, and a one-line rejected alternative only where
someone would plausibly re-propose it. History, build narratives and superseded designs go —
git has them. An unbuilt part stays only in an **Open** section, with the trigger that would
justify building it. Each spec's status line says what is built; never cite the rest as behaviour.

**Identifiers match the code**: a disagreement is a bug in one of them, and a rename updates every
spec that names it in the same change. A silent-failure invariant also goes in the owning
package's `CLAUDE.md`. Code comments cite `specs/<file>.md §N`, so never renumber — a removed
section keeps its heading and a one-line pointer.

## Built

- [api-auth](api-auth.md) — authorization and attribution. genroc owns which endpoints a caller
  may reach: `actionDef.Allow` (zero value admin-only), one `authorize` gate for every transport.
- [auth-two-credentials](auth-two-credentials.md) — genroc issues opaque `genroc_sk_*` tokens for
  machines and only verifies JWTs for people; it reads no identity header and mints for no proxy.
- [child-error-handling](child-error-handling.md) — raise/panic across a child. An error is a
  branch slot, not a value: a rule names only declared codes, and a defect is never catchable.
- [compat-command](compat-command.md) — `genctl compat` is two checks: upgrade (non-negotiable) and
  contract (`--ignore contract`). Direction is set by who submits the value (§2).
- [declared-slot-schemas](declared-slot-schemas.md) — a `*_schema` beside a shape is the slot's
  published type. Its conform is an assertion: a failure is a type-system defect, never data.
- [delay-syntax](delay-syntax.md) — the `delay`/`timeout` grammar (`for`/`until`/`tz`). An
  unsatisfiable date fails at parse; resolving it at registration would make validity vary by day.
- [docs-site](docs-site.md) (partly) — the Astro site in `docs/`: shipped behaviour only, never
  linking into `specs/`. Deployed by rsync, because `actions/deploy-pages` would erase `/bench/`.
- [durability-levels](durability-levels.md) (partly) — `--durability`: fsync at boundaries, and the
  default keeps ingress and `only_once`. macOS `fsync` does not flush, so Mac numbers lie.
- [error-extensions](error-extensions.md) (partly) — extensions to the child error model: X2 (a
  `raise` payload typed by the caller's `raises`) is built; X1 and X3 are declined, with triggers.
- [error-handling-audit](error-handling-audit.md) — the Go plumbing under the workflow error model.
  `errcode` namespaces are guarantees (`pre.` means the request never left), not naming.
- [external-outcome-as-signal](external-outcome-as-signal.md) — an external outcome is a buffered
  signal, never a row write. `runExternal` phase 2 reads in advance and pops in persist.
- [external-task-queue](external-task-queue.md) (partly) — `external` as a claim/lease queue. A
  claim has its own `external_*` columns; reusing the engine lease would forge `ReclaimedExpired`.
- [fetch-http-surface](fetch-http-surface.md) — `query`, status-keyed `responses`, `self.status` /
  `self.headers`. Engine and inference must resolve acceptance through one helper.
- [guard-narrowing](guard-narrowing.md) — a `switch` case's proof refines types along its edge.
  Guards ride the context: `WithProperty`/`WithDefs` carry them, navigation must not (silently).
- [id-list-commands](id-list-commands.md) — lifecycle verbs over many ids. Pause/resume are
  assertions: `already` succeeds (`Reply.Outcome`: 200/202/204); 409 only where it cannot hold.
- [language-server](language-server.md) — `genctl lsp`. `Validate` already was the analysis; what
  was missing is a location, and that is schema-command's slot address, not a second grammar.
- [lazy-context](lazy-context.md) (partly) — the object store's read side: a context loads only what
  a path needs, and `Roots.Through` leaves a merely copied reference unloaded.
- [lease-fencing](lease-fencing.md) — every lease-holding write is fenced on `lease_epoch` and
  `worker_id`. `worker_id` alone is no token: self-reclaim is the common case.
- [map-expressions](map-expressions.md) — `map`, lambdas and literals: our parser on expr-lang's
  lexer. `??`'s precedence and mixing rule are replicated because stored definitions rely on them.
- [number-precision](number-precision.md) — exact decimals, fixed at the door (`numeric.Decode` at
  every boundary). `/` is the one rounding point; a global cap would round long ids.
- [object-store](object-store.md) — content-addressed `objects`, owned through `object_refs`. Delete
  on "no live ref remains", never "my ref is gone"; the upsert must `DO UPDATE` to hold the row.
- [only-once-interrupted](only-once-interrupted.md) — a reclaimed `only_once` task raises catchable
  `only_once.interrupted`: not `pre.*` (auto-retried), not `engine.*` (never routed).
- [path-sensitive-output](path-sensitive-output.md) (partly) — the process output is typed once per
  terminal and joined. The partition is the context (`anyOf` per terminal), so readers see it too.
- [pause-resume](pause-resume.md) — `paused` is not an outcome: pause is non-destructive, so resume
  is a status flip and must never merge with `retry`. `cancelled` is settled beside `failed`.
- [recursive-type-inference](recursive-type-inference.md) — one demand-driven solver shaped as
  Tarjan. Degenerate cycles collapse, real recursion stays a `$ref`, `CheckDoc` checks productivity.
- [resource-limits](resource-limits.md) — response cap, jitter, timeouts, readiness. A worker is
  not a request handler: a fault there costs every lease it holds.
- [retry-policy](retry-policy.md) — `retry: {retries, delay, factor, max_delay}` (`retry: 3` for
  short), every slot taking `$:` — so retry reaches the delay grammar, as a hand loop could.
- [schema-command](schema-command.md) — `genctl schema context|type`. An address is a path into one
  schema (`schema.At`), with no grammar beside it; both views share one address space.
- [script-tasks](script-tasks.md) — a script task is an `external` task carrying code, pulled by
  `eval-node`. No engine capability; the server having no resolver is the security answer.
- [source-resolution](source-resolution.md) (partly) — `$<resolver>:` directives. Phases named by
  permission: `structural` runs before validation, `code` after it and must produce a string.
- [task-scopes](task-scopes.md) — what a task's slots may read. `self` members exist from different
  points; `error` is what a rule caught, `last_error` what routed control here (not durable).
- [typed-values](typed-values.md) — `"$: EXPR"` computes a typed value, `${}` always builds a
  string, `$$` escapes. Two intents, two syntaxes; expression-only slots take no marker.
- [ui-component](ui-component.md) — genroc-ui, its own image and Go module, owns the browser login.
  The server keeps no UI, OIDC flow or cookie, because it is meant to be embedded.
- [ui-issued-tokens](ui-issued-tokens.md) — genroc-ui mints an HS256 JWT carrying `perms`. HMAC,
  not RSA: a signing key regenerated on restart is the failure it avoids.
- [unknown-type](unknown-type.md) (partly) — `{}` is the top type. It enters the typed world only
  through a runtime-checked narrowing (`NarrowsTo` at collect); a typed input still refuses it.
- [version-compatibility](version-compatibility.md) (partly) — the upgrade gate. It may accept what
  compat calls different, never the reverse; an upgrade writes the version and the migrated state.

## Not built

- [custom-tasks](custom-tasks.md) — a principle in force: no plugins, a custom task is a child
  process and arbitrary logic a sidecar. Open: sidecar idempotency, cancel reaching a sidecar.
- [deterministic-simulation](deterministic-simulation.md) — exhaustive crash points and
  interleavings. The races are over DB state, so a baton at transaction boundaries replays them.
- [literal-types](literal-types.md) — infer `"sent"` as `enum: [sent]`, unblocking discriminant
  narrowing (§9). Enum-aware merging comes first, or `?? false` infers an overlapping `oneOf`.
- [openapi-resolver](openapi-resolver.md) — `$openapi` spreads an operation's `method` and
  `responses` into a fetch. `allOf` is flattened in the resolver, not admitted to the language.

# Roadmap

One line per item. The argument lives in `specs/`; this is the index.

## Open

- [] **auth: `jwt` mode** — a proxy that forwards a signed token is the one shape `header` mode
  cannot verify, and its trust rests on a network fact genroc cannot test (specs/api-auth.md §2.1)
- [] **attribution** — no actor column and no actor on the audit log, so "who deployed v7?" is
  unanswerable forever for anything written before it lands; the `Principal` already exists
  (specs/api-auth.md §7)
- [] **TLS in genroc** — `--tls-cert`/`--tls-key`, which is what makes the token-only deployment
  "no proxy" rather than "no proxy except the one terminating TLS" (specs/api-auth.md §9)
- [] **metrics** — `/healthz` is binary; nothing reports backlog depth or the age of the
  oldest due `wake_at` (specs/resource-limits.md)
- [] **renew as a heartbeat** — the response is a count, so a worker holding several claims
  cannot tell which to abandon; per-token `renewed`/`lost` is also the only channel that can
  reach work in flight (specs/external-task-queue.md §Renew is the heartbeat)
- [] **cancel** — no terminal "an operator stopped this"; a paused tree holds its row forever
  and strands a waiting parent. `cancelling`/`cancelled` mirroring pause, reusing `failing`'s
  descendant drain, and riding the renew heartbeat to stop claimed work
- [] **instance retention** — logs prune and objects sweep, `process_instances` grows forever
- [] **deterministic simulation**, tier 1 — the only place `only_once` can be asserted
  (specs/deterministic-simulation.md)
- [] **guard narrowing** — a `switch` case's proof is discarded, so the routed task still
  needs a `?? default` that can never be evaluated (specs/guard-narrowing.md)
- [] **enum-aware canonicalization** — `mergeSimpleVariants` won't fold arms carrying an
  `enum`; prerequisite for literal types (specs/literal-types.md §4)
- [] **literal types** — `"sent"` infers as `string`; unblocks discriminated unions
  (specs/literal-types.md)
- [] **discriminated unions** — blocked on literal types (specs/discriminated-unions.md)
- [] **mid-process path sensitivity** — needs a DNF lattice; workaround is `?? default`
  (specs/path-sensitive-output.md §5)
- [] **action extensibility from a parent** — what a parent can hand a child is a fixed shape
  (specs/custom-tasks.md)
- [] **source resolution**: the structural phase and `$infer` (specs/source-resolution.md)
- [] **long-poll** on the external-task queue (specs/external-task-queue.md)
- [] **per-definition durability field** (specs/durability-levels.md §8)
- [] **delete the taint system** — dead since `secret: true` narrowed to config;
  `RedactContext` and `SecretAt` have no callers
- [] **pause as a debugging tool** — start an instance paused, step it with `tick`
- [] **docs** — the site ships four pages; the reference gap it was written to close is open

## Shipped

- [x] auth — permissions on every action, `token` and `header` modes, the session exchange, and
      a default that warns when it is exposed (specs/api-auth.md; `jwt` and attribution remain open)
- [x] CLI mirroring the API, YAML, config file
- [x] versioning channels, and version compatibility as a check (`genctl compat`)
- [x] instance upgrade, gated on that check, one column, tree-closed
- [x] child processes — `child`, `child_map`, `child_list`
- [x] child→parent errors — `raise`, the `raised` status, error payloads, `case` on
  `on_error`, child retries (specs/child-error-handling.md)
- [x] `only_once` and the catchable `only_once.interrupted`
- [x] external tasks, and the queue a worker fleet pulls from — claim / renew / release /
  resolve, `external.lost`, outcome-as-signal (specs/external-task-queue.md)
- [x] script tasks — user TypeScript on the claim queue, no new engine capability
- [x] source resolution, code phase — `$import` resolved client-side, typecheck as exit code
- [x] lease fencing — frozen-worker repair, per-grant `lease_epoch`
- [x] durability levels — `--durability`, `--pg-commit-delay`, `--sqlite-fullfsync`
- [x] object store — content-addressed, ref-owned, grace window on collection
- [x] lazy context — a `Context` that answers a path and loads only what it needs
- [x] resource limits and readiness — response cap, shared client, jittered backoff,
  listener timeouts, `GET /healthz`
- [x] typed data flow — inference, recursive shapes, the unknown type, path-sensitive output
- [x] expressions — lambdas and map, object/array literals, string-literal indexing,
  computed keys, typed values and the Shape grammar
- [x] delay syntax — `for` / `until` / `tz`
- [x] fetch surface — `query`, status-keyed `responses`, `self.status` / `self.headers`
- [x] config vars and secrets, resolved per tick from the environment
- [x] pause / resume / retry, taking several ids
- [x] per-instance logs, pagination, filtering, the detail endpoint
- [x] Go + REST error handling — codes on `Reply`, db sentinels, a panic barrier
- [x] the docs site — Astro, auto-deployed, benchmark history preserved

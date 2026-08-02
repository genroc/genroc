# internal/engine

## `only_once`, `only_once.interrupted`, and the unknowable set

An `only_once` task whose previous attempt was interrupted is never re-run by the engine.
It raises **`only_once.interrupted`** — the one engine-produced code that `on_error` can
catch — so a definition can ask the system of record what actually happened and then
continue, or deliberately route back into the task to re-run it. Uncaught, it is the same
terminal failure it always was. Design and rejected alternatives:
[docs/only-once-interrupted.md](../../docs/only-once-interrupted.md).

The registration-time half of this rule (the three retry tiers, unknown-key rejection)
lives in `internal/model` — see [internal/model/CLAUDE.md](../model/CLAUDE.md).

Three things that break silently if you touch this:

1. **The evidence is `worker_id`, and it is transient.** `ReclaimedExpired` is derived per
   claim from the row having carried a `worker_id`, and every ordinary write clears that
   column. So a decision about an interruption can never be deferred past the next write —
   which is why `advance()` resolves it *before* settling a pending pause, ignoring the
   `pausing` status to do so, and why `settlePausing` must not regain an `only_once`
   branch. The routed instance still lands `paused`: it writes status `running` and the
   `CASE` in `UpdateInstance` maps `pausing` + `running` → `paused` (see the pause
   invariants in [internal/db/CLAUDE.md](../db/CLAUDE.md), applied to a path that used to
   opt out of them). Anything that hands a row back by clearing `worker_id` re-runs
   `only_once` tasks that must never re-run.
2. **The unknowable set is `only_once.interrupted`, `http.timeout`, `external.timeout`** —
   the errors where the request left and nothing came back. On an `only_once` task these
   can never be retried, and `not_reached: true` does **not** override them: that flag
   asserts what an error *means*, which is a claim only about an error that returned.
   `errcode.Unknowable()` is the list; `Code.IsUnknowable()` its predicate, mirroring
   `IsNotReached()`.
3. **`isRetryAllowed` (`error.go`) refuses at runtime too**, which is not redundant:
   validation runs only at registration, and definitions stored before the rule keep their
   `on_error` verbatim.

`errcode.MatchCode` lives in `errcode` rather than `transport` because that is the package
that owns codes; the engine and `internal/validation` share the one implementation.

## The backoff curve (`backoff.go`)

An `on_error` rule's `retry` policy supplies the base delay, the growth factor and the
ceiling; `backoff` turns them into the wait before attempt *n*. Design, and the survey of
other engines the field set came from: [docs/retry-policy.md](../../docs/retry-policy.md).
The registration-time rules are in [internal/model/CLAUDE.md](../model/CLAUDE.md).

1. **Jitter may only shorten.** It is applied to the upper half of the window
   (`d/2 + rand[0, d/2]`), so the returned delay is always **≤** nominal. That is
   load-bearing twice over: the ceiling stays a true ceiling, and the clock-advancing
   integration tests still expire a retry timer by advancing the nominal amount. Widening
   the jitter above nominal breaks the second silently — as intermittent failures in tests
   that have nothing to do with retries.
2. **The growth accumulates in `float64`, and stops at the ceiling.** The predecessor
   shifted a `time.Duration` and had to clamp the exponent by hand: `time.Duration` is
   int64 *nanoseconds*, so `1<<attempt * time.Second` overflowed the multiply at attempt 34,
   returning about minus forty years, and a flat `0s` from 62 up. A zero or negative delay
   is a hot retry loop against an already-failing endpoint, and `attempts` has no upper
   bound at registration to keep a definition out of that range. Anything that reintroduces
   integer growth reintroduces that.
3. **Read a policy's slots through `Base`/`Growth`/`Ceiling`, never off the struct.** An
   unset slot is the zero value, and a zero base is that same hot loop while a zero ceiling
   clamps every wait to nothing.

## Lease fencing (partly implemented)

[docs/lease-fencing.md](../../docs/lease-fencing.md). *Live:* the stale-lease gate
(`Engine.leaseGate`). Before every claim the pump checks how long ago a renewal last
succeeded; on stale evidence it repairs its own leases and passes `db.SkipTakeover` for one
lease period, so a resumed laptop or a throttled container keeps the work it was doing
instead of re-claiming its own in-flight rows and dying of `OverwhelmError`. *Still
proposal:* the fence — a `lease_epoch` token bumped by `ClaimInstances` (never by renewal)
and checked on every write a worker makes while holding the lease, so a stale advance's
write is refused rather than clobbering. Two traps the doc records: `worker_id` cannot
serve as the token (the reclaiming worker is usually the same worker), and nothing may hand
a row back by clearing `worker_id` — that column is the evidence `ReclaimedExpired` is
derived from, and erasing it re-runs `only_once` tasks that must never re-run.

## Pointers

- `delayArity` (`action.go`) — the `for`/`until` arity rule and why it must fail loudly:
  [internal/delayspec/CLAUDE.md](../delayspec/CLAUDE.md).
- Pause settlement, `SpawnChildrenAndWait`, `FailAncestors`, `CountActiveSiblings` —
  [internal/db/CLAUDE.md](../db/CLAUDE.md).

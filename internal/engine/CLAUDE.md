# internal/engine

## `only_once`, `only_once.interrupted`, and the unknowable set

An `only_once` task whose previous attempt was interrupted is never re-run by the engine.
It raises **`only_once.interrupted`** — the one engine-produced code that `on_error` can
catch — so a definition can ask the system of record what actually happened and then
continue, or deliberately route back into the task to re-run it. Uncaught, it is the same
terminal failure it always was. Design and rejected alternatives:
[specs/only-once-interrupted.md](../../specs/only-once-interrupted.md).

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

## advance decides, persist writes

`advance()` writes no state. Everything a step changes travels back in `advanceOutcome` —
including rows that are not the instance's own (a spawned batch, an external wait) — and
`persist` applies it in **one transaction**, which is also the only place the lease is
released. That is what makes "one advance, one commit" checkable by reading `persist` alone,
and it is load-bearing for three things that break silently if a path starts writing for
itself again:

1. **The in-flight marker.** `runAdvance` drops it immediately *before* every write and
   never after. The reverse order leaves the row claimable while still marked, and
   `dispatch` reads that as re-claiming live work and takes the worker down over an
   instance it had in fact finished with. Dropping it early is safe in the other direction:
   nothing can be handed a row whose lease this worker still holds.
2. **A refused write is the instance's failure, not the worker's.** Spawning and arming can
   fail on the state of the row (a parent already parked, a vanished instance), so
   `runAdvance` converts those into `engine.spawn` via `failInstance` and writes that —
   the verdict those paths reached themselves when they still wrote for themselves.
   `advanceOutcome.writeVerb` is what marks the two.
3. **The one outcome that keeps the lease.** An external arm is a read-modify-write: the
   database decides under the instance row lock whether to park or to consume a signal that
   reached the task first, and that atomicity is why a signal racing the arm cannot be lost.
   On a consume, `persist` hands the result back in memory and asks for another advance pass
   (`continued = true`, which skips the session-scoped `work_started` and reclaim handling).
   Each pass consumes one buffered signal, so the loop terminates.

Logs are deliberately outside this: `audit()` writes each line as the advance runs, in its
own transaction, so a crash leaves a trail of what the worker was doing even when nothing
committed. The audits for spawn and arm are the exception — they run *after* their commit so
they can never name children that do not exist.

## The backoff curve (`backoff.go`)

An `on_error` rule's `retry` policy supplies the base delay, the growth factor and the
ceiling; `backoff` turns them into the wait before attempt *n*. Design, and the survey of
other engines the field set came from: [specs/retry-policy.md](../../specs/retry-policy.md).
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

[specs/lease-fencing.md](../../specs/lease-fencing.md). *Live:* the stale-lease gate
(`Engine.leaseGate`). Before every claim the pump checks how long ago a renewal last
succeeded; on stale evidence it repairs its own leases and passes `db.SkipTakeover` for one
lease period, so a resumed laptop or a throttled container keeps the work it was doing
instead of re-claiming its own in-flight rows and dying of `OverwhelmError`.

The gate rules its own rows out of a takeover on one invariant — **every lease this worker
holds expires at `lastRenewMs + leaseDuration` or later** — and both halves of it are a
clock read that has to happen at the right moment. `lastRenewMs` is stamped from the instant
the renewal *derived* its expiries from (`RenewWorkerLeases` returns it), never the clock
after the write; and the gate hands `ClaimInstances` the *instant* it decided at rather than
a flag the query resolves against its own clock. Either one read late credits the worker
with lease life it never wrote, and the pump then re-claims a row it is still advancing —
which is fatal, and only under load, which is where the reads run late.

*Still proposal:* the fence — a `lease_epoch` token bumped by `ClaimInstances` (never by renewal)
and checked on every write a worker makes while holding the lease, so a stale advance's
write is refused rather than clobbering. Two traps the doc records: `worker_id` cannot
serve as the token (the reclaiming worker is usually the same worker), and nothing may hand
a row back by clearing `worker_id` — that column is the evidence `ReclaimedExpired` is
derived from, and erasing it re-runs `only_once` tasks that must never re-run.

## A collected child is conformed against the parent's CURRENT task

`resolveAndValidateChildOutput` (`collect.go`) takes the `result_schema` from the parent's
task as the parent stands now — `task.Action.ResultSchema`, or `task.Action.Children[key]`
for a `child_map` — never from a copy taken at spawn. The schema used to be marshalled onto
every child row as `_spawn_result_schema`; dropping it was the prerequisite for upgrading a
live instance ([specs/version-compatibility.md](../../specs/version-compatibility.md) §5a).

Two things that break silently if it goes back:

1. **The conform NORMALIZES**, stripping undeclared properties. A field added to a child's
   output and the parent's `result_schema` in one release would arrive stripped for
   in-flight children, and the parent would read `null` — uncatchably, if the new schema
   declares it optional.
2. **The external path already resolves its schema this way**, from the pinned definition
   when the result arrives. Two answers to one question is what let them drift.

## Pointers

- `delayArity` (`action.go`) — the `for`/`until` arity rule and why it must fail loudly:
  [internal/delayspec/CLAUDE.md](../delayspec/CLAUDE.md).
- Pause settlement, `SpawnChildrenAndWait`, `FailAncestors`, `CountActiveSiblings` —
  [internal/db/CLAUDE.md](../db/CLAUDE.md).

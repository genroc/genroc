# Lease fencing: make a lost lease harmless

Status: **implemented** (drafted 2026-08-02; gate shipped 2026-08-02, fence 2026-08-05).

Live invariants moved to [internal/engine/CLAUDE.md](../internal/engine/CLAUDE.md) and
[internal/db/CLAUDE.md](../internal/db/CLAUDE.md); this file records the decisions and
the rejected alternatives.

Two deviations from the draft as shipped:

- The consume branch of the external arm does **not** keep the lease for a second advance
  pass. It is an ordinary fenced `UpdateInstanceProgress` that releases; the result lands
  durably in `external_data` and the next claim resumes via `runExternal` phase 2. Retired
  for uniformity ("no outcome keeps the lease") at the cost of one claim round trip per
  pre-buffered signal; the pop still rolls back with a refused write. `SetExternalResult`
  is therefore unfenced — its only callers act on parked rows under the row lock.
- The migration is 025, not 024 (taken by then).

## Motivation

A laptop slept ~3 minutes mid-`fetch`. Leases are DB wall-clock millis; the renewer is a
monotonic-clock ticker, which froze with the host. On wake the pump re-claimed the very
instance the same worker was still advancing, and the engine died of a fatal
`OverwhelmError` blaming `--max-concurrent`. Beyond the misdiagnosis, the real hazard was
never addressed: a lapsed lease invites another worker to take the row over, and the
frozen worker's write then lands unconditionally (`WHERE id = ?`) — a clobber after a
double execution. The lease granted **execution**; nothing tied it to the **write**.

Long leases would mask the problem but forfeit fast failover, which is the point of short
leases. So: stop the stale writer at the resource, not the stale worker by timing.

## The model

One column: `lease_epoch BIGINT NOT NULL DEFAULT 0` — a per-row fencing token counting
how many times the row has been **granted**.

- `ClaimInstances` bumps it in the same UPDATE that stamps `worker_id`/`lease_expires_at`
  and returns the new value on the instance. It is the only place the epoch moves.
- `RenewWorkerLeases` never touches it: a renewal extends a grant, it does not create one
  (which is also why the token cannot be `lease_expires_at`).
- Every lease-holding write carries `AND lease_epoch = ?`; zero rows affected means the
  grant is gone — the transaction rolls back with `db.ErrLeaseLost`.
- `worker_id` cannot be the token: in the incident the reclaiming worker *was* the frozen
  worker, so a `worker_id` predicate matches and fences nothing. Self-reclaim is the
  common case.

## The fenced write surface

Six lease-holding entry points; the fence goes on the leased row's UPDATE inside each
transaction, so a lost lease leaks no partial effects — a stale spawn inserts no
children, a stale arm-consume rolls its signal pop back to its FIFO position.

| entry point | fenced statement | rolls back with it |
|---|---|---|
| `UpdateInstanceProgress` | `UpdateInstanceProgress` | context object diff |
| `UpdateInstance` | `UpdateInstance` | context object diff |
| `FinishChild` | child's `UpdateInstance` | `WakeParent` |
| `FailInstanceAndAncestors` | child's `UpdateInstance` | `FailAncestors`, `WakeParent` |
| `SpawnChildrenAndWait` | parent's `UpdateInstance` | every `InsertInstance` |
| `ArmExternalOrConsumeSignal` | `UpdateInstanceProgress` (consume) / `UpdateInstance` (park) | `PopOldestSignal` |

Deliberately unfenced: inserts (no prior grant); operator verbs (pause/resume/retry act
on rows regardless of holder — retry binds the epoch it read under the tree lock, a
no-op); `ResolveExternalTask`/`DeliverSignal`/`SetExternalResult` (parked rows under the
row lock); `FailAncestors`/`WakeParent` (rows this worker never held — their right
derives from the fenced child write in the same transaction); log and object writes
(append-only; a trail that survives a lost lease is the point). Revival needs no bump of
its own: any stale advance was already fenced by the claim that produced the failure.

## On losing the fence

`runAdvance` **drops the outcome** — no retry, and specifically no `failInstance`, which
would be the clobber under another name — audits `lease_lost` (unfenced on purpose: the
only trace of the abandoned attempt, and the telemetry that replaced the fatal exit), and
stops renewing the row. Whether the task executed is unknowable; that is the ordinary
at-least-once contract, with `only_once` as the opt-out the next section protects.

## `only_once`: the takeover evidence must survive

The invariant: **every fence loss is preceded by a claim that observed the takeover** —
the epoch moves only on a claim, and a claim of a row still carrying a `worker_id` sets
`ReclaimedExpired`. So the `only_once.interrupted` verdict is always reached by the row's
new owner.

`worker_id` is that evidence, and an early draft destroyed it: a `ReleaseLease` that
cleared `worker_id` to hand back a self-reclaimed row made the next claim read clean and
**re-run the one class of task that must never re-run**. Do not reintroduce it.

The hand-back is renewer scoping instead: `RenewWorkerLeases` takes the worker's **held
set** (ids inserted on claim, removed when `runAdvance` returns — the lease must outlive
the write it protects, and the in-flight marker must drop before it). A row that leaves
the set stops being renewed, expires with `worker_id` intact, and the next claim observes
the takeover. During a long freeze the row is claimed and skipped once per lease period
(each bump harmless) until the doomed advance is fenced out — bounded churn, no stuck
row, and re-execution within one worker stays serialized. Rejected: letting the second
advance run concurrently and leaning on the fence (a deliberate duplicate, in-process,
with the information to avoid it — and for `only_once` the second call fires before
anything can refuse it).

## The stale-lease gate

The fence makes a sleeping laptop safe; the gate makes it a non-event. The wake race that
caused the incident (pump vs. renewer, both tickers firing at once) is decided by
evidence instead: before every claim the pump checks how long ago a renewal last
succeeded. Stale evidence ⇒ repair own leases synchronously (renewal re-stamps only
`worker_id = us` rows, and never bumps the epoch — a bump would fence out the advance the
repair just rescued), then claim with takeovers suppressed (`SkipTakeover`) for one lease
period, so co-frozen peers get to repair theirs.

Four choices, each with a wrong-looking neighbour:

- **Renewal gap, not wall-vs-monotonic drift.** Drift only sees suspends; CFS throttling,
  cgroup freezes and a dead DB ride the monotonic clock. The gap measures the actual
  question — were my leases renewed? — so the old `freezeDetector` was deleted, not
  extended.
- **Renewal gap, not claim gap.** A saturated pump parks on `e.sem` for minutes with
  perfectly healthy leases; and a claim stamps only the rows it took, so claim-derived
  freshness can be fresh while held leases rot.
- **Checked by the claimant, not the renewer.** Makes both wake orderings correct; the
  renewer's unconditional job already is the repair.
- **The verdict is an instant, not a flag.** The invariant "every held lease expires at
  `lastRenewMs + leaseDuration` or later" holds only if the stamp is the instant the
  renewal *derived* expiries from (never the post-write clock) and the claim binds the
  gate's instant as its cutoff (never re-reading its own clock) — either read late lets a
  delayed claim reach its own rows. Trips one poll early (margin capped at half the
  lease) so repair happens while the lease is alive. No once-per-window repair bound: a
  repeating repair means the DB is unreachable, where the claim is failing per poll anyway.

A worker that never froze detects nothing and takes over a sleeping peer's rows on
schedule — the grace wins the wake race for workers that were out; it does not extend
ownership backwards. Residuals: a freeze landing *inside* `ClaimInstances` degrades to
fence-only (safe, wasteful); the floor is inexact by at most one claim transaction where
a renewal lands mid-claim — covered by the margin.

## What this retired

The fatal `OverwhelmError` (type deleted; `Run` returns nothing; `main.go` lost the fatal
branch). A self-reclaim is now a logged skip — error-level with the capacity remediation
outside a grace window, warn inside — and sustained overload reads as a stream of
`lease_lost` entries instead of a dead worker. `freezeDetector` was deleted (subsumed).
Mixed-version fleets are safe but unfenced during rollout: old binaries ignore the column.

## Tests

Go throughout (no HTTP surface freezes a worker), both engines for the DB layer; freezes
are simulated with `db.AdvanceClock`.

- Epoch mechanics, the per-entry-point fence ("stale ⇒ `ErrLeaseLost` and *nothing at
  all* changed"), renewer scoping and the hand-back evidence:
  `internal/db/dbtest/lease_epoch_test.go`.
- Engine behaviour — dropped outcomes, self-reclaim hand-back to completion, the frozen
  worker whose stale write cannot clobber the takeover verdict, the repair saving an
  `only_once` task through a freeze: `internal/engine/fence_test.go`; gate cases in
  `internal/engine/lease_test.go` (renamed from `overwhelm_test.go` — its
  graceful-exit test became unconstructible, which was the point).
- The pre-fence coverage of 5.2–5.4 (plain re-run, stored-rule refusal, `retry --force`)
  predates this spec: `interrupted_test.go`, `pause_retry_test.go`.
- Stress: `tests/stress/lease_pressure_test.ts` — the crippled worker must survive the
  configuration that used to kill it (zero unforced restarts), plus two SIGKILLs and an
  exactly-once tree aggregation.
- E2e (possible only since the fence — the frozen tick's write used to clobber the
  verdict): `tests/tick/lease_fence_test.ts` drives the sleep with `/tick advance_ms`, a
  concurrent manual tick as the wake, and reads every verdict over the API, including
  `lease_lost` in the logs. The gate half stays in Go: `Tick` deliberately has no gate.

## Open questions

- Should `lease_lost` be counted and exposed (an API field), or is the per-instance audit
  entry enough? Leaning enough — the loud case is a stream of them.
- Should the skipping claimant apply the `only_once` verdict immediately rather than
  deferring to the next claim? It holds the current epoch, so the write would be
  legitimate and a lease period faster; against it, `dispatch` gains a definition lookup
  and a `failInstance`, and the deferred path reuses the existing `prepareAdvance` check.
  Deferring wins unless the extra lease period turns out to matter.

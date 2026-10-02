# Lease fencing: make a lost lease harmless

Status: **Built.**

The silent-failure rules live in [internal/engine/CLAUDE.md](../internal/engine/CLAUDE.md) and
[internal/db/CLAUDE.md](../internal/db/CLAUDE.md); this records the decisions and rejected
alternatives.

## Motivation

Leases are DB wall-clock millis; the renewer is a monotonic ticker, which freezes with its host.
A laptop sleeping mid-`fetch` lets its leases lapse, another claim takes the row over, and the
frozen worker's write then lands unconditionally — a clobber after a double execution. The lease
granted **execution**; nothing tied it to the **write**. Long leases would mask this but forfeit
fast failover, so the stale writer is stopped at the resource, not by timing.

## The model

`lease_epoch BIGINT NOT NULL DEFAULT 0` counts how many times the row has been **granted**.

- `ClaimInstances` bumps it in the same statement that stamps `worker_id` / `lease_expires_at`
  (Postgres claim CTE; `GrantLeases` on SQLite). Nothing else moves it.
- `RenewWorkerLeases` never touches it: a renewal extends a grant (which is also why the token
  cannot be `lease_expires_at`).
- Every lease-holding write carries `AND lease_epoch = ? AND COALESCE(worker_id,'') = ?`; zero
  rows affected rolls the transaction back with `db.ErrLeaseLost` (`requireFenced`).
- **`worker_id` cannot be the token**: the reclaiming worker is usually the frozen worker itself,
  so self-reclaim — the common case — matches and fences nothing. The epoch decides it.
- **`worker_id` is a second conjunct** for the case the epoch cannot see: a **rewind**, where the
  database loses committed transactions while a worker survives, un-issuing a claim so the next
  one re-issues the same epoch to someone else. Postgres only (SQLite is in-process, so a rewind
  takes the worker with it): failover to a lagging replica, or an unclean shutdown under relaxed
  durability (durability-levels.md §7). So two live workers must never share an id, and the
  default is `hostname-pid-random`. One narrow rewind case stays open (§Open).

## The fenced write surface

The fence sits on the leased row's UPDATE *inside* each transaction, so a lost lease leaks no
partial effects — a stale spawn inserts no children, a stale phase-2 write leaves its signal at its
FIFO position. A fenced write also **releases** the lease without moving the epoch, so a second
write after a successful one is refused: it holds no grant.

| entry point | fenced statement | rolls back with it |
|---|---|---|
| `UpdateInstanceProgress` | `UpdateInstanceProgress` | context object diff, consumed signal |
| `UpdateInstance` | `UpdateInstance` | context object diff, consumed signal |
| `FinishChild` | child's `UpdateInstance` | `WakeParent` |
| `FailInstanceAndAncestors` | child's `UpdateInstance` | `FailAncestors`, `WakeParent` |
| `SpawnChildrenAndWait` | parent's `UpdateInstance` | every `InsertInstance` |
| `RespawnSlotsAndWait` | parent's `UpdateInstance` (`parkParentWaiting`) | slot retire, every `InsertInstance` |
| `ArmExternalUnlessSignalled` | `UpdateInstanceProgress` (skip park) / `UpdateInstance` (park) | — |

Deliberately unfenced: inserts (no prior grant); operator verbs, which act regardless of holder
(`retry` binds the epoch it read under the tree lock, a no-op); `ResolveExternalTask` /
`DeliverSignal` (parked rows, under the row lock, writing only the signal buffer and the un-park);
`FailAncestors` / `WakeParent` (their right derives from the fenced child write in the same
transaction); log and object writes (a trail that survives a lost lease is the point).

## On losing the fence

`runAdvance` **drops the outcome** — no retry, and specifically no `failInstance`, which would be
the clobber under another name — audits `lease_lost` (unfenced: the only trace of the abandoned
attempt), and stops renewing the row. Whether the task executed is unknowable: the ordinary
at-least-once contract, with `only_once` as the opt-out. Nothing here is fatal to the worker:
sustained overload reads as a stream of `lease_lost`, and a self-reclaim (the pump re-claiming an
instance still in flight here) is a logged skip — error-level with the capacity remediation, warn
inside a gate grace window.

## `only_once`: the takeover evidence must survive

**Every fence loss that follows a takeover is preceded by a claim that observed it**: the epoch
moves only on a claim, and claiming a row that still carries a `worker_id` sets
`ReclaimedExpired`. So the `only_once.interrupted` verdict is always reached by the row's new
owner. (A fence loss can also mean writing against a lease this worker already released — no claim
precedes that one, so "every fence loss implies a takeover" is not a premise to reuse.)

`worker_id` is that evidence. **Never hand a row back by clearing it**: a `ReleaseLease` doing so
made the next claim read clean and re-run the one class of task that must never re-run. The
hand-back is renewer scoping: `RenewWorkerLeases` takes the worker's **held set** (inserted on
claim, removed when `runAdvance` returns — the lease outlives the write it protects, and the
in-flight marker drops before it). A row leaving the set expires with `worker_id` intact, and the
next claim observes the takeover. During a long freeze the row is claimed and skipped once per
lease period until the doomed advance is fenced out — bounded churn, and re-execution within one
worker stays serialized. Rejected: letting the second advance run and leaning on the fence — a
deliberate duplicate, and for `only_once` the call fires before anything can refuse it.

## The stale-lease gate

The fence makes a sleeping laptop safe; the gate makes it a non-event. Before every claim the pump
checks how long ago a renewal last succeeded. Stale ⇒ repair its own leases synchronously
(renewal re-stamps only `worker_id = us` rows and never bumps the epoch — a bump would fence out
the advance the repair rescues), then claim with takeovers suppressed (`SkipTakeover`) for one
lease period, so co-frozen peers repair theirs.

Four choices, each with a wrong-looking neighbour:

- **Renewal gap, not wall-vs-monotonic drift.** Drift sees only suspends; CFS throttling, cgroup
  freezes and a dead DB ride the monotonic clock. The gap measures the actual question.
- **Renewal gap, not claim gap.** A saturated pump waits on `e.sem` for minutes with healthy
  leases, and a claim stamps only the rows it took.
- **Checked by the claimant, not the renewer.** Both wake orderings are then correct; the
  renewer's job already is the repair.
- **The verdict is an instant, not a flag.** "Every held lease expires at `lastRenewMs +
  leaseDuration` or later" holds only if the stamp is the instant the renewal *derived* expiries
  from, and the claim binds the gate's instant as its cutoff — either read late lets a delayed
  claim take its own rows. It trips one poll early (margin capped at half the lease) so repair
  happens while the lease is alive.

A worker that never froze takes over a sleeping peer's rows on schedule — the grace does not
extend ownership backwards. A freeze landing *inside* `ClaimInstances` degrades to fence-only
(safe, wasteful).

## Tests

Go throughout (nothing over HTTP can freeze a worker); freezes are simulated with
`db.AdvanceClock`.

- `internal/db/dbtest/lease_epoch_test.go`, both engines — epoch mechanics, every entry point
  ("stale ⇒ `ErrLeaseLost` and *nothing at all* changed"), renewer scoping and the hand-back
  evidence. `TestFence_ReusedEpoch*` pins the rewind case; both fail with `err=<nil>` if the
  `worker_id` conjunct is removed.
- `TestRunAdvance_DoubledAdvanceCannotFailTheInstance` (`internal/engine/persist_test.go`) — the
  release consequence, engine-side.
- `internal/engine/fence_test.go` — dropped outcomes, self-reclaim hand-back, a frozen worker
  unable to clobber the takeover verdict, the repair saving an `only_once` task; gate cases in
  `internal/engine/lease_test.go`.
- `tests/stress/lease_pressure_test.ts` — the crippled worker survives (zero unforced restarts),
  plus SIGKILLs and an exactly-once tree aggregation.
- `tests/tick/lease_fence_test.ts` — the fence over HTTP (`/tick advance_ms` as the sleep, a
  concurrent tick as the wake). The gate stays in Go: `Tick` has no gate.

## Open

- **A rewind inside `runAdvance`'s marker gap is not fenced.** The in-flight marker drops *before*
  persisting (a freed row still marked is a wedged instance), so a rewind there lets the same
  worker start a second advance at a re-issued epoch, `worker_id` matching on both. Closing it
  needs rewind detection (`pg_postmaster_start_time()`), a second mechanism to keep true — build
  if Postgres failover or relaxed durability makes that window real.
- **The skipping claimant's `only_once` verdict** is deferred to the next claim; revisit if a lease
  period matters.

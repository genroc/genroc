# Lease fencing: make a lost lease harmless

Status: **implemented** (drafted 2026-08-02; gate shipped 2026-08-02, fence shipped
2026-08-05).

- **Shipped 2026-08-02** — the stale-lease gate ([Repairing leases after a freeze](#repairing-leases-after-a-freeze)):
  `Engine.leaseGate` / `renewLeases` / `lastRenewMs` in `internal/engine/engine.go`,
  `db.Takeover` in `internal/db/db_claim.go`, tests in `internal/engine/lease_test.go` and
  `TestClaimInstances_SkipTakeover`. It needs no migration and touches no write path, which
  is why it went first — it removes the failure that prompted this document.
- **Shipped 2026-08-05** — the fence: `lease_epoch` (migration 025 — the doc below says
  024, which was taken by the time it landed), the fenced write surface, the held-set
  renewer scoping, `db.ErrLeaseLost`, the `lease_lost` audit event, and the retirement of
  the fatal `OverwhelmError` (the type is deleted; `Engine.Run` returns nothing). Tests:
  `internal/db/dbtest/lease_epoch_test.go` (§1–§3), `internal/engine/fence_test.go`
  (§4–§5), and `tests/stress/lease_pressure_test.ts` (the reshaped overwhelm stress test —
  the worker now must *survive* the pressure that used to kill it).
- One deliberate deviation (2026-08-05, revised the same day): the doc below says the
  consume branch fences `SetExternalResult` and keeps the lease for a second advance
  pass. As shipped, the consume is instead an ordinary fenced `UpdateInstanceProgress`
  that RELEASES the lease — the result lands durably in `external_data` and the next
  claim resumes via `runExternal` phase 2. The keep-the-lease special case was retired
  for uniformity ("no outcome keeps the lease"), at the cost of one claim round trip per
  pre-buffered signal; the pop still rolls back with a refused write, so the signal is
  never lost. `SetExternalResult` is therefore unfenced: its only callers act on parked
  rows under the instance row lock, where no grant exists to check.

## Motivation

On 2026-08-01 a laptop slept for ~3 minutes while a worker was mid-`fetch`. Leases
live in the DB as wall-clock unix milliseconds; the lease renewer is a Go ticker on
the **monotonic** clock, which stops with the host. Zero renewals ran, the 10s lease
lapsed, and on wake the pump's first `ClaimInstances` returned the very instance the
same worker was still advancing. `dispatch` read that as proof that renewal cannot
keep up and returned a fatal `OverwhelmError`, taking the whole server down with a
remediation message (*"lower --max-concurrent"*) that had nothing to do with the
cause.

The immediate fix (shipped) detects the freeze by comparing `db.Now()` against
`time.Now()` and skips the reclaim instead of exiting. It is not sufficient:

1. **It only catches suspends and clock steps.** During CFS quota throttling in
   Kubernetes, a cgroup freeze (CRIU checkpointing), swap thrash, or any stop-the-world
   stall, the monotonic clock keeps ticking. The lease still lapses, the detector sees
   nothing, and a server still exits over a condition that is not a `--max-concurrent`
   misconfiguration.
2. **It does nothing about the real hazard.** A lease that lapses is, by design, an
   invitation for *another worker* to take the instance over. That worker advances it
   while the frozen one is still holding a stale snapshot — and when the frozen one
   wakes, its write lands unconditionally, because every write in the persist path is
   keyed `WHERE id = ?`. Exiting the process does not prevent that; the double execution
   has already happened and the clobber happens on the way out.

The lease grants **execution**; nothing today ties it to the **write**. That is the gap,
and closing it is what this spec is about: stop the stale writer at the resource, rather
than trying to stop the stale *worker* by timing.

There is a second way out — make reclaim so slow (thresholds of an hour rather than
seconds) that no plausible pause can trigger it. genroc cannot take it: short leases are
what give it fast failover after a real crash.

## The model

Add one column:

```sql
ALTER TABLE process_instances ADD COLUMN lease_epoch BIGINT NOT NULL DEFAULT 0;
```

`lease_epoch` is a per-row fencing token: a monotonically increasing count of how many
times the row has been **granted** to an executor.

- `ClaimInstances` sets `lease_epoch = lease_epoch + 1` in the same UPDATE that stamps
  `worker_id` / `lease_expires_at`, and returns the new value on the claimed instance
  (`model.ProcessInstance.LeaseEpoch`, alongside `WorkerID` / `LeaseExpiresAt`).
- `RenewWorkerLeases` **never touches it**. A renewal extends an existing grant; it does
  not create a new one. This is precisely why the token cannot be `lease_expires_at`.
- Every write a worker makes *on the strength of holding the lease* carries
  `AND lease_epoch = ?` and checks rows-affected. Zero rows means the grant this advance
  was operating under is gone: the write is refused and the transaction rolls back with
  `db.ErrLeaseLost`.

**`worker_id` cannot serve as the token.** In the incident above the reclaiming worker
*is* the frozen worker — same hostname, same pid, same `worker_id` — so a
`AND worker_id = ?` predicate matches and fences nothing. Self-reclaim is the common
case, not the exotic one.

## The fenced write surface

A lease-holding worker reaches the DB through exactly six entry points. Each writes the
leased row inside a transaction; the fence goes on that row's UPDATE and nowhere else.

| entry point | fenced statement | rolls back with it |
|---|---|---|
| `UpdateInstanceProgress` | `UpdateInstanceProgress` | context object diff |
| `UpdateInstance` | `UpdateInstance` | context object diff |
| `FinishChild` | child's `UpdateInstance` | `WakeParent` |
| `FailInstanceAndAncestors` | child's `UpdateInstance` | `FailAncestors`, `WakeParent` |
| `SpawnChildrenAndWait` | parent's `UpdateInstance` | every `InsertInstance` for the batch |
| `ArmExternalOrConsumeSignal` | `UpdateInstanceProgress` (consume) / `UpdateInstance` (park) | `PopOldestSignal` |

Because the fence sits **inside** each transaction, a lost lease cannot leak partial
effects. The two that matter:

- A stale `SpawnChildrenAndWait` inserts no children. There is no world where the
  children exist and the parent was never parked.
- A stale `ArmExternalOrConsumeSignal` does not consume the buffered signal it popped —
  the pop rolls back with the refused write, so the signal stays queued for whoever owns
  the instance now.

Writes that are deliberately **not** fenced, and why:

- `SaveInstance` — an insert; there is no prior grant to check.
- `PauseProcess` / `ResumeProcess` / `RetryProcess` — operator verbs. They are not lease
  holders; their whole job is to act on rows regardless of who holds them. `PauseProcess`
  already coordinates through the `pausing` state and the `CASE` in `UpdateInstance`.
- `ResolveExternalTask` / `DeliverSignal` — act on a **parked** row (no lease held by
  anyone) under the instance row lock they already take.
- `FailAncestors` / `WakeParent` — rows this worker never held. Their right to be written
  derives from the fenced child write that precedes them in the same transaction, which is
  the correct dependency: no child write, no ancestor write.
- Log and object writes — append-only, and an audit trail that survives a lost lease is
  the point (below).

Revival paths (`RetryProcess`) need no epoch bump of their own: any advance that was
in flight when the instance failed was already fenced out by the claim that took the
instance over and produced the failure. The bump belongs on the grant, and `ClaimInstances`
is the only grant.

## What a worker does when it loses the fence

`persist` propagates `db.ErrLeaseLost` to `runAdvance`, which:

1. **Drops the outcome.** No retry, no fallback write, and specifically **no
   `failInstance`** — failing the instance would be exactly the clobber being prevented.
2. **Audits it on the instance**: a new `model.EventLeaseLost = "lease_lost"` entry at
   warn level. Log writes are not fenced, so this lands regardless of who owns the row,
   and it is what explains a `work_started` with no matching completion in the timeline.
   It is also the telemetry that replaces the fatal error: a worker that is genuinely
   overwhelmed now emits a stream of `lease_lost` entries instead of dying.
3. **Stops renewing the lease** — which is how the row gets handed back; see below.

The task may have executed, and nothing here can tell whether it did. That is inherent:
genroc is at-least-once for actions, and `only_once` is the opt-out that turns "may have
executed" into a failure instead of a retry. The next section is about keeping that opt-out
working, because it is the part this design can most easily break.

## `only_once`: the takeover signal must survive

An interrupted `only_once` task must **not** be re-executed. It already isn't, at three
enforcement points: `prepareAdvance` (`advance.go`) fails the instance with
`errcode.EngineOnlyOnce` when it claims a running row with `ReclaimedExpired` sitting on an
`only_once` action; the `pausing` branch of `advance` (`advance.go`) applies the same test
on the crash-recovery claim of a `pausing` row; and `isRetryAllowed` (`error.go`) refuses the ordinary retry of a
reached error on such a task. Failing is the only honest verdict —
the engine cannot distinguish "the call never left" from "the call landed and the response
was lost" — and the failure propagates to the tree exactly like any other, so the operator
gets `only_once.interrupted` on a `failed` process rather than a silent second charge.

(That failure is now *catchable* rather than merely terminal — see
[only-once-interrupted.md](only-once-interrupted.md), which renamed the code to
`only_once.interrupted` and lets `on_error` route it. Nothing in this document changed:
the verdict is reached in the same place, by the same owner, on the same evidence. Only
what happens after it differs. Code names below are written as they were at drafting.)

Fencing does not weaken that, and the reason is worth stating as an invariant:

> **Every fence loss is preceded by a claim that observed the takeover.** A write is
> refused only if the epoch moved, the epoch moves only on a claim, and every claim sets
> `ReclaimedExpired` when it takes a row that still carried a `worker_id`. So the
> `only_once` verdict is always reached by the row's *new* owner, which is the only party
> entitled to write it.

`ReclaimedExpired` is derived, in `ClaimInstances`, from the row having a non-null
`worker_id` at claim time. **That column is the evidence that an attempt was interrupted**,
and anything that clears it without advancing the task destroys the evidence.

Which is exactly what an earlier draft of this spec got wrong. It proposed a
`ReleaseLease(id, epoch)` — `SET worker_id = NULL, lease_expires_at = NULL` — so a skipped
self-reclaim would hand the row back. That release erases `worker_id`, so the *next* claim
sees a clean row, reports `ReclaimedExpired = false`, and **re-runs the `only_once` task it
was supposed to refuse.** A laptop sleep would silently double-execute the one class of task
that must never double-execute. Do not reintroduce it in that form.

### Handing the row back after a self-reclaim

The requirement is therefore: get the row claimable again *without* clearing `worker_id`.

After a self-reclaim the pump holds a fresh grant (epoch N+1) on a row whose in-flight
advance is still running under epoch N. That advance's write is already doomed, and
`dispatch` refuses to start a second one (the in-flight marker), so nothing is advancing
the row. Today `RenewWorkerLeases` would keep it alive forever — it renews everything with
`worker_id = us` — and the instance would never become claimable again.

**Scope the renewer to what this worker still intends to write.** `RenewWorkerLeases`
becomes an ID-list renewal (`… WHERE id IN (SELECT value FROM json_each(?)) AND worker_id = ?`,
same chunking, `json_each` already available on both engines) driven by the worker's own
held-lease set. A row this worker is not advancing stops being renewed, its lease expires
on its own, and the next claim finds `worker_id` still set — takeover observed,
`ReclaimedExpired = true`, `only_once` honoured. No release write, no new method, and the
one column that carries the evidence is never touched.

Two pieces of state, because the persist window makes them genuinely different questions:

| set | inserted | removed | answers |
|---|---|---|---|
| advancing | `dispatch` | before `persist` (as today) | may a second advance start? |
| held | `dispatch` | after `persist` returns | should the renewer keep this lease alive? |

They coincide except during `persist`, and that difference matters in both directions: the
lease must outlive the advance marker (a lease dropped at the start of `persist` could
expire mid-write and get the write fenced out), and the marker must be dropped before the
write that frees the instance (the existing ordering, so a freed instance is never still
marked).

Steady state during a long freeze: the row is claimed and skipped once per lease period —
each such claim bumps the epoch harmlessly — until the doomed advance finishes and is
fenced out, after which the next claim advances it for real (or fails it, if the task is
`only_once`). Bounded churn, no stuck row, and re-execution inside a single worker stays
**serialized**: the doomed attempt finishes before the replacement starts. Across workers
that guarantee is unavailable — the other worker starts the moment the lease lapses — which
is the honest division of responsibility: *within a worker we know enough to be precise, so
be precise; across workers we cannot, so fence.*

Alternatives weighed:

- **`ReleaseLease` clearing `worker_id`** — breaks `only_once`, as above.
- **`ReleaseLease` clearing only `lease_expires_at`**, keeping `worker_id` — preserves the
  evidence, but the renewer immediately re-stamps the row (it matches on `worker_id`), so
  it needs the scoped renewer anyway. At which point the release is redundant.
- **Let the second advance run** (drop the marker, rely on the fence). The plain
  at-least-once reading, and it deletes the most code. Rejected: it runs a duplicate of the
  action *concurrently with* the first, inside one process, when we have the information to
  avoid exactly that — and for an `only_once` task it would issue the second call before
  anything got the chance to refuse it.

## Repairing leases after a freeze

The fence makes a sleeping laptop **safe**. It does not make it **harmless**: as described
so far, a laptop that sleeps mid-task loses the entire in-flight advance (its write is
refused on wake) and, if the task is `only_once`, fails the process outright. That is the
correct verdict once the lease is gone — but on a single-worker machine the lease need not
be gone at all.

Look again at what happened on wake. Both tickers had been frozen, so both fired: the
renewer's and the pump's. **The pump won the race.** Had the renewer won, it would have
re-stamped `lease_expires_at` on every row this worker still held, the pump's claim would
not have matched any of them, and the sleep would have cost nothing beyond a dead HTTP
connection. The incident is a lost race, not an inevitability.

So: **a worker that has been out should notice, and hold off until everyone has caught up.**

### Detecting it: the renewer's own gap

The signal is in memory and costs nothing — the timestamp of the **last successful renewal
pass**. The renewer stamps it after each pass; before every claim the pump asks how old it
is. Older than one `leaseDuration` and the worker cannot prove its leases are alive: either
it was not running, or it was running but could not reach the database, and for this purpose
those are the same fact.

The stamp is the instant the renewal **derived its new expiries from**, not the clock when
the write returned — `RenewWorkerLeases` reports the former for exactly this reason. The two
differ by however long the renewal took, and dating the evidence from the later one credits
every lease with time that was never written: the worker then reads as fresh for that much
longer than its leases actually live, which is precisely the interval in which the gate
approves a claim over rows it is still advancing. It is a real fault, not a test artifact —
the renewal is a write against a loaded queue table, and the amount by which it can overrun
a poll interval is unbounded. The invariant to preserve: **every lease this worker holds
expires no earlier than `lastRenew + leaseDuration`.** Everything below rests on it.

Three choices inside that, each of which has a wrong-looking neighbour:

- **The renewal gap, not the wall-vs-monotonic drift** the shipped `freezeDetector`
  measures. Drift detects a suspend or a clock step and nothing else; a CPU-throttled
  container, a cgroup freeze, a long stop-the-world pause and a database that went away all
  ride the monotonic clock and produce no drift at all — while producing exactly the lapsed
  leases that matter. The gap measures the thing we actually care about (*were my leases
  renewed?*) instead of one of its possible causes, so it strictly subsumes the drift
  detector. This spec therefore **deletes `freezeDetector`** rather than extending it, and
  the log line names the observation — *"no successful renewal for 3m4s"* — rather than
  guessing at a cause.
- **Since the last renewal, not since the last claim.** The pump legitimately parks on
  `e.sem` for as long as its slowest advance whenever every slot is busy, so a claim-gap
  would declare a grace on a perfectly healthy, fully saturated worker. The renewer blocks
  on nothing but its own ticker and one query.
- **The pump reads it; the renewer does not act on it.** On resume every frozen ticker
  fires at once, and the pump and the renewer race — which is precisely how the incident
  happened. Having the claimant do the check makes both orderings correct: if the renewer
  runs first it repairs the leases and the pump then finds a fresh stamp and proceeds
  normally; if the pump runs first it finds a stale stamp and repairs before claiming.
  Nothing depends on a goroutine being scheduled in a particular order.

### What it does

On a stale stamp, before claiming:

1. **Repair.** Run a renewal pass synchronously. It is sound in a fleet because renewal
   matches on `worker_id = ?`: it re-stamps only rows nobody took while we were away. A row
   another worker claimed during the freeze carries that worker's id, does not match, and
   stays theirs — our stale advance's write is then refused by the fence, which is the right
   outcome. And because renewal does **not** bump the epoch (§1.3), a repaired lease leaves
   the in-flight advance's write valid: that invariant is what makes the repair lossless
   rather than merely tidy.
2. **Grace.** For `leaseDuration`, claim with takeovers suppressed — only rows with no
   `worker_id` at all. A host that suspends suspends every worker on it, and on wake they
   all race; a worker cannot repair a peer's leases, but it can decline to profit from them
   while the peer repairs its own. New and released work keeps flowing, so a graced worker
   is not idle; only theft pauses. `leaseDuration` is the natural window because it is by
   config contract at least one renew interval, so every live peer gets a pass — and a peer
   that is genuinely dead is taken over one lease period later than it otherwise would have
   been, which is the entire cost.
3. **Trips one poll early.** A lease stamped at `lastRenew` dies at
   `lastRenew + leaseDuration`, so staleness *reaching* the lease duration is the exact
   moment the evidence runs out, and a gate consulted once per cycle would find it already
   gone. Tripping a poll early repairs the lease while it is still alive, rather than after
   a peer has had a cycle's worth of chances to take the row. The threshold is therefore
   `stale + pollEvery >= leaseDuration`, capped at half the lease so a poll interval longer
   than the lease (a broken config) cannot park the worker in permanent grace and stop it
   recovering genuinely dead workers' rows.
4. **Hands down an instant, not a verdict.** What the gate approves is a *cutoff*: rows
   whose lease had already expired when it read its evidence. `ClaimInstances` binds that
   value rather than re-deriving one from its own clock, so the arbitrary and unbounded gap
   between the check and the query — a stop-the-world pause, a descheduled goroutine, a slow
   claim ahead of this one — delays the claim without widening what it may take. Since the
   gate approves a takeover only while `now < lastRenew + leaseDuration - margin`, and every
   lease this worker holds outlives `lastRenew + leaseDuration`, no such claim can reach its
   own rows however late it runs. A boolean flag cannot express this: it defers the
   comparison to a clock read that happens after the decision it belongs to.

As built there is **no "one repair per window" bound**, which an earlier draft of this
section called for. The only way to see a stale stamp twice is for the repair itself to
keep failing, which means the database is unreachable — and in that state the pump's own
claim is already failing once per poll, so a second failing query is not worth a special
case, let alone one that interacts with the grace window (a bound tied to the window
suppresses exactly the repair a short lease needs).

What this deliberately cannot do: a worker that never froze detects nothing and suppresses
nothing. A remote fleet member will take over a sleeping laptop's rows the moment their
leases lapse, and that is correct — from the outside, a three-minute-old lease is
indistinguishable from a crash. The grace wins the race on wake for the workers that were
out; it does not extend ownership backwards through the sleep.

### Where this leaves the laptop

| scenario | fence only | fence + repair + grace |
|---|---|---|
| single worker, ordinary task | advance re-runs from its last checkpoint | nothing is disturbed |
| single worker, `only_once` task | process fails `only_once.interrupted` | nothing is disturbed |
| co-resident workers on one host | a peer may steal the row on wake | takeover suppressed for one lease period |
| worker stalled by CPU throttling or a slow DB, not a suspend | stale write refused | same repair and grace — the gap detector does not care about the cause |
| remote fleet, sleep longer than the lease | peer takes over, stale write refused | unchanged — correct by design |

Residual: a freeze that lands *inside* `ClaimInstances` is detected only after the claim
returns, by which point our own rows may already carry a new epoch. It cannot cost us a row
— the cutoff was fixed before the call (§4 above) — so it degrades to the "fence only"
column: safe, just wasteful. Closing it completely would mean excluding the in-flight id set
from the claim predicate (`NOT IN (SELECT value FROM json_each(?))` on the hottest query in
the system, against the partial index migration 010 exists to keep cheap). Not worth it for
the tail.

The other residual is the one place the `lastRenew + leaseDuration` floor is not exact: a
renewal that lands between a claim's clock read and its commit re-stamps every row *except*
the one being claimed, which then expires that much earlier than the floor claims. The gap
is one claim transaction wide, and to actually cost a row it would have to exceed
`leaseDuration - margin` — ten seconds on the defaults, by which point nothing about this
worker is healthy. The margin covers it; do not read the floor as exact.

## What this retires

- **`OverwhelmError` stops being fatal, and stops being the detector.** `Run` no longer
  returns it and `cmd/genroc/main.go` loses that fatal branch. Note what it was actually
  detecting: a healthy renewer re-stamps every row this worker holds every few seconds, so
  an in-flight row's lease can only lapse if renewal was not happening — which is exactly
  the condition the stale-stamp gate reads directly, one poll earlier, with a repair
  attached. The self-reclaim check stays as a backstop (skip the dispatch, log at error
  level with the same remediation text, carry on), but it becomes near-unreachable: the
  gate now catches the same fault before the claim rather than after it.
- **`freezeDetector` is deleted, not extended.** The renewal gap subsumes it — same
  suspends, plus every stall that rides the monotonic clock — so keeping ~40 lines of
  wall-vs-monotonic comparison to choose an adjective for a log line is not worth it. The
  log reports the observation instead: how long the worker went without proving its leases.

## Implementation notes

- **Migration 024**, column appended at the end. `queries.sql` documents that the
  `GetInstance` column order mirrors the table's, and `instanceColumns` /
  `scanInstance` / the Postgres `ClaimInstances` scan list all follow it — appending
  keeps every existing position intact.
- **sqlc**: `UpdateInstance` and `UpdateInstanceProgress` move from `:exec` to
  `:execrows` so the wrapper can compare against 0 (`RenewWorkerLeasesChunk` already uses
  that annotation). `SetExternalResult` likewise. Keep `queries.sql` ASCII-only.
- **Both engines**: `lease_epoch = lease_epoch + 1` and `AND lease_epoch = ?` are plain
  SQL, no dialect branch. The Postgres claim already returns the row via `RETURNING`; the
  SQLite select-then-update path computes the new epoch in Go as `old + 1`, the same way
  it already reflects `worker_id` / `lease_expires_at` onto the returned instances.
- **Suppressing takeovers needs no second query.** `ClaimInstances` takes a `Takeover` — the
  instant the caller decided from, as the *bound value* for the lease cutoff. `SkipTakeover`
  is its zero value: no stamped lease can be at or below 0 (they are all
  `nowMillis() + leaseDur`), so `lease_expires_at <= ?` never fires and only the
  `worker_id IS NULL` branch of the predicate can match. The SQL text, its placeholder
  count and its plan are identical whatever the cutoff, on both engines — the partial index
  migration 010 tunes is walked exactly as before, just with a more selective filter.
  `AllowTakeover()` (a call, resolving to `Now()`) stays available for callers holding no
  leases of their own to protect — `Tick`, and the tests.
- **The stale-lease gate is two fields**: an `atomic.Int64` of db-clock millis stamped from
  each successful renewal pass — the instant that pass derived its expiries from, see
  §Detecting it — (and once in `Run`, before the renewer starts, so a young engine is not
  instantly "stale"), and a plain `int64` grace deadline touched only by the pump goroutine.
  Both use `db.Now()`, not `time.Now()` — the clock leases live in, and the one a test can
  shift.
- **The renewer** keeps its chunked one-transaction-per-chunk shape and its
  `lease_expires_at < new_expiry` termination predicate; only the row selection changes,
  from `worker_id = ?` to an ID list intersected with `worker_id = ?` (the `worker_id`
  guard stays, so a row whose advance already finished and cleared the owner is never
  re-stamped). The list is at most `--max-concurrent` long and is passed as JSON, the same
  way `ClaimInstances` already passes its id list on SQLite.
- **Cost**: one extra `BIGINT` per row, one more column in the claim UPDATE, and a
  predicate on an already-located PK row. No new index; `idx_instances_runnable` is
  unaffected. The renewer's per-pass cost is unchanged in row count and becomes PK-driven.
- **Mixed-version fleets** are safe but unfenced: the epoch only ever increases, and an
  old binary ignores the column. There is no ordering requirement on the rollout, only a
  window during which old workers can still clobber.

## Testing

Placement: this is DB internals and worker-lifecycle concurrency with no HTTP surface, so
it is Go throughout, and everything under §1–§3 must pass on **both** engines
(`POSTGRES_DSN=… go test ./internal/db/...` as well as the default SQLite run). Freezes are
simulated with `db.AdvanceClock` — it moves the clock leases are stamped against while
monotonic time stands still, which is exactly a resumed host.

Each case names the failure it guards; a case whose guard cannot be broken by any plausible
edit is not worth writing.

### 1. Epoch mechanics — `internal/db/dbtest/`

| # | case | guards |
|---|---|---|
| 1.1 | a claim bumps `lease_epoch` by exactly 1 and returns the new value on the instance | the token is granted, not read stale |
| 1.2 | successive claims of the same row (lease expired between) yield N, N+1, N+2 | monotonicity; a reused token fences nothing |
| 1.3 | `RenewWorkerLeases` leaves `lease_epoch` untouched while moving `lease_expires_at` | **the central invariant** — a renewal extends a grant, it does not create one; if renewal bumped, a worker would fence itself out every 3s |
| 1.4 | two workers race for one row: exactly one claim succeeds, exactly one bump | double-granting the same epoch (Postgres `SKIP LOCKED`; SQLite single-writer) |
| 1.5 | `PauseProcess` / `ResumeProcess` / `RetryProcess` / `ResolveExternalTask` / `DeliverSignal` leave the epoch untouched | operator and external verbs are not grants; bumping there would fence out a legitimately-running advance |
| 1.6 | a fresh instance and a row migrated from before 024 both start at 0 and are claimable | the default, and that a 0 epoch is not treated as "no lease" |

### 2. The fence, per entry point — `internal/db/dbtest/`

For each of the six lease-holding entry points, three cases: **current epoch → write lands
(1 row affected)**, **stale epoch → `ErrLeaseLost`**, and **stale epoch → nothing at all
changed**. The third is the one that catches a fence placed outside its transaction.

| # | entry point | on a stale epoch, assert unchanged |
|---|---|---|
| 2.1 | `UpdateInstanceProgress` | `task`, `wait_state`, `wake_at`, `retry_count`, context columns |
| 2.2 | `UpdateInstance` | the above plus `status`, `error`, `error_code` |
| 2.3 | `FinishChild` | child still non-terminal **and** the parent was not woken (`wait_state` still `waiting`) |
| 2.4 | `FailInstanceAndAncestors` | child not failed, **no ancestor flipped to `failing`**, parent not woken |
| 2.5 | `SpawnChildrenAndWait` | **zero children inserted**, parent not parked on `waiting` |
| 2.6 | `ArmExternalOrConsumeSignal`, consume branch | the popped signal is **still queued, at its FIFO position**, no `_external_result` written |
| 2.7 | `ArmExternalOrConsumeSignal`, park branch | not parked, no `_external` written, `wake_at` untouched |
| 2.8 | any of the above with a context value large enough to externalize | `process_objects` has no new rows and no dereference stamps from the rolled-back diff |

2.4–2.6 are the cases that matter most: they are the ones where a fence at the wrong nesting
level lets a *partial* effect commit, which is worse than either outcome on its own.

### 3. Renewer scoping — `internal/db/dbtest/`

| # | case | guards |
|---|---|---|
| 3.1 | renewing ids {A} leaves B's `lease_expires_at` alone, though both carry this `worker_id` | the scoping itself — without it a skipped self-reclaim is renewed forever and the row never returns |
| 3.2 | an id in the list whose row now has `worker_id` NULL, or another worker's id, is not re-stamped | the `worker_id` guard; re-stamping would resurrect a lease on a row we no longer own |
| 3.3 | a list longer than `renewChunkSize` renews every row and terminates | the chunk loop's exit condition survives the predicate change |
| 3.4 | **a row that stops being renewed expires, stays claimable, and still carries its `worker_id`; the next claim reports `ReclaimedExpired = true`** | the `only_once` evidence chain — this is the test that fails if anyone reintroduces a release that clears `worker_id` |
| 3.5 | renewing an **already-expired** lease that nobody took re-stamps it, and leaves the epoch alone | the repair pass; if renewal refused expired rows there would be nothing to repair, and if it bumped, the repair would fence out the advance it just rescued |
| 3.6 | renewing an expired lease that another worker has since claimed is a no-op | the `worker_id` predicate is what makes the repair safe in a fleet |

### 4. Engine behaviour — `internal/engine/`

| # | case | guards |
|---|---|---|
| 4.1 | fence loss on a terminal outcome: outcome dropped, **no `failInstance`**, worker keeps running | the stale worker must not convert its lost write into a failure — that is the clobber under another name |
| 4.2 | fence loss on a progress checkpoint: same | the common path, since most advances checkpoint |
| 4.3 | a `lease_lost` entry lands on the instance despite the lost lease | log writes are unfenced on purpose; this is the only trace of the abandoned attempt |
| 4.4 | self-reclaim: no second advance starts, the reserved slot is released, `Run` returns no error | the skip, and the slot leak that a naive early-return would cause |
| 4.5 | a slow `persist` (large object writes) keeps its lease to the end and its write lands | the held-set lifetime — dropping the lease at the start of `persist` lets a long write fence itself out |
| 4.6 | a claim immediately after a freeing write dispatches normally rather than skipping | the advance marker still drops *before* the write, so a freed instance is never still marked |
| 4.7 | after the doomed advance is fenced out, a later claim advances the instance from its last checkpoint to completion | no stuck row; the whole hand-back path end to end |
| 4.8 | two workers, one instance, A frozen: B advances to completion, A's write refused, **the final row is B's outcome** | the multi-worker hazard the design exists for; run it with A's outcome terminal-failed and B's completed, so a lost fence would be visible as a wrong terminal state |
| 4.9 | manual tick mode (`pollEvery == 0`) also fences and drops cleanly | `Tick` keeps no marker; the fence-loss path must not assume one |
| 4.10 | **freeze, then wake: the repair pass runs before the claim, the in-flight instance is not reclaimed, and its advance completes normally** | the whole repair path — the laptop case end to end, and the one that turns "safe" into "harmless" |
| 4.11 | the repair does not bump the epoch, so the rescued advance's write still lands | the coupling between §1.3 and the repair; a renewal that bumped would rescue the lease and destroy the work |
| 4.12 | during the grace window the claim takes unowned rows but skips rows carrying another worker's id; after it expires, takeover resumes | the co-resident rule — including that it suppresses *takeovers only*, so a graced worker is not idle |
| 4.13 | a worker that did **not** freeze claims an expired peer lease immediately | the grace is triggered by detection, never by policy; a healthy fleet must not slow down because one member slept |
| 4.14 | **a saturated worker — every `e.sem` slot busy with a long advance — never enters a grace** | the false positive the "since last renewal, not since last claim" choice exists to avoid; a busy worker's pump parks for minutes while its renewer is perfectly healthy |
| 4.15 | ~~a persistently stale stamp triggers one repair per grace window~~ — **dropped**, see the note under §What it does; a repeated repair only happens when the DB is unreachable, where the pump is already failing a claim per poll | — |
| 4.16 | a stall that leaves the monotonic clock ticking (simulate by holding the renewer off without shifting the clock) still trips the gate | that the detector reads the renewal gap and not a suspend signature — the case `freezeDetector` could not see |

### 5. `only_once` — `internal/engine/`

| # | case | guards |
|---|---|---|
| 5.1 | **freeze mid-`only_once` action → instance ends `failed` with `only_once.interrupted`, endpoint hit exactly once** | the reason this spec exists; fails if the takeover signal is lost anywhere in §3–§4 |
| 5.2 | the same scenario with `only_once` absent → endpoint hit twice, instance completes | that the guard is specific to `only_once` and has not silently become "never retry anything" |
| 5.3 | the `pausing` reclaim path still refuses to re-run an interrupted `only_once` task, and `isRetryAllowed` still refuses its retry | the other two enforcement points, both easy to miss when touching the first |
| 5.4 | `RetryProcess(force)` still overrides `only_once` on a tree failed this way | the documented escape hatch survives |
| 5.5 | **single worker, freeze mid-`only_once`, repair enabled → the instance completes; the endpoint is hit exactly once and the process does not fail** | the payoff case: with the repair the lease was never lost, so there is no interrupted attempt to adjudicate. Pairs with 5.1, which is the same scenario once the lease *is* genuinely gone |

### 6. Existing tests this changes

Done with the gate (`internal/engine/lease_test.go`, renamed from `overwhelm_test.go`):

- `TestOverwhelm_GracefulExit` **became unconstructible as written**, and that was the
  point. It fabricated the overwhelm by setting the renew interval to a minute so the
  renewer never re-stamped the lease — exactly the stale stamp the gate now catches first,
  repairing the lease before the claim that would have self-reclaimed it. It is now
  `TestLeaseGate_RepairsInsteadOfExiting`: same setup, opposite assertion (§4.16 as well,
  since no clock is shifted — the renewer simply is not running).
- 4.10 is `TestLeaseGate_SurvivesFrozenHost`, 4.14 is
  `TestLeaseGate_SaturatedPumpDoesNotTrip`, and 4.12/4.13 are covered at the DB layer by
  `TestClaimInstances_SkipTakeover`. Both engine tests were verified to fail with the gate
  disabled, reproducing the original `OverwhelmError` verbatim.
- The self-reclaim skip lost its natural end-to-end trigger, so `TestDispatch_SelfReclaim`
  covers it directly: seed the in-flight marker, call `dispatch` once with the grace
  inactive (expect `*OverwhelmError`) and once with it active (expect a skip). White-box,
  but deterministic and fast, and the branch is now a backstop.
- `TestGracefulShutdown_ReleasesLeases` passes unchanged.

Done with the fence (2026-08-05):

- §1–§3 are `internal/db/dbtest/lease_epoch_test.go`, both engines. 3.3 rides
  `TestRenewLease_ReportsWhatItWrote` (1000 ids through 100-row chunks); §2.8 has no
  black-box detector — the object diff rides the same transaction as the fenced write,
  and the gc_chaos stress test asserts the reachability invariant.
- §4/§5 are `internal/engine/fence_test.go`: 4.1–4.3/4.9 =
  `TestRunAdvance_LeaseLostDropsOutcome`, 4.4/4.7 (+3.4 end to end) =
  `TestSelfReclaim_RowHandsBackAndCompletes`, 4.8/5.1 =
  `TestFence_TakeoverVerdictOutlivesTheFrozenWorker`, 5.5 =
  `TestLeaseGate_RepairSavesOnlyOnceThroughFreeze`. 4.11 is pinned at the DB layer (§1.3,
  §3.5) and end-to-end by `TestLeaseGate_SurvivesFrozenHost`, which now completes only
  because the repair leaves the epoch alone. 4.5/4.6 have no test of their own: the
  held-set lifetime and marker order are each one line in `runAdvance`, and
  `TestSelfReclaim_RowHandsBackAndCompletes` fails if either moves.
- 5.2–5.4 predate the fence: `TestInterrupted_PlainTaskStillReRuns`,
  `TestInterrupted_StoredRetryRuleIsNotRetried` / the `pausing` branch tests, and the
  RetryProcess force tests in `pause_retry_test.go`.
- `TestDispatch_SelfReclaim` now asserts the skip in both grace states (the error return
  is gone); `TestGracefulShutdown_ReleasesLeases` passes unchanged — the held set does not
  keep a lease alive past the drain.
- The stress half is `tests/stress/lease_pressure_test.ts` (was
  overwhelm_recovery_test.ts): the crippled worker must ride out the pressure with zero
  supervisor restarts, survive two SIGKILLs, and every tree must still aggregate exactly.

### Not at the e2e layer, and why

Nothing here is reachable through the HTTP API: there is no endpoint that freezes a worker
or hands out a lease, and the observable difference (a `lease_lost` entry, an
`only_once.interrupted` failure) is a consequence of engine-internal timing rather than of a
request. The one candidate — asserting `lease_lost` shows up in `GET /instances/:id/logs` —
would test the log plumbing that dozens of e2e tests already cover, using a far more
elaborate setup. Keep it in Go.

## Open questions

- Should `lease_lost` be counted and exposed (an API field, a log summary), or is the
  per-instance audit entry enough? Leaning enough — the loud case is a stream of them.
- ~~Should the repair and the grace ship before the fence?~~ **Yes — done**, for the
  reasons given: no migration, no schema, no change to any write path, and on their own
  they turn the laptop case from "reclaim, skip, warn" into "nothing happened". The fence
  remains the correctness backstop for everything they cannot see (a stale advance whose
  row was taken by a remote worker while its owner was frozen).
- Should the skipping claimant apply the `only_once` verdict **immediately** rather than
  deferring it to the next claim? It holds the current epoch, so the write would be
  legitimate, and it would fail the instance a lease-period sooner. Against: it puts a
  definition lookup and a `failInstance` into `dispatch`, and the deferred path reuses the
  `prepareAdvance` check that already exists and needs no new code. Deferring wins unless
  the extra lease period turns out to matter.

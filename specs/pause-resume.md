# Pause/resume vs retry: design

Status: agreed and implemented 2026-07-20. The lifecycle operations live in
`internal/db/db_lifecycle.go` (`PauseProcess`, `ResumeProcess`, `RetryProcess`);
the pause-landing rules in `internal/db/queries.sql` (`UpdateInstance`,
`UpdateInstanceProgress`) and `internal/engine/error.go` (`settlePausing`);
statuses in `internal/model/instance.go`; migration `022_pause_states.up.sql`.
Tests: `internal/db/dbtest/pause_retry_test.go`, `tests/tick/tree_pause_test.ts`,
`tests/tick/tree_error_pause_test.ts`, `tests/tick/pause_retry_test.ts`,
`tests/integration/pause_test.ts`.

## Motivation

The system previously had `cancel` (`running` → `cancelling` → `cancelled`) and a
single `retry` that accepted **either** a `failed` or a `cancelled` root. One
verb served two situations that have nothing in common:

- A process that **failed** has spent the `on_error` budget its author
  configured. Reviving it is an override of the definition: it hands the tree an
  attempt the definition did not authorise, and with `force`, one that skips
  `only_once` protection too.
- A process an operator **stopped** was never owed anything. Restarting it should
  grant nothing at all — it should carry on exactly where it was.

Because `cancelled` was terminal, the shared implementation had to be the
*failure* one: `cancelInstance` destroyed state on the way down (`wait_state`
cleared, `wake_at` dropped when a retry backoff was pending), so `RetryProcess`
had to *reconstruct* the interrupted path — re-walking children to decide
waiting-vs-collecting, and asking the `only_once` question on the way.

That produced a concrete bug as well as a naming problem: cancel+retry silently
converted "wait 30s, then retry" into "retry immediately", because both halves
cleared `wake_at`. Correct for reviving a failure; wrong for resuming a pause.

## The model

`paused` is **not an outcome**. It means only *"does not advance
automatically"*. The instance keeps its `wait_state`, `wake_at`, `retry_count`
and context verbatim, and its timers keep running. Only `completed` and `failed`
are terminal (`model.Status.Terminal()`).

| verb | precondition | effect |
|---|---|---|
| `pause` | root, `running` | subtree → `pausing` if leased, else `paused` |
| `resume` | paused rows anywhere in the subtree | → `running` |
| `retry` | root, `failed` only | revive walk, keeps `force` |

`cancelling`/`cancelled` were **removed**, not renamed: with no terminal
operator-driven stop, the whole draining state machine around them
(`cancelInstance`, the cancel-vs-retry precedence in `handleCallError`, the
cancelling propagation in `SpawnChildrenAndWait`) went with them. Migration 022
maps existing rows (`cancelled` → `paused`, `cancelling` → `pausing`) and
rebuilds the partial runnable index over the renamed draining state.

There is deliberately no permanent cancel. Every process must have a way back;
if a terminal stop is wanted later it slots in as a real status alongside
`failed`, leaving `paused` untouched.

## The decisions

### 1. Pause is non-destructive, so resume is a status flip

`PauseProcess` changes the `status` column and nothing else. Everything that
made `RetryProcess` complicated is therefore absent from `ResumeProcess`: no
revive walk, no `wait_state` reconstruction, no `only_once` question, no
`force`. The instances resume exactly where they stopped.

This is the whole design in one sentence, and it is why the two verbs must not
be merged again. The asymmetries fall out rather than being chosen:

| | resume (paused) | retry (failed) |
|---|---|---|
| retry budget | untouched | deliberately exceeded |
| `wake_at` | preserved | backoff cleared, so the task runs now |
| `only_once` | not asked | asked, `force` overrides |
| repeatability | mechanical | a judgement call each time |

### 2. `pausing` means *leased*, not *not-yet-seen*

A row lands in `pausing` only if a worker currently holds it (`worker_id IS NOT
NULL AND lease_expires_at > now`). Everything parked — waiting on children, on a
delay, on an external task — has nothing in flight and goes straight to
`paused`.

This is load-bearing, not cosmetic. An instance parked with
`wait_state='waiting'` is excluded from `ClaimInstances`, so marking it `pausing`
would leave it draining forever: no claim could ever settle it. The old cancel
path dodged this only because `FinishChild` clears a draining parent's
`wait_state` — a trick pause cannot use, since preserving `waiting` is the point.

`pausing` remains in the claim predicate purely for **crash recovery**: a worker
that dies holding a `pausing` row leaves it leased-but-dead, and only a reclaim
can settle it (`settlePausing`).

An interrupted `only_once` task is resolved on that reclaim *before* the pause
settles, and `advance` ignores the `pausing` status to do it — the evidence
(`ReclaimedExpired`, derived per claim from `worker_id`) does not survive the
write that lands a pause, so a deferred decision would be made on evidence that
no longer exists. Running the recovery handler is not time-sensitive, so the
instance lands `paused` at whichever task the routing chose and runs it on
resume; with no handler it fails, a failure outranking a pause as ever. See
[only-once-interrupted.md](only-once-interrupted.md). `settlePausing` itself no
longer asks the `only_once` question, and must not regain it.

### 3. A pending pause lands in SQL, not in Go

A worker mid-task cannot know a pause arrived after it claimed. So the
`pausing` → `paused` transition is a `CASE` on the writes that release the lease:

```sql
-- UpdateInstance: guarded, so real outcomes still win
status = CASE WHEN status = 'pausing' AND CAST(:status AS TEXT) = 'running'
              THEN 'paused' ELSE CAST(:status AS TEXT) END

-- UpdateInstanceProgress: a checkpoint always means "still running"
status = CASE WHEN status = 'pausing' THEN 'paused' ELSE status END
```

A task that *completes* or *fails* the process writes that outcome; only a
still-running instance settles into `paused`.

`UpdateInstanceProgress` matters most: it is also the write that parks on a
delay or an external task. Parking moves the row to a `wait_state` that removes
it from the claim predicate, so the pause has to land on that write or never.
`SpawnChildrenAndWait` needs the same remap explicitly, for the same reason — it
parks the parent on `wait_state='waiting'`, and its children inherit the settled
status so a suspended tree never spawns runnable work.

### 4. A failure outranks a pause

`FailAncestors` includes `paused`/`pausing` rows. A branch that dies while the
tree is suspended still propagates `failing` upward — a failure is a real
outcome and must not be hidden by a suspension.

But a failing parent waits for every child, and paused children count as active
(`CountActiveSiblings` excludes only `completed`/`failed`). So a tree that loses
a branch while suspended **sits at `failing` over paused descendants** and cannot
settle on its own. That is not a bug; resuming is how the operator unblocks it —
the branches drain, the tree settles to `failed`, and it becomes retryable.

This is why `ResumeProcess`'s precondition is on the **subtree**, not on the
root's own status: here the root is `failing` while the rows that need resuming
are its descendants. `WakeParent` follows the same principle — a paused parent is
healthy, just suspended, so it is armed for `collecting` rather than the `''` a
`failing` parent gets.

### 5. Timers keep running while paused

`wake_at` is preserved and the clock is not rebased. A delay or external timeout
that elapses during a pause is simply **due the moment the process resumes** — it
fires on the next tick with no further clock advance.

The alternative (freeze the remaining duration, rebase on resume) was considered
and rejected: the distinction between `running` and `paused` should be *only*
"does it continue automatically", and nothing else.

### 6. Signals: delivery is not advancement

A paused instance still accepts signals. Rejecting them would make a pause lose
events, which is exactly what a pause must not do.

The arming test deliberately ignores status. If the instance is parked on an
external wait at that task, the result is **delivered**: `SetExternalResult`
stores it and clears the wait but leaves `status` alone, so the instance stays
unclaimable and does not advance until resumed. If it has not reached the task
yet, the signal **buffers** as usual and is consumed when the task arms after the
resume.

Treating a paused-but-armed task as unarmed looks safer and is not: an
already-armed task never re-arms, so the buffered result would sit unread
forever and the process would never complete. (This was caught by a test, not by
review.)

Relatedly, the external-task queue (`ListExternalTasks`) excludes paused rows —
`ResolveExternalTask` rejects anything not `running`, so advertising a suspended
task would hand external workers something they cannot submit a result for. They
reappear on resume.

### 7. Audit events, and one deliberate asymmetry

| event | level | scope |
|---|---|---|
| `inst_pause_requested` | info | root only, `meta: {instances, pausing}` |
| `inst_paused` | debug | each instance suspended outright |
| `inst_pausing` | debug | each instance leased when the pause arrived |
| `inst_resumed` | debug | each instance resumed |

Per-instance entries are debug because one call fans out over a whole subtree —
the same high-volume class as the `action_*` events. A tree of N instances costs
N debug rows per pause, where one *tick* of that tree already emits about 4N.

Only pause gets an info-level root entry, and only because its outcome can be
deferred: `meta.pausing` reports how many rows could not be stopped mid-task,
which nothing else records. Resuming is atomic — every row flips in one
transaction — so a root-level entry would restate what the per-instance ones
already say. The asymmetry is the point; it should not be "fixed" for symmetry.

Both operations log **after** the transaction commits, so a rejected call leaves
no trace.

`PauseProcess`/`ResumeProcess` therefore `SELECT ... FOR UPDATE` the rows they
will mutate and update by explicit id list (the `json_each` pattern used by
`FailAncestors`) rather than issuing a blind bulk `UPDATE`. Same lock order, same
deadlock properties, and it yields the per-instance outcome a row count cannot
express.

## Known gaps

**The deferred landing is not logged.** Every instance a pause touches logs
exactly one of `inst_paused` or `inst_pausing`, but the subsequent
`pausing` → `paused` transition emits nothing in the normal case: it happens as
a `CASE` inside the owning worker's write, which cannot report it back without
adding `RETURNING status` to `UpdateInstance`/`UpdateInstanceProgress` — the
hottest queries in the system, across seven call sites, and switching them from
sqlc `:exec` to `:one` would turn a no-match write from a silent no-op into
`ErrNoRows`. Judged a bad trade for an audit nicety. `inst_paused` *does* appear
for that instance when the landing goes through the crash-recovery path instead.

**Resume has no info-level trace.** At the default log level a tree shows a pause
and then `work_started`, which does not distinguish "an operator resumed this"
from "a worker picked it up". Attribution requires `?level=debug`. Accepted
knowingly in exchange for not restating atomic work at info level.

## Coverage

Where each decision above is pinned down, so a change that breaks one fails a test
that names it:

| decision | tests |
|---|---|
| 1. non-destructive pause / resume is a flip | `TestResumeProcess_RestoresSubtree` (wait_state, retry_count, wake_at all preserved); `tree_pause_test.ts` — whole-tree pause, pause mid-flight, pause while `collecting` |
| 2. `pausing` means leased | `TestPauseProcess_SingleInstance` (leased → `pausing`, unleased → `paused`); `tree_error_pause_test.ts`; parked-goes-straight-to-`paused` in `delay_test.ts` / `external_test.ts` / `tree_pause_test.ts` |
| 2. crash recovery via `settlePausing` | `crash_recovery_test.ts` — a `pausing` instance whose worker is SIGKILLed is settled by the reclaimer without re-running the task; the `only_once` variant fails instead of pausing, or, with an `on_error` handler, pauses *at the handler* and runs it on resume |
| 3. the pause lands in SQL | `TestUpdateInstance_LandsPendingPause` (and that a `completed` write still wins); `TestUpdateInstanceProgress_LandsPendingPause` (parking on `external`); `TestSpawnChildrenAndWait_PausingParent` |
| 4. a failure outranks a pause | `TestFailInstanceAndAncestors_OverridesPaused`; `TestFailInstanceAndAncestors_PausedSiblingKeepsParentWaiting`; `TestResumeProcess_FailingRootOverPausedDescendant`; `TestFinishChild_PausedParent_ArmsCollect`; the wedged-tree recovery in `tree_error_pause_test.ts` |
| 5. timers keep running | `delay_test.ts` (delay elapsing while paused); `external_test.ts` (external timeout elapsing while paused); `pause_retry_test.ts` (retry budget untouched across a pause) |
| 6. signals | `signal_test.ts` — delivered-but-not-advanced when armed, buffered when not yet reached; queue exclusion in `external_test.ts` |
| 7. audit events | `tree_pause_test.ts` (root info entry + per-instance debug entries + no `inst_resume_requested` + rejected calls leave no trace); `tree_error_pause_test.ts` (`inst_pausing` on the leased node, and *not* `inst_paused`) |
| retry is failed-only | `TestRetryProcess_NonRetryableStatuses`; `pause_retry_test.ts`; `retry_test.ts` |
| concurrency | `TestStress_PauseProcess_vs_FinishChild`, `..._vs_FailInstanceAndAncestors`, `TestStress_RetryProcess_vs_PauseProcess`; `gc_chaos_test.ts` (pause/resume under SIGKILL chaos); `multi_worker_test.ts` (Postgres worker fleet) |

**Not covered: migration 022's data migration** (`cancelled` → `paused`,
`cancelling` → `pausing`). The runner only exposes `m.Up()`, so a test database
is always created at the latest schema and never holds legacy rows; exercising
it would need stepped-migration machinery that does not exist yet. The index
rebuild in the same migration *is* covered indirectly — every claim-path test
depends on it.

## Not built (yet)

Pause was chosen partly as the foundation for **step-debugging**: a paused
instance is already exactly "will not advance unless told to", so starting an
instance paused and stepping it with `tick` is a small addition on top —
per-instance tick endpoint plus a step-granularity decision (one `advance()`
call is the natural unit, since it already collapses call-less routing tasks and
stops at the first side effect). Tracked in `ROADMAP.md`.

## Prior art

Camunda 7 separates the same three axes: suspension (`PUT
/process-instance/{id}/suspended`, reversible, also available per definition and
per job), termination (`DELETE`, terminal), and retry-budget manipulation
(incrementing a failed job's `retries`, which is explicitly an override of
configuration). Camunda 8 dropped suspension entirely in the Zeebe rewrite and
tracks it as an open feature request — its "paused" proposal is defined
negatively across three subsystems ("no element is executed, no job can be
activated or completed and no event is correlated. Only resume and cancel is
possible"), which is expensive in an event-sourced partitioned engine and nearly
free here, where the scheduler is a claim query over one table.

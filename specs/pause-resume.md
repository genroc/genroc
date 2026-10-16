# Pause/resume vs retry: design

Status: **Built.** Code: `db_lifecycle.go` (`stopTree`, `settleUp`), `PauseTree`/`CancelTree`
and the keep-CASE in `queries.sql`, `settlePausing`/`settleCancelling` in
`internal/engine/error.go`. The silent-failure invariants live in
[internal/db/CLAUDE.md](../internal/db/CLAUDE.md).

## Motivation

A **failed** process spent its authored `on_error` budget, so reviving it is an override the
definition never authorised (with `force`, one that skips `only_once` too). A process an operator
**paused** was never owed anything and should carry on exactly where it was. One verb serving both
must be the destructive one — a shared cancel+retry silently turned "wait 30s, then retry" into
"retry immediately", both halves clearing `wake_at`.

## The model

`paused` is **not an outcome** — it means only "does not advance automatically". The instance
keeps `phase`, `wake_at`, `retry_count` and context verbatim; timers keep running.

- `pause` — root only; a `running` row goes to `pausing` if a worker holds it or a row below
  it, else `paused`. The root's status is the whole tree's.
- `resume` — `paused`/`pausing` rows anywhere in the subtree go to `running`; refused on a
  `failing`/`failed` root.
- `retry` — root only, `failed` only; resumes what the failure paused; `force` overrides
  `only_once`.
- `cancel` — root only, terminal (§Cancel). `cancelled` is settled beside `failed`,
  `RetryProcess` refuses it, and it never meets `paused`.

## The decisions

1. **Pause is non-destructive, so resume is a status flip.** Everything that makes retry
   complicated is *absent* from resume: no revive walk, no wait reconstruction, no `only_once`
   question, no force. The asymmetries fall out rather than being chosen (budget untouched vs
   deliberately exceeded; `wake_at` preserved vs backoff cleared). This is why the verbs must
   never re-merge.
2. **`pausing` means *a task is in flight at or below the row*.** A row a worker is on
   (`worker_id` set, lease lapsed or not) drains, and so does every row above it; the rest stop in
   the verb's own statement. A lapsed owner may still save, or a new claim fences it out; settling
   a held row directly let the late save undo the stop. One `UPDATE` makes the split
   (`PauseTree`): its subquery reads the held rows' call stacks once, through the partial index on
   `worker_id`, so a pause costs the rows it writes. It is uncorrelated on purpose — correlated it
   runs once per row — and it runs under `lockTree`, or a claim landing between its snapshot and
   its write pauses a parent over a running child. The interrupted-`only_once` verdict is resolved
   on the settling claim *before* the pause settles, since its evidence does not survive that write
   ([only-once-interrupted.md](only-once-interrupted.md)). `settlePausing` must not regain it.
3. **A stop settles bottom-up, so the root reports the tree.** A held row's own write keeps
   `pausing` (the CASE in `UpdateInstance`, only where the new status is `running`, so real
   outcomes win; `UpdateInstanceProgress` does not write status). The claim predicate grants a
   draining row whatever its `wake_at` or park, so the next claim settles it (`settlePausing`; its
   `paused` yields to a verb landing after that claim — a cancel lands, a resume stands), and
   `FinishChild` climbs from there: every parent whose batch has nothing busy settles in the same
   transaction (`settleUp`; paused is not busy). Settling in the holder's write instead skips the
   climb and leaves the ancestors draining forever — which is also why a spawn hands a pending stop
   to its children rather than settling it. The climb locks only the parent until the parent is
   draining, then the call stack (and a first failure every live row), re-running the transaction
   before its first write: every sibling locks its parent, so a wider lock serialises a fan-out.
4. **A failure outranks a pause, and pauses the rest of its tree.** `FailAncestors` includes
   paused/pausing rows. The failure that poisons the root (finds it `running`/`pausing`/`paused`)
   also runs `PauseTree` over the rows off its call stack, so doomed siblings stop rather than run
   to completion; later failures find nothing running and skip the tree lock. A failed root is
   therefore a quiescent tree: retry resumes what the failure paused, and resume refuses a
   `failing`/`failed` root, which would run siblings under a parent that can no longer collect
   them. `WakeParent` arms a paused parent for `collecting` (healthy, just suspended); a failing
   one gets `''`.
5. **Timers keep running while paused.** A timer elapsing during a pause is due at resume.
   Rejected: freeze-and-rebase — paused must mean *only* "does not continue automatically".
6. **Delivery is not advancement.** A paused instance still accepts signals (rejecting would
   lose events); the outcome waits, unclaimable, until resume. `DeliverSignal` decides armed-ness
   without testing status: treating paused-but-armed as unarmed would buffer a result no re-arm
   will ever read. The external-task claim queue excludes paused rows.
7. **Audit asymmetry is deliberate.** Per-instance entries are debug (subtree fan-out); only
   pause gets a root info entry, because only its outcome is deferred (`meta.pausing` counts the
   drainers). Resume is atomic, so a root entry would restate the per-instance ones — do not
   "fix" the asymmetry. Both log after commit; both lock the subtree in id order (`lockTree`, the
   order every tree verb shares) and write through a statement that returns each row's outcome,
   which a row count cannot.

## Known gaps

Resume has no info-level trace; attribution needs `?level=debug`. When a settle's `paused`
yields to a cancel or resume that landed after its claim (§3), the engine's audit still reads
"instance paused": it is written before the write that decides. The status is right; fixing the
line needs the write to report what it landed. Migration 022's data mapping is untested (test databases never hold legacy rows).

## Cancel

`cancelling`/`cancelled`, root-only, reusing this document's machinery: the split is pause's
(`CancelTree`; a row a worker is inside can only be ASKED to stop), so is the bottom-up settle,
and `settleCancelling` has `settlePausing`'s shape. A concluded root is left alone by both verbs:
cancelling what a failure paused would leave a retried parent waiting on cancelled children.
Three things differ, each because cancel is terminal where pause is reversible:

1. **The selector is every live status, not `running` alone.** A paused tree is exactly what an
   operator needs to dispose of, and a `failing` one draining a dead branch is the other.
2. **A failure does not outrank a cancel** — the reverse of §4. There is no later run for the
   failure to matter to, so `FailAncestors` excludes both cancel states and the operator's stop
   stands.
3. **`settleCancelling` does not resolve an interrupted `only_once` first.** That resolution
   routes `only_once.interrupted` into `on_error` so the process can carry on — which is what the
   operator just forbade.

What does NOT differ is §1: cancel writes the status column and nothing else, so a stopped tree
still records what each node was doing. Do not clear `phase`: `ReleaseExternalClaim` finds a
claim by `phase = 'external'`, and releasing is what the heartbeat tells a cancelled worker to do.
(Settling a failure clears the wait because there it genuinely ended; a cancel abandons one.)

Two seams. `ClaimInstances`'s status list and the `idx_instances_runnable` partial index are
**one predicate written twice**: `cancelling` must be in both, and a status in one but not the
other is either never scanned (stranding every draining row whose worker died) or index churn. And
`Status.Terminal()`'s SQL copies (`CountActiveSiblings`, `NonTerminalSubtree`) gain a terminal
status by hand, as `CountBusySiblings` follows `Status.Busy()`.

Reaching work already in flight is the heartbeat's job:
[external-task-queue.md](external-task-queue.md) §Renew is the heartbeat.

## Open

- Step-debugging on top of pause: start paused, then a per-instance tick, one `advance()` per step.
  Trigger: debugging a definition by stepping it (tracked in ROADMAP).

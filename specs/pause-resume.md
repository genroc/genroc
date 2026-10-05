# Pause/resume vs retry: design

Status: **Built.** Code: `db_lifecycle.go`, the pause-landing CASEs in `queries.sql`,
`settlePausing`/`settleCancelling` in `internal/engine/error.go`. The silent-failure invariants
live in [internal/db/CLAUDE.md](../internal/db/CLAUDE.md).

## Motivation

A **failed** process spent its authored `on_error` budget, so reviving it is an override the
definition never authorised (with `force`, one that skips `only_once` too). A process an operator
**paused** was never owed anything and should carry on exactly where it was. One verb serving both
must be the destructive one — a shared cancel+retry silently turned "wait 30s, then retry" into
"retry immediately", both halves clearing `wake_at`.

## The model

`paused` is **not an outcome** — it means only "does not advance automatically". The instance
keeps `phase`, `wake_at`, `retry_count` and context verbatim; timers keep running.

- `pause` — root only; `running` rows go to `pausing` if leased, else `paused`.
- `resume` — `paused`/`pausing` rows anywhere in the subtree go to `running`.
- `retry` — root only, `failed` only; `force` overrides `only_once`.
- `cancel` — root only, terminal (§Cancel). `cancelled` is settled beside `failed`,
  `RetryProcess` refuses it, and it never meets `paused`.

## The decisions

1. **Pause is non-destructive, so resume is a status flip.** Everything that makes retry
   complicated is *absent* from resume: no revive walk, no wait reconstruction, no `only_once`
   question, no force. The asymmetries fall out rather than being chosen (budget untouched vs
   deliberately exceeded; `wake_at` preserved vs backoff cleared). This is why the verbs must
   never re-merge.
2. **`pausing` means *a worker is on it* (`worker_id` set, lease lapsed or not), not not-yet-seen.**
   A lapsed owner may still save; either its save settles the row, or a new claim bumps the epoch,
   fences it out and settles the row; settling a lapsed row directly let the late save undo the
   stop. Only such a row drains; everything parked goes straight to `paused` — load-bearing,
   because a `children` row is excluded from claims, so marking it `pausing` would strand it
   forever. `pausing` stays claimable for a dead or lapsed owner (`settlePausing`); the interrupted-`only_once` verdict is
   resolved on that reclaim *before* the pause settles, since its evidence does not survive the
   settling write ([only-once-interrupted.md](only-once-interrupted.md)). `settlePausing` must
   not regain the question.
3. **A pending pause lands in SQL, not in Go.** A worker mid-task cannot know the pause arrived
   after its claim, so `pausing → paused` is a CASE on the lease-releasing writes — guarded in
   `UpdateInstance` (only where the new status is `running`, so real outcomes win),
   unconditional in `UpdateInstanceProgress` (a checkpoint means "still running"). Progress
   matters most: it is also the write that parks on a delay/external — the pause lands there or
   never. A spawn settles a pending pause or cancel (`settledAtSpawn`), and children inherit the
   settled status, so a stopped tree never spawns work that waits for a worker.
4. **A failure outranks a pause.** `FailAncestors` includes paused/pausing rows. Paused children
   count as active, so a tree that loses a branch while suspended sits at `failing` over paused
   descendants until resumed — which is why `ResumeProcess` keys on the *subtree*, not the root's
   status. `WakeParent` arms a paused parent for `collecting` (healthy, just suspended); a failing
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
   order every tree verb shares) and update an explicit id list, which also yields the
   per-instance outcomes a row count cannot.

## Known gaps

The deferred `pausing → paused` landing is unlogged in the normal case: it happens inside the
owner's write, and reporting it back means RETURNING on the hottest queries — judged a bad trade
(the crash-recovery path does log it). Resume has no info-level trace; attribution needs
`?level=debug`. Migration 022's data mapping is untested (test databases never hold legacy rows).

## Cancel

`cancelling`/`cancelled`, root-only, reusing this document's machinery: the leased/parked split
is pause's (a row a worker is inside can only be ASKED to stop), the deferred landing is pause's
CASE in the lease-releasing writes, and `settleCancelling` has `settlePausing`'s shape. Three
things differ, each because cancel is terminal where pause is reversible:

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
(`settleFailing` clears the wait because there it genuinely ended; a cancel abandons one.)

Two seams. `ClaimInstances`'s status list and the `idx_instances_runnable` partial index are
**one predicate written twice**: `cancelling` must be in both, and a status in one but not the
other is either never scanned (stranding every draining row whose worker died) or index churn. And
`Status.Terminal()`'s SQL copies (`CountActiveSiblings`, `NonTerminalSubtree`) gain a terminal
status by hand.

Reaching work already in flight is the heartbeat's job:
[external-task-queue.md](external-task-queue.md) §Renew is the heartbeat.

## Open

- Step-debugging on top of pause: start paused, then a per-instance tick, one `advance()` per step.
  Trigger: debugging a definition by stepping it (tracked in ROADMAP).

# Pause/resume vs retry: design

Status: **agreed and implemented 2026-07-20.** Code: `db_lifecycle.go`,
the pause-landing CASEs in `queries.sql`, `settlePausing`, migration 022. The
silent-failure invariants live in [internal/db/CLAUDE.md](../internal/db/CLAUDE.md).

## Motivation

`cancel` + a `retry` that accepted failed **or** cancelled roots served two unrelated
situations with one verb: a **failed** process spent its authored `on_error` budget, so
reviving it is an override the definition never authorised (with `force`, one that
skips `only_once` too); a process an operator **stopped** was never owed anything and
should carry on exactly where it was. Because `cancelled` was terminal, the shared
implementation had to be the destructive one — and produced a real bug: cancel+retry
silently converted "wait 30s, then retry" into "retry immediately", both halves
clearing `wake_at`.

## The model

`paused` is **not an outcome** — it means only "does not advance automatically". The
instance keeps `wait_state`, `wake_at`, `retry_count` and context verbatim; timers keep
running. `pause` (root, running → pausing if leased else paused), `resume` (paused rows
anywhere in the subtree → running), `retry` (root, failed only, keeps `force`).
`cancelling`/`cancelled` were **removed, not renamed** — the whole draining machine
went with them; migration 022 maps old rows. There is deliberately no permanent cancel:
every process must have a way back; a terminal stop, if ever wanted, is a new status
beside `failed`, leaving `paused` untouched.

## The decisions

1. **Pause is non-destructive, so resume is a status flip.** Everything that makes
   retry complicated is *absent* from resume: no revive walk, no wait-state
   reconstruction, no `only_once` question, no force. The asymmetries fall out rather
   than being chosen (budget untouched vs deliberately exceeded; `wake_at` preserved vs
   backoff cleared; mechanical vs judgement call). This is why the verbs must never
   re-merge.
2. **`pausing` means *leased*, not not-yet-seen.** Only a row a worker currently holds
   drains; everything parked goes straight to `paused` — load-bearing, because a
   `waiting` row is excluded from claims, so marking it `pausing` would strand it
   forever (the old cancel path dodged this only via a trick pause cannot use).
   `pausing` stays claimable purely for crash recovery (`settlePausing`); the
   interrupted-`only_once` verdict is resolved on that reclaim *before* the pause
   settles — its evidence does not survive the settling write — see
   [only-once-interrupted.md](only-once-interrupted.md); `settlePausing` must not
   regain the question.
3. **A pending pause lands in SQL, not in Go.** A worker mid-task cannot know the pause
   arrived after its claim, so `pausing → paused` is a CASE on the lease-releasing
   writes — guarded in `UpdateInstance` so real outcomes still win, unconditional in
   `UpdateInstanceProgress` (a checkpoint means "still running"). Progress matters
   most: it is also the write that parks on a delay/external — the pause lands there
   or never. `SpawnChildrenAndWait` remaps explicitly, and children inherit the
   settled status so a suspended tree never spawns runnable work.
4. **A failure outranks a pause.** `FailAncestors` includes paused/pausing rows. But
   paused children count as active, so a tree that loses a branch while suspended sits
   at `failing` over paused descendants until resumed — not a bug; resume is how the
   operator unblocks it, which is why `ResumeProcess`'s precondition is on the
   *subtree*, not the root's status. `WakeParent` arms a paused parent for `collecting`
   (healthy, just suspended); a failing one gets `''`.
5. **Timers keep running while paused.** A timer elapsing during a pause is due at
   resume. Freeze-and-rebase was rejected: paused must mean *only* "does not continue
   automatically".
6. **Delivery is not advancement.** A paused instance still accepts signals (rejecting
   would make a pause lose events). Armed → delivered but not advanced
   (`DeliverSignal` leaves status alone); unreached → buffered. Treating
   paused-but-armed as unarmed looks safer and is not: an armed task never re-arms, so
   the buffered result would sit unread forever (caught by a test, not review). The
   external-task queue excludes paused rows — resolve would reject the submission.
7. **Audit asymmetry is deliberate.** Per-instance entries are debug (subtree fan-out);
   only pause gets a root info entry, because only its outcome is deferred
   (`meta.pausing` counts the drainers). Resume is atomic, so a root entry would
   restate the per-instance ones — do not "fix" the asymmetry. Both log after commit;
   both SELECT-FOR-UPDATE + explicit id-list updates (same lock order as
   FailAncestors), which also yields the per-instance outcomes a row count cannot.

## Known gaps

The deferred `pausing → paused` landing is unlogged in the normal case — it happens
inside the owner's write, and reporting it back means RETURNING on the hottest queries
plus `:exec`→`:one` semantics changes at seven call sites; judged a bad trade (the
crash-recovery path does log it). Resume has no info-level trace — attribution needs
`?level=debug`; accepted. Migration 022's data mapping is untested (the runner only
exposes `m.Up()`, so test DBs never hold legacy rows); its index rebuild is covered
indirectly by every claim-path test.

## Coverage & future

Each decision above is pinned by named tests across `pause_retry_test.go`, the
tick-suite pause files, `crash_recovery_test.ts`, `signal_test.ts`, and the stress
suites (pause vs FinishChild/FailAncestors races, SIGKILL chaos, the Postgres fleet).
Pause is also the foundation for step-debugging (start paused + per-instance tick; one
`advance()` is the natural step unit) — tracked in ROADMAP.

## Prior art

Mature engines separate the same three axes: reversible suspension, terminal
cancellation, retry-budget override. Suspension is expensive in an event-sourced
partitioned engine; it is nearly free here, where the scheduler is a claim query over
one table — why genroc has it and some larger engines still do not.

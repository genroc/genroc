# The external-task queue: claim, lease, and an error channel

Status: **Phases 1-3 BUILT** (error channel 2026-08-23; claim/lease/renew/release and
`external.lost` 2026-08-24). Phase 4 (long-poll, the evaluator switchover) is unbuilt. The
`external.timeout` split phase 3 proposed was dropped as unsound — see §`external.lost`. Turning `external` from a wait point
into a queue a worker fleet pulls from, with the claim/lease machinery the engine already
runs against `process_instances` — and the error channel `external` has never had. The
motivating consumer is [`evaluator/`](../evaluator/README.md), which ships as a `fetch`
([script-tasks.md](script-tasks.md)).

## Why move off `fetch`

Not because requests are lost under overload — a failed fetch is a routed error code and
the instance stays durable. Overload produces a worse *code*: the sidecar's own `timeout_ms`
never fires, the engine's deadline does, and `http.timeout` is unknowable, so it is never
retried on `only_once`. That is a signalling bug, fixable inside the push design.

The two real reasons: a fetch holds one of `--max-concurrent` for its whole duration (an
`external` holds none), so a slow sidecar starves unrelated tasks on that worker; and pull
inverts the connection direction, which is the only shape that works for a worker off
loopback — `/eval` is arbitrary code execution with the runner's authority.

## What is missing

1. **No claim.** `ListExternalTasks` is a read. N pollers all execute; the losers learn it
   from `ResolveExternalTask`'s epoch check, after the side effects.
2. **No error channel.** `resolve` takes a result and nothing else, so `on_error`, `retry`
   and backoff are unreachable and the evaluator's 422/500 split has no representation.
   `raises` is refused on a non-child action
   ([validate.go:513](../internal/model/validate.go#L513)), so the shapes cannot even be
   declared.
3. **No visibility timeout.** `wake_at` is the only clock, so a worker that died holding a
   task looks identical to one nobody picked up.
4. **The two leases would alias** — see below.
5. **Pause discards a finished answer.** `ResolveExternalTask` rejects non-`running`, so a
   task claimed and then paused can never be resolved — and on an `only_once` task that is a
   correctness break, not an inconvenience (see §Pause).
6. **No nudge.** Nothing wakes the pump after a resolve, so a resume waits up to `--poll`
   (500ms) — irrelevant for an approval, material for a 50ms script.

## The claim must not reuse the engine's lease columns

`worker_id` / `lease_expires_at` / `lease_epoch` mean *an engine worker is advancing this
instance*. A claim means the opposite: the instance is parked. Four ways aliasing breaks:

- `ResolveExternalTask` refuses a submit under a live lease
  ([db_external.go:83](../internal/db/db_external.go#L83)) — a worker would be locked out of
  its own resolve.
- `ClaimInstances` skips rows with a live lease
  ([db_claim.go:110](../internal/db/db_claim.go#L110)), so a claim delays the
  `external.timeout` the engine owes at `wake_at`.
- `RenewWorkerLeases` renews only `Engine.held`; a claim there is a lease nothing renews.
- `worker_id` is the `ReclaimedExpired` evidence behind `only_once.interrupted`. An external
  stamp forges it.

Separate columns also make §"Two clocks" fall out with no change, which is the payoff.

## The mechanism is the engine's, applied to a different subject

Separate columns, identical semantics. Every rule below is the one
[lease-fencing.md](lease-fencing.md) already argues for `process_instances`, and the reasons
carry over unchanged:

| engine | external | rule |
|---|---|---|
| `ClaimInstances` | `ClaimExternalTasks` | a claim is a **grant**: it stamps the holder and bumps the epoch |
| `lease_expires_at` | `external_lease_expires_at` | expiry alone writes nothing — the row simply becomes claimable again |
| `RenewWorkerLeases` | `RenewExternalClaims` | extends a grant, **never** bumps the epoch, never clears the holder |
| `lease_epoch` | `external_claim_epoch` | the fence bound into every write by the holder |
| `ReclaimedExpired` | `external.lost` | the previous holder's id is the evidence, so nothing may clear it |

**Re-claim, not expiry, is what invalidates a handle.** Nothing runs at expiry; the *next*
claim bumps the epoch and fences the previous holder out. So a worker that overran its lease
and was never taken over still resolves successfully — strictly better than discarding work
that was done, and exactly how the engine treats its own late writes.

**Resolve, fail, renew and release all bind the claim epoch** and refuse on mismatch, under
the same instance row lock that already checks the wait state and `task_epoch`
([db_external.go:60](../internal/db/db_external.go#L60)). That is `requireFenced` /
`ErrLeaseLost` in the API's vocabulary: a conflict, naming re-claim as the cause.

**Same principles, no shared code.** The two claim functions are ~90 near-identical lines
twice, but the predicates and the columns written differ enough that a helper would be
parameterised on nearly everything. The table above is what "the same" means here, and the
tests below are what hold it — not a call graph.

One thing does **not** carry over: `Takeover` / `SkipTakeover`. The engine's cutoff exists
because a worker that has just discovered it was frozen must not steal rows from co-resident
workers about to repair their own leases. An external claimer holds no leases it must reason
about, so its cutoff is plain `now`.

## Design

### Schema — migration 028

    external_worker_id        TEXT
    external_lease_expires_at BIGINT
    external_claim_epoch      BIGINT NOT NULL DEFAULT 0   -- the fence; bumped only by a claim

plus `(external_worker_id, external_lease_expires_at)` on the claimable predicate.

**Not a separate table.** The queue *is* the parked rows, and every field of an entry is
already derived from the row — the token from `task_epoch`, the input from `external_data`.
A second table is a second thing that can disagree ([db/CLAUDE.md](../internal/db/CLAUDE.md)
§"The task epoch").

### The handle

`external_claim_epoch` is a grant, bumped only by a claim, exactly as `lease_epoch` is. It
is needed on top of the token because two workers can claim the same *arming* in sequence —
the first claim expires, the second is granted, and the first worker's
`<instance>.<task_epoch>` is still valid.

So the handle is `<instance>.<task_epoch>.<claim_epoch>`, with the two-part form still
accepted when no claim is live. That keeps the human-approval path (list, hand a token to a
UI, resolve) untouched and makes claiming a property of the consumer, not the task.

### `ClaimExternalTasks`

A mirror of `ClaimInstances`, including the dual-dialect split (Postgres: CTE +
`FOR UPDATE SKIP LOCKED` + `RETURNING`; SQLite: select-then-update in one transaction).

    wait_state = 'external' AND status = 'running'
      AND (external_worker_id IS NULL OR external_lease_expires_at <= ?)
      AND (wake_at IS NULL OR wake_at > ?)

ordered `updated_at ASC` — FIFO by park time, the opposite of the list endpoint's
newest-first, which is a UI affordance. Three things it must not do:

1. **Not touch `task_epoch`** — a claim is not a new occurrence, and bumping it invalidates
   tokens already handed out.
2. **Not touch the engine's lease columns** (above).
3. **Not clear `external_worker_id` on expiry** — that is the evidence `external.lost` reads.

The response carries the list's fields plus the task deadline and the declared `raises`, so
a worker can decline work it cannot finish and knows which codes it may report.

**Addressing is the existing filters** — `(process, version, task)`, the same three
`ListExternalTasks` takes — not a `queue:` name on the action. A definition already names
its work in three ways a worker can subscribe to, and a fourth would be a second address for
the same rows. If subscription-style naming is ever wanted it is one more filter column, not
a different model.

`status = 'running'` excludes paused instances deliberately: no *new* work is handed out for
a suspended tree, even though an answer to work already handed out is always accepted
(§Pause).

### Renew and release

`RenewExternalClaims(workerID, tokens, dur)` mirrors `RenewWorkerLeasesChunk`: chunked,
scoped to an explicit id list intersected with `external_worker_id`, must not bump the claim
epoch (it would fence the worker out of its own resolve) and must not clear the worker id.
`ReleaseExternalTask(token)` is the nack, which is what makes graceful shutdown possible.

**[built] A release bumps the epoch; an expiry does not.** The asymmetry is the point and was
not in the original draft: a lapse is the absence of a live lease, so a worker that overran and
was never taken over still answers, whereas a release is a deliberate hand-back and must void
the releasing worker's own handle at once. Both are pinned, in both directions.

**[built] A signal defers to a live claim rather than being refused by it.** Signal carries no
handle, so it has nothing to fence with — and `DeliverSignal` already had exactly this rule for
the engine's live lease ("don't race it; buffer instead, and the signal is consumed if the task
re-arms"). A claim is the same situation with a different holder, so it got the same treatment
rather than a rule of its own.

### The error channel

**One outcome, two addressing modes — not a third endpoint.** [built, revised] The first cut
added `POST /external-tasks/fail` beside `resolve`, and that was wrong in a way the build made
visible: `/instances/{id}/signal` was left able to report success and not failure, and
`process_signals` had a single `result` column that could not have held the other half anyway
(migration 021 had named it for the success channel on the grounds that "the value delivered to
an external task IS its result"). Meanwhile every layer *below* the API had already unified —
`SetExternalOutcome`, `withExternalOutcome`, a phase 2 reading both slots — because a result and
a failure are one event: the wait is over.

So a submission carries an **outcome**: `{result}` or `{error: {code, message, data?}}`,
discriminated by key presence on `error` so a null result stays an ordinary success (both keys
present is a 400). `resolve` (by token) and `signal` (by instance + task) each take either;
`/external-tasks/fail` is deleted. Migration 027 renames `process_signals.result` → `outcome`
holding that same envelope, which is what lets a failure buffer when the task is not yet armed.
The name `resolve` stays: you resolve the *task*, the way you resolve a ticket, not the way a
promise resolves-versus-rejects.

**Routing happens in phase 2, not the API handler.** `handleCallErrorWith`
([error.go:68](../internal/engine/error.go#L68)) resolves the retry policy and mutates the
instance, which needs the lease. So `fail` does what `resolve` does — store the failure on
the row and un-park — and `runExternal` routes it on the next claim:

- `external_data` gains `error` + `has_error`; `model.CtxExternalError`;
  `SetExternalResult` generalises to `SetExternalOutcome` (still unfenced, same reason).
- Phase 2 checks `_external_error` first and calls `handleCallErrorWith(..., {"data":
  payload})` — the path an unaccepted `fetch` response already takes, so `error.data` means
  the same on both.

**The code namespace is the authored one**: lower_snake_case, no dot, the namespace a
child's `raise` uses and `matchOnError` already serves. Rejected: a reserved `external.*`
family (it would sit in a namespace `errcode` documents as engine-produced) and a single
`external.failed` carrying the real code in `error.data` (it hides the discriminator from
the thing that exists to match on it). The shape is **enforced on submission**
(`model.ValidFaultCode`), which is not decoration: without it a worker could send `http.500`
and be caught by a rule written for the wire, or `external.timeout` and reach the unknowable
set an `only_once` task can never retry.

`raises` becomes legal on an external action. **[built]** Two things came out differently
from the paragraph this replaces:

- **The payload is conformed on submission, not in the engine.** A child's raiser is another
  process that cannot be told, so its mismatch can only degrade to `output.invalid` at
  collect. An external submitter is an HTTP caller holding the connection, so the mismatch is
  a `400` it can act on — and because the task stays parked, answering again with a corrected
  payload is the ordinary path rather than a stranded instance. `resolve` already set this
  precedent for the success channel.
- **`raises` is a CLOSED set on an external task**, unlike on a child. A code outside it is
  refused with a 400 listing what is accepted, and a task declaring no `raises` has no error
  channel. The asymmetry is the point: a child's raisable codes come from its own definition,
  so R5 already catches an `on_error` typo at registration — nothing about a worker is
  knowable until it submits, which makes submission the only place a wrong code can ever be
  caught. Left open, `limit_exceded` would quietly reach whatever catch-all exists. The cost
  is that adding a failure mode to a worker is a definition change; that is what declaring a
  contract buys.
- **`raises: {code: null}` declares a code that carries nothing.** The three declarations are
  `null` (no `data`; submitting one is refused; `error.data` absent), `{}` (opaque) and a
  schema (typed) — so "an error with no payload" has a spelling that is still a declaration,
  which a closed set requires. This reverses the old rule that null is never a declaration,
  and the reversal is sound because that rule's premise is gone: it held that "omitting the
  code already says that", which stops being true once omission means *not submittable*.

  **Null rather than a boolean**, having tried `true` first. `raises[code]` is a schema
  position, genroc has no boolean schemas, and JSON Schema's bare `true` means "any value
  validates" — so `true` would spell "nothing" with the standard symbol for "anything".
  `internal/schema` already refuses the boolean form on the stated principle that *closed is
  expressed by absence rather than an explicit false*; no payload is the absence of a schema.
  It is also the cheaper half: null round-trips through `map[string]*schema.Schema` natively,
  where `true` needed a custom `UnmarshalJSON` **and** a `MarshalJSON` — the latter only
  because nil would otherwise marshal to `null` and fail to load on the read after the write
  that stored it. Both are deleted.

**No worker-supplied `not_reached`.** An authored code is neither `pre.*` nor unknowable, so
the default classification is "potentially reached" — not retryable on `only_once` without
the author asserting otherwise. That flag is a claim about what an error *means*, and the
author owns it. **[built]** This turned out to be enforced at *registration*, not merely at
runtime: `only_once` + a `retry` naming a worker code is refused at `PUT /definitions` unless
the rule carries `not_reached: true` and names exact codes. Stronger than this paragraph
assumed, and it needed no new code — the existing guard does not care that the code is
authored rather than an engine code.

### Pause suspends execution, not delivery

**Resolve and fail accept `running`, `paused` and `pausing`** — the same status set
`signalInstance` already accepts ([handlers_external.go:118](../internal/api/handlers_external.go#L118)),
for the same stated reason: *a pause suspends execution, not delivery; rejecting here would
make a pause lose events.* The live-lease rejection stays exactly as it is (a timeout advance
in flight wins).

Refusing is not merely wasteful, it breaks `only_once`. A worker claims the task, performs
the side effect, a pause lands, and the resolve is refused: the instance stays parked, its
deadline keeps running (`paused` preserves `wake_at`, and a timer that elapses while paused
is due the moment it resumes), and `external.timeout` is unknowable — so `only_once` forbids
the retry and the instance fails terminally for work that actually succeeded.

The write needs no new mechanism. `SetExternalOutcome` stores the result and clears
`wait_state` / `wake_at`; `ClaimInstances` excludes `paused`, so the row simply waits, and
`ResumeProcess`'s plain status flip makes it claimable into phase 2. Clearing `wake_at` also
disarms the deadline, which is the point — an answered wait cannot later time out.

Buffering into `process_signals` instead would be wrong here: `DeliverSignal` buffers because
it addresses a task by id that may not be armed yet, whereas a resolve is addressed by token
to an arming that is live. Buffering would leave the instance parked on a wait already
answered, with the deadline still running — the break above, one step removed.

### Two clocks, one authority

`wake_at` stays authoritative over the claim's visibility timeout: the engine claims at the
deadline regardless of a live claim, raises `external.timeout`, and the worker's later
resolve fails the wait-state check. This needs **no change** — `ClaimInstances` reads only
the engine's own lease columns — so a test has to pin it, because it is what a
"simplification" back onto shared columns would break. Cap a granted lease at the remaining
budget, or at least return the deadline.

### `external.lost` [built 2026-08-24]

An expired claim returns the task to the queue: at-least-once, which is what a queue is for.
On an `only_once` task that is wrong — the work may have happened — so the task is **not handed
out again**, and the arming is marked instead. The engine turns the marker into
`external.lost`: catchable, and in `errcode.Unknowable()`, the analogue of
`only_once.interrupted` for a worker that is not the engine's.

Three mechanics that are not obvious from the outside:

- **The decision is made at claim time, in the API handler, not in the claim's SQL.**
  `only_once` is a property of the definition and the handler already resolves `CurrentTask`
  for every claimed row, so that is where it can be known. The claim is granted and then undone
  (`MarkExternalClaimLost`), which costs a microsecond-long grant nobody sees and keeps the
  claim query free of a definition lookup it cannot do.
- **The marker exists because the engine has to be woken.** A parked row is not runnable, so
  unlike the engine's own `ReclaimedExpired` — derived at the next claim, which the poll loop
  guarantees — nothing would ever look again. `MarkExternalClaimLost` sets `wake_at = now`
  alongside the marker; without it a task with no timeout would sit unclaimable forever with
  nothing saying why.
- **Detection is best-effort-sooner, the guarantee is not.** The marker is only written when
  some worker next tries to claim. If the fleet is idle nothing is reported until the task's own
  deadline, which raises `external.timeout` — also unknowable, so the `only_once` guarantee
  holds either way. What the claim-time path buys is a *sooner and more specific* answer, not
  the safety property.

**The timeout split this section used to propose is NOT built, because it is unsound.** The
argument was that a timeout on a task nobody ever claimed means nothing was reached, so it
could be retried even under `only_once`. It cannot: `GET /external-tasks` publishes `input` to
any caller without claiming — that is what the two-part token is for — so an unclaimed task may
still have been picked up and worked on through the list-then-resolve path. "Never claimed"
therefore does not prove "never reached", and loosening it would break at-most-once for exactly
the callers who do not use the claim API. It would become sound only if the list endpoint
stopped exposing `input`, which is the approval-UI path's whole reason to exist — so this is a
foreclosed direction, not a deferred one.

### API surface

New entries in the action registry ([actions.go](../internal/api/actions.go)):
`claim_external_tasks`, `renew_external_claims`, `release_external_task`,
`fail_external_task`. `resolve_external_task` accepts a three-part token. `ExternalTaskResp`
gains `claimed_by` / `claim_expires_at` so a claimed task does not look idle in `genctl
external-tasks`. New events beside [logs.go:40](../internal/model/logs.go#L40):
`extern_claimed`, `extern_claim_lost`, `extern_failed`. A `wait_ms` long-poll on `claim`
answers the latency note, but second — it changes connection lifetime, not the data model.

## Phasing

1. **Error channel.** ✅ **Built 2026-08-23.** An `error` outcome on
   `/external-tasks/resolve` and `/instances/{id}/signal` (`genctl resolve|signal --code`); `external_data` gains `error`/`has_error` (`SetExternalResult` generalised
   to `SetExternalOutcome`); `model.CtxExternalError` routed by `runExternal` phase 2a
   through `handleCallErrorWith`; `raises` legal on an external action, typed into
   `error.data` by `declaredRaises`; `raises` closed on external, with `null` as the no-payload
   declaration. Carried the §Pause fix with it, since
   `resolve` and `fail` cannot differ on which statuses accept an answer
   (`model.Status.AcceptsExternalOutcome`). Tests: `tests/integration/external_fail_test.ts`.
2. **Claim, lease, renew, release.** Migration 027 and the mirror of `ClaimInstances`.
3. **`only_once` fidelity.** ✅ **Built 2026-08-24.** `external.lost` (unknowable, catchable),
   refusing to re-hand an `only_once` task whose holder lapsed, and the marker + `wake_at` that
   makes the engine report it. The `external.timeout` split was dropped as unsound.
4. **Long-poll, `queue:` naming, evaluator switchover.** Its loop becomes claim(N) →
   evaluate → resolve | fail, renewing while it runs; the existing failure `kind`s
   (`compile_error`, `threw`, `timeout`, `nonserializable`, `exited`) become authored codes.

## Tests that must bite

**Go, `dbtest`, both engines:** two concurrent claimers get disjoint sets; a claim leaves
`worker_id`, `lease_expires_at`, `lease_epoch` and `task_epoch` unchanged; an expired claim
is re-claimable, bumps the claim epoch, and the first worker's resolve is then refused; an
expired claim that nobody re-claimed **still resolves** (expiry writes nothing); a renewal
does not bump the epoch and an unlisted claim expires with the worker id intact.

**Go, engine:** `external.timeout` still fires at `wake_at` with a live claim outstanding.

**JS e2e:** `fail` → `on_error` matches the authored code → each of `goto`/`raise`/`retry`;
`error.data` conformed against `raises`, mismatch → `output.invalid`; a failed code on an
`only_once` task is refused a retry without `not_reached: true`; a lost claim raises
`external.lost` and is catchable.

**JS e2e, pause** — the case that motivated the rule, so it has to fail loudly if the status
set narrows again: claim an `only_once` external task, pause the instance, resolve it, resume
— the instance continues on the submitted result and never reports `external.timeout`. Same
for `fail`. And a paused instance is not offered by `claim` in the first place.

## Decided, and one thing that is not

**Authorization is out of scope**, but `worker_id` is required on claim/renew/release from
the start — the mechanism needs a holder regardless, so checking it later is a tightening
rather than a protocol break. The token stays what it is today: an occurrence
discriminator the queue hands to any caller, not a capability.

Not introduced here, but adjacent and worth a look while in this code: a deadline that
elapses during a long pause fires `external.timeout` the instant the tree resumes. That is
[pause-resume.md](pause-resume.md)'s stated behaviour for timers generally, and it is the
same `only_once` hazard as §Pause with nothing to accept in its place.

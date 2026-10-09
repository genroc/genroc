# The external-task queue: claim, lease, and an error channel

Status: **Built, except the claim long-poll and a pump nudge after a resolve (§Open).**

`external` is a queue a worker fleet pulls from: the engine's claim/lease semantics on columns of
its own, plus an error channel. The motivating consumer is [`eval-node/`](../eval-node/README.md)
([script-tasks.md](script-tasks.md)). Delivery of an answer into a parked instance is
[external-outcome-as-signal.md](external-outcome-as-signal.md).

**There is no listing endpoint.** Polling a list is not a queue: two readers see one row and
nothing leases. Nothing else needs one either — a token is derived from the row
(`<instance>.<task_epoch>`, `model.ExternalToken`), discovery is `GET /instances?phase=external`,
and the work itself (input and its `objects`) travels with the claim, which is what hands it out.

## Why move off `fetch`

Not because requests are lost under overload — a failed fetch is a routed error, and the instance
stays durable (overload only produced a worse *code*, `http.timeout`, unknowable). The reasons: a
fetch holds one of `--max-concurrent` for its whole duration while an `external` holds none, and
pull inverts the connection direction, so a worker can live behind NAT and genroc never dials a
code-execution endpoint.

## The claim must not reuse the engine's lease columns

`worker_id` / `lease_expires_at` / `lease_epoch` mean *an engine worker is advancing this
instance*. A claim means the opposite: the instance is parked. Aliasing them would:

- lock a worker out of its own resolve — `ResolveExternalTask` refuses under a live lease;
- delay the `external.timeout` the engine owes at `wake_at` — `ClaimInstances` skips live leases;
- leave the claim unrenewed — `RenewWorkerLeases` renews only `Engine.held`;
- forge the `ReclaimedExpired` evidence `only_once.interrupted` reads.

## The mechanism is the engine's, applied to a different subject

Separate columns, identical semantics — every rule is one [lease-fencing.md](lease-fencing.md)
argues for `process_instances`:

| engine | external | rule |
|---|---|---|
| `ClaimInstances` | `ClaimExternalTasks` | a claim is a **grant**: it stamps the holder and bumps the epoch |
| `lease_expires_at` | `external_lease_expires_at` | expiry alone writes nothing — the row simply becomes claimable again |
| `RenewWorkerLeases` | `RenewExternalClaims` | extends a grant, **never** bumps the epoch, never clears the holder |
| `lease_epoch` | `external_claim_epoch` | the fence bound into every write by the holder |
| `ReclaimedExpired` | `external.lost` | the previous holder's id is the evidence, so nothing may clear it |

**Re-claim, not expiry, invalidates a handle**, so a worker that overran its lease and was never
taken over still answers successfully. Resolve and release bind the claim epoch and refuse on
mismatch (a conflict naming re-claim), under the row lock that checks the phase and `task_epoch`;
renew matches the whole grant (instance, `task_epoch`, claim epoch) and `external_worker_id`. The two claim paths
share principles, not code: a helper would be parameterised on nearly everything, so the tests
hold them together.

`Takeover` / `SkipTakeover` do not carry over: an external claimer holds no leases it must repair,
so its cutoff is plain `now`.

## Design

### Schema — migration 028

    external_worker_id        TEXT
    external_lease_expires_at BIGINT
    external_claim_epoch      BIGINT NOT NULL DEFAULT 0   -- the fence: claim, release and lost-marking bump it; renewal and expiry never do

A claim belongs to one occurrence (`task_epoch`): the engine's writes keep the first two columns
only while the epoch is unchanged, or a claimed answer leaves its claim blocking, refusing and
mis-reporting the next task as re-claimed.

**Not a separate table.** The queue *is* the parked rows; a second table is a second thing that
can disagree ([db/CLAUDE.md](../internal/db/CLAUDE.md) §"The task epoch").

### The handle

`<instance>.<task_epoch>.<claim_epoch>`. The claim epoch is needed on top of the token because two
workers can claim one *arming* in sequence, and the first worker's `<instance>.<task_epoch>` stays
valid. The two-part form is refused only while a claim is live, so unclaimed answering (`signal`,
or `resolve` with a two-part token) is untouched and claiming is a property of the consumer, not
the task.

### `ClaimExternalTasks`

A mirror of `ClaimInstances`, dialect split included (Postgres `FOR UPDATE SKIP LOCKED`; SQLite
select-then-grant in one transaction):

    phase = 'external' AND status = 'running'
      AND (external_worker_id IS NULL OR external_lease_expires_at <= ?)
      AND (wake_at IS NULL OR wake_at > ?)

FIFO by park time (`updated_at ASC`). It must **not** touch `task_epoch` (every handed-out token
would die), the engine's lease columns, or clear `external_worker_id` on expiry (the evidence
`external.lost` reads). The response carries input and `objects`, the task deadline and
`renew_before_ms`, so a worker can decline work it cannot finish in time. Not `result_schema` or
`raises`: they are the definition's, fixed per version and enforced at resolve, so a worker is
written against them rather than reading them per claim. `status = 'running'` excludes paused and cancelled trees: no new work for a
suspended tree, though an answer to work already out is always accepted (§Pause).

**Addressing is the `(process, version, task)` filters**, not a `queue:` name: a definition already
names its work three ways, and an unfiltered worker claims every parked task on the server,
other fleets' included.

### Renew and release

`RenewExternalClaims` answers per token and renews a grant only on an exact match of instance,
`task_epoch`, claim epoch and `external_worker_id`: matched by worker alone, a stale token (an
earlier arming or grant) read `renewed` while the same worker held the current claim. It must
neither bump the claim epoch (it would fence the worker out of its own answer) nor clear the
worker id. `ReleaseExternalClaim` is the nack — how a queue spells *retryable*, so a runner fault
needs no error code.

**A release bumps the epoch; an expiry does not.** A lapse is the absence of a live lease, so an
overrun worker never taken over still answers; a release is a deliberate hand-back and must void
the releaser's own handle at once.

**A signal defers to a live claim rather than being refused by it.** It carries no handle to fence
with, so it buffers without un-parking — the rule `DeliverSignal` already applies to a live engine
lease (`TestDeliverSignal_DefersToALiveClaim`). The claim's end hands that answer to the engine
(`UnparkAnsweredExternal`): a release un-parks in its own transaction, and since expiry writes
nothing, the next claim un-parks a lapsed row holding an answer instead of granting it. Re-offered,
the task's next answer would lose to the buffered one under FIFO.

### Renew is the heartbeat

Renew extends the visibility timeout and tells the holder whether the work is still wanted. It is
the *only* channel for the second — a worker dials genroc, never the reverse — so renewal is
mandatory, and claim and renew state `renew_before_ms` (a third of the lease). It answers **per
token**:

- `renewed`;
- `lost` — the claim is someone else's: stop, and **do not** release (that would bump the new
  holder's epoch out from under it);
- `cancelled` — still yours and nobody wants it: stop and **must** release, or the row waits out a
  lease nobody serves. A cancelled claim is not renewed, and cancel leaves `external_worker_id`
  set — cleared, the next renewal would answer `lost`, exactly the wrong instruction.

The classifying read shares the renewal's transaction: a row turning cancelled in between would
come back `renewed`. An `on_cancel` hook in the definition was rejected: the statuses are needed
either way, and `on_error` already reaches an external resource if the cancellation code is
catchable.

### The error channel

**One outcome, two addressing modes.** A submission carries `{result}` or `{error: {code, message,
data?}}`; an `error` key makes it a failure, so a null result stays a success, and a result
beside an error is a 400. `resolve` (by token) and `signal` (by instance + task) each take either.
A result and a failure are one event — the wait is over — so `process_signals.outcome` holds
either, which also lets a failure buffer before its task arms.

**Routing happens in phase 2, not the API handler.** `handleCallErrorWith` resolves the retry
policy and writes `retry_count` / `wake_at`, which needs the lease. The failure is routed exactly
like an unaccepted `fetch` response, so `error.data` means the same on both.

**The code namespace is the authored one**: lower_snake_case, no dot (`model.ValidFaultCode`),
the namespace a child's `raise` uses. Without the check a worker could send `http.500` and be
caught by a rule written for the wire, or `external.timeout` and reach the unknowable set. A
reserved `external.*` family was rejected (that namespace means engine-produced), as was one
`external.failed` carrying the real code in `data` (it hides the discriminator from the matcher).

`raises` is legal on an external action, and differs from a child's:

- **The payload is conformed on submission**, a 400 the HTTP caller can act on, with the task left
  parked — a child's raiser cannot be told, so its mismatch degrades to `result.invalid`.
- **`raises` is a CLOSED set.** An undeclared code is refused with the accepted list; a task
  declaring no `raises` has no error channel. A child's codes are checked at registration (R5); a
  worker's are unknowable until it submits, so submission is the only place a typo can be caught.
- **`raises: {code: null}` declares a code that carries nothing** (`data` refused, `error.data`
  absent) beside `{}` (opaque) and a schema (typed). Null, not `true`: `raises[code]` is a schema
  position, genroc has no boolean schemas, and JSON Schema's bare `true` means "anything".

**No worker-supplied `not_reached`.** An authored code is "potentially reached"; asserting
otherwise is the author's claim. `only_once` + a `retry` naming a worker code is refused at
`PUT /definitions` unless the rule carries `not_reached: true` and names exact codes.

### Pause suspends execution, not delivery

**Resolve and signal accept `running`, `paused` and `pausing`**
(`model.Status.AcceptsExternalOutcome`). Refusing would break `only_once`: a worker claims, does
the side effect, a pause lands, the resolve is refused, the deadline keeps running (paused
preserves `wake_at`), and the unknowable `external.timeout` fails the instance terminally for work
that succeeded. The live-lease rejection stays: a timeout advance in flight wins.

The write is the ordinary one: buffer the outcome and `UnparkExternal` (clearing `wake_at`, which
disarms the deadline); `ClaimInstances` excludes `paused`, and resume's status flip makes the row
claimable into phase 2. Buffering *without* un-parking would leave an answered wait parked with
its deadline still running.

### Two clocks, one authority

`wake_at` stays authoritative over the claim's visibility timeout: the engine claims at the
deadline regardless of a live claim, raises `external.timeout`, and the worker's later resolve
fails the phase check. It needs no code — `ClaimInstances` reads only the engine's own lease
columns — so `TestExternalClaim_DoesNotDelayTheEngineTimeout` pins it against a "simplification"
onto shared columns.

### `external.lost`

An expired claim returns the task to the queue — at-least-once. On an `only_once` task the work
may have happened, so the task is **not handed out again**; the engine raises `external.lost`
instead (catchable, in `errcode.Unknowable()`).

- **Decided at claim time, in the API handler**, which already resolves the current task: the
  claim is granted, then undone (`MarkExternalClaimLost`), keeping a definition lookup out of the
  claim's SQL.
- **The marker must wake the engine.** A parked row is not runnable, so `MarkExternalClaimLost`
  sets `wake_at = now` beside `external_lost`; without it a task with no timeout sits forever.
- **Detection is best-effort-sooner, the guarantee is not.** If no worker claims again, the
  deadline raises `external.timeout` — also unknowable — so `only_once` holds either way.

**A never-claimed timeout is not "never reached", so it is not retryable under `only_once`.** An
unclaimed task is still reachable: the instance detail publishes its `external_input`, a two-part
token is formed from the row, and `signal` answers with no handle at all. The unclaimed path is
the approval path's whole purpose, so loosening this would break at-most-once for exactly its
callers.

### API surface

`claim_external_tasks`, `renew_external_claims`, `release_external_task`,
`resolve_external_task` (two- or three-part token) and `signal_instance`
([actions.go](../internal/api/actions.go)), each requiring the `worker` permission. Events
`extern_failed` and `extern_lost`.

## Tests that must bite

**Go, `dbtest`, both engines** (`external_claim_test.go`): concurrent claimers get disjoint sets; a
claim leaves `worker_id`, `lease_expires_at`, `lease_epoch` and `task_epoch` alone; an expired
claim is re-claimable and fences the first worker; one nobody re-claimed **still resolves**;
renewal keeps the epoch and an unlisted claim expires with its worker id intact; a live claim does
not delay the engine's timeout; an answer that deferred to a claim goes to the engine, not a
worker, once the claim is released or lapses, without stranding the rest of the batch.

**JS e2e:** a failure routes to `on_error` by the authored code; `error.data` mismatch is a 400
with the task left parked; an `only_once` retry on a worker code is refused without
`not_reached: true`; a lost claim raises `external.lost`.

**JS e2e, pause** — must fail loudly if the status set narrows: claim an `only_once` task, pause,
resolve, resume — it continues on the submitted result and never reports `external.timeout`; a
paused instance is not offered by `claim` at all.

## Decided

`worker_id` is required on claim and renew (release is fenced by the three-part token). It is
self-declared, not bound to the credential; the endpoints are authorized by the `worker`
permission ([api-auth.md](api-auth.md)). The token is an occurrence discriminator the queue hands
to any caller, not a capability.

## Open

- **Nudge after a resolve.** Nothing wakes the pump, so a resume waits up to `--poll` (500ms) —
  irrelevant for an approval, material for a 50ms script. Build when script latency matters.
- **Claim long-poll.** A `wait_ms` on `claim` is the only thing between a 250ms idle poll and
  immediate pickup, and changes connection lifetime, not the data model — build when pickup
  latency matters.
- **A deadline elapsing during a pause** fires `external.timeout` the instant the tree resumes:
  [pause-resume.md](pause-resume.md)'s rule for timers, and §Pause's `only_once` hazard with
  nothing to accept in its place. Revisit if long pauses over `only_once` externals occur.

# Recovering an interrupted `only_once` task

Status: **proposed, not implemented** (drafted 2026-08-02).

Would touch: `internal/errcode/errcode.go`, `internal/engine/advance.go`
(`prepareAdvance`), `internal/engine/error.go` (a router beside `handleCallError`),
`internal/model/validate.go` (`validateOnError`), `internal/model/wire.go` (the
`only_once` / `on_error` descriptions the editor schema is generated from),
`docs/child-error-handling.md` (the error-code table), and
`tests/integration/crash_recovery_test.ts`.

## The gap

When a worker is interrupted mid-task — it crashed, or froze long enough to lose its
lease — the next worker to claim the instance sees `ReclaimedExpired` and re-runs the
task. That is at-least-once, and it is correct for the default case.

`only_once: true` opts out: the engine refuses to re-run, because the call may already
have happened, and fails the instance with `engine.only_once`. Correct, and terminal —
`engine.*` codes are never routed through `on_error`, so a definition cannot react.

Terminal is the wrong stopping point. The engine genuinely cannot know whether the charge
went through; **the definition's author often can** — by asking the payment provider
whether that idempotency key exists, by looking the order up, by reading back the row the
call was supposed to write. What the author needs is a place to put that question. The
outcome should be *recoverable but never blindly repeatable*: no automatic retry ever, and
a re-run only after the definition has checked and decided.

## The code: `only_once.interrupted`

A new catchable code, in a family of its own named after the declaration that produces it.

```yaml
- id: charge_card
  only_once: true
  action: { type: fetch, url: "https://psp.example/charge" }
  on_error:
    - code: [only_once.interrupted]
      goto: verify_charge
```

The name is deliberately narrow rather than general. Three families were rejected for
concrete reasons, and each is a trap worth keeping written down:

- **Not `pre.*`.** That prefix is not a description, it is a retry-safety *assertion*:
  `Code.IsNotReached` and `patternOnlyMatchesPre` use it to authorise retries on
  `only_once` tasks. A `pre.interrupted` would make `pre.%` rules retry the one error that
  must never be retried automatically.
- **Not `engine.*`.** That family is documented in `errcode.go` as terminal and never
  routed. Making one member catchable turns a clean invariant into an exception list.
- **Not `task.*` or `call.*`.** A general family invites general membership, and this
  event is not general: it exists only because a task declared `only_once`. Naming the
  family after the declaration keeps the set self-limiting — anything else in it would
  also have to be a consequence of that same guard.

So `errcode.go` grows a third section beside "call codes" and "engine-internal codes": a
code the **engine** produces (dotted, like every engine code) that is nonetheless
**catchable** (routed through `on_error`, like a call code).

The message keeps the author's vocabulary rather than the engine's — a definition should
not have to know what a lease is:

> `task "charge_card" is only_once and its previous attempt was interrupted; the engine will not re-run it`

## Where it is raised, and what a pending pause does to it

The guard has two sites: `prepareAdvance` (`advance.go`), on the claim of a **running**
instance, and `settlePausing` (`error.go`), on the crash-recovery claim of a **pausing**
one — a row whose worker died mid-task after an operator asked the tree to stop. Both
carry `only_once.interrupted`; `errcode.EngineOnlyOnce` is retired.

The pausing case looks like it needs special handling and does not. The rule:

> **An interruption is resolved immediately; the pause lands at the next stable boundary.**

The two halves are separable because they have different deadlines. Deciding *what to do
about the interruption* is time-sensitive: `ReclaimedExpired` is derived per claim from
`worker_id` and never persisted, and the write that settles a pause clears that column, so
a decision deferred past this write is a decision made on evidence that no longer exists.
Actually *running* the handler is not time-sensitive at all — asking a payment provider
whether a charge exists answers the same tomorrow.

So `settlePausing` stops asking the `only_once` question and the pausing status is ignored
for the purpose of answering it: the instance goes through the same router the running path
uses, and lands wherever that router says, paused. Concretely:

| routed to | result |
|---|---|
| nothing matched | terminal failure — a failure outranks a pause, which is already the engine's rule (`FailAncestors` deliberately includes `paused`/`pausing` rows) |
| `raise` / `panic` / `goto: end` | terminal likewise; there is nothing left to suspend |
| `goto: <task>` | the instance parks at the handler task, **paused**, and runs it on resume |

The last row needs no new Go branch, which is the good part. A routed `goto` returns the
ordinary running checkpoint, and `UpdateInstance`'s `CASE` turns row-status `pausing` plus
incoming status `running` into `paused` — the existing mechanism, unchanged, doing exactly
what it is for. This is invariant 1 from CLAUDE.md ("a pending pause lands in SQL, not in
Go") applied to a path that used to opt out of it. `$error` survives the wait because it is
persisted in `error_data` like any other context, so the handler reads it on resume.

What remains in `settlePausing` afterwards is only its original job: land the pause. An
interrupted task that is *not* `only_once` still parks and re-runs on resume, unchanged —
at-least-once needs no evidence, so there is nothing to decide before the pause.

Two consequences worth naming. A definition that routes back into the interrupted task
(`goto: $charge_card`) parks paused *on that task* and re-runs it on resume without the
guard firing, which is the author's decision taken at pause time rather than at resume
time — the same sanctioned re-entry described below. And an operator who pauses a tree
containing an interrupted `only_once` task may find it settled as `failed` rather than
`paused`; that is not new (it is today's behaviour) and it is what "a failure outranks a
pause" means.

## What a matching rule may do

Everything `on_error` already offers, minus retries:

- **`retries` is refused, always.** Not "refused unless `not_reached`" — refused. That flag
  is an author's assertion that a *call error* means the request never left, and here there
  was no call error: the attempt vanished without recording anything. Today a rule of
  `{code: ["%"], retries: 3, not_reached: true}` on an `only_once` task passes validation,
  so the runtime refusal is the load-bearing one. Registration additionally rejects
  `retries > 0` on a rule whose pattern **literally** names the code, because an ignored
  `retries` is worse than a refused one (the same argument D7 makes for child tasks).
- **`goto`, `raise`, `panic`, `goto: end`** all behave exactly as they do for a call error.
- **Wildcards and catch-alls may match it.** `%` is the author's risk, consistent with how
  every other code is matched; there is no exactness gate. One consequence to accept
  knowingly: an *already registered* definition with a catch-all `on_error` on an
  `only_once` task changes behaviour under this feature — what used to fail terminally now
  routes to that handler. Definitions are immutable per version, so this is a runtime
  behaviour change for existing rows, not a re-registration.
- **Uncaught is unchanged**: no matching rule means the same terminal failure as today,
  with the same phrase in `error`.

Mechanically it is `handleCallError` minus the retry branch: match, inject
`$error = {task, message, code}`, then route. `$error.task` is what tells the handler
*which* task was interrupted. No `work_started` is emitted for the interrupted task —
the handler task emits its own on the next claim.

## Recovering: verify, then continue

The intended shape, and the reason retries are banned rather than merely discouraged:

```yaml
- id: charge_card
  only_once: true
  action: { type: fetch, url: "https://psp.example/charge", ... }
  on_error:
    - code: [only_once.interrupted]
      goto: verify_charge
  switch: [{ goto: receipt }]

- id: verify_charge                      # ask the system of record, not the engine
  action: { type: fetch, url: "https://psp.example/charges/${ order_id }" }
  switch:
    - case: $: self.exists                # it did happen — carry on
      goto: receipt
    - goto: charge_card                   # it did not — re-run it, deliberately
```

The re-entry on the last line works and is sanctioned: the `only_once` guard is evaluated
**once per claim**, in `prepareAdvance`, against the task the instance was parked on — not
on every entry into a task. A routed `goto` persists and ends the advance, so the handler
runs under a fresh, clean claim where `ReclaimedExpired` is false; a later `goto` back into
`charge_card` executes it as an ordinary first attempt. That is exactly the distinction
being drawn: the engine will never repeat the call on its own, and the definition may, once
it has established that repeating is safe.

**What cannot be expressed:** "assume it succeeded and continue with its output". The
interrupted attempt recorded nothing, so there is no `self` to resume with. A handler that
concludes the call did happen must produce the equivalent value itself — which is what
`verify_charge` above does by reading the charge back, its own output taking the place of
the lost one.

## Compatibility

- Instances that already failed carry the string `engine.only_once` in `error_code`
  forever; dashboards and alerts filtering on it need updating. The prose in `error` still
  contains "only_once", so `crash_recovery_test.ts`'s existing assertion survives.
- No schema change, no migration. `error_code` is a free-form string column.
- Definitions with no rule for the code behave identically to today.

## Testing

- `tests/integration/crash_recovery_test.ts` already has the exact harness: worker 1 takes
  an `only_once` task whose mock hangs forever, gets SIGKILLed mid-request, and worker 2
  reclaims. Extend it with a definition that routes `only_once.interrupted` to a verify
  task and assert the handler ran, the process completed, and the mock's action endpoint
  was hit exactly once — the re-run must be the *handler's* decision, visible as a separate
  request to the verify endpoint.
- A second e2e for the deliberate re-run: verify says "did not happen", the definition
  gotos back, and the action endpoint is hit a second time — proving the guard does not
  re-fire on an authored re-entry.
- The pausing variant, which `pause_retry_test.ts` and the crash suite can be combined
  into: pause the tree, SIGKILL the worker mid-`only_once` task, let the reclaim happen,
  and assert the instance settles at **`paused` parked on the handler task** — not on the
  interrupted one, and not `running`. Then resume and assert the handler runs. The
  uncaught variant of the same setup must settle `failed`, as it does today.
- Go, `internal/model`: registration rejects `retries` on a rule literally naming the code;
  a wildcard rule with `retries` + `not_reached` still registers (the author's risk) but is
  refused at runtime.
- Go, `internal/engine`: uncaught → terminal with the code; matched `goto` → routed with
  `$error` populated; matched rule carrying `retries` → routed, never retried.

## Open questions

- ~~Should the pause path become catchable too?~~ **Yes, and it needs no new state** — see
  above. Two rejected approaches are worth recording, because both look reasonable and
  both are worse: persisting an "interrupted" marker (a column or an `engine_state` field)
  so the verdict survives the pause, and having `settlePausing` park the instance *without*
  clearing `worker_id` so `ReclaimedExpired` re-derives on resume. The second is the
  smaller of the two and nearly works — pause and resume write only `status`, so a stale
  `worker_id` does survive them — but it leaves a live worker's id on a paused row, which
  `RenewWorkerLeases` would renew forever, so it depends on the renewer scoping specced in
  [lease-fencing.md](lease-fencing.md). Resolving the interruption immediately makes both
  unnecessary: nothing has to survive the pause because nothing is deferred.
- **Should `$error` say more than the code?** A handler currently learns *which* task was
  interrupted but nothing about how far it got — there is nothing to learn, since the
  attempt recorded nothing. If action-level metadata (`self.status`, the `fetch` surface
  draft) ever lands, an attempt that got as far as a response would be a different, richer
  case than this one.
- **Does the family stay a family of one?** If a second `only_once.*` code never appears,
  the family is really just a two-word name — which is fine, and cheaper than having
  guessed at a general one.

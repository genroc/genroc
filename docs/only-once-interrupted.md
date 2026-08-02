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

- **`goto`, `raise`, `panic`, `goto: end`** all behave exactly as they do for a call error.
- **Wildcards and catch-alls may match it.** `%` is the author's risk, consistent with how
  every other code is matched; there is no exactness gate. One consequence to accept
  knowingly: an *already registered* definition with a catch-all `on_error` on an
  `only_once` task changes behaviour under this feature — what used to fail terminally now
  routes to that handler. Definitions are immutable per version, so this is a runtime
  behaviour change for existing rows, not a re-registration.
- **Uncaught is unchanged**: no matching rule means the same terminal failure as today,
  with the same phrase in `error`.
- **`retries` is refused** — and not only for this code. See the next section.

Mechanically it is `handleCallError` minus the retry branch: match, inject
`$error = {task, message, code}`, then route. `$error.task` is what tells the handler
*which* task was interrupted. No `work_started` is emitted for the interrupted task —
the handler task emits its own on the next claim.

## The unknowable set: where `not_reached` stops being an option

`only_once.interrupted` is not the only error an `only_once` task can meet where retrying
is a coin flip, and it would be strange to ban the retry here while allowing it on the
error one line over that means the same thing operationally. So the rule is drawn around
the property, not the code:

> **A retry is refused when the definition cannot, even in principle, know whether the
> call took effect** — which is exactly when the request left and no response ever came
> back.

| code | why it is in the set |
|---|---|
| `only_once.interrupted` | the worker vanished mid-task; nothing was recorded anywhere |
| `http.timeout` | connected, and no response ever arrived — the server may have processed it |
| `external.timeout` | an occurrence was armed and nobody resolved it before the deadline (routed through `handleCallError` like any call error, so it is subject to the same rules) |

And what stays outside it, where `not_reached: true` keeps working exactly as documented:

| code | why the author may still assert |
|---|---|
| `pre.*` | the request never left; a retry is safe with no assertion needed at all |
| `http.<status>`, `output.parse`, `output.invalid` | a response **did** arrive, so there is evidence to reason from — "a 422 from this API means nothing was charged" is a real claim about a real API |

That is the line: `not_reached` is an assertion about what an error *means*, and it can
only be made about an error that actually came back. For the set above nothing came back,
so there is nothing to interpret — an author writing `not_reached: true` there is not
asserting domain knowledge, they are guessing.

**Enforcement is at declaration, and again at runtime.** Registration rejects `retries > 0`
on any rule whose pattern can match a member of the set, and `not_reached: true` does not
rescue it. "Can match" is exact rather than approximated: the set is small and fixed, so
each pattern is tested against each member with the same matcher the engine routes with —
`%` and every other wildcard fall out of that for free, with no prefix analysis.

The runtime refusal stays as well, and it is not redundant. Validation runs at
registration; definitions registered before this rule keep their stored `on_error` rules
verbatim and never re-validate, so the engine must still refuse the retry when one of them
routes today.

Wildcards remain legal for **matching**. `{code: ["%"], goto: verify}` is fine and
sometimes exactly right; `{code: ["%"], retries: 3}` is not, because `%` can match
`http.timeout`.

## Recovering: verify, then continue

The intended shape, and the reason retries are banned rather than merely discouraged:

```yaml
- id: charge_card
  only_once: true
  action: { type: fetch, url: "https://psp.example/charge", ... }
  on_error:
    - code: [only_once.interrupted, http.timeout]   # both mean "outcome unknown"
      goto: verify_charge
  switch: [{ goto: receipt }]

- id: verify_charge                      # ask the system of record, not the engine
  action: { type: fetch, url: "https://psp.example/charges/${ order_id }" }
  switch:
    - case: $: self.exists                # it did happen — carry on
      goto: receipt
    - goto: charge_card                   # it did not — re-run it, deliberately
```

The rule lists **both** unknowable codes, and handlers generally should. The two arise
differently — a worker that vanished versus a server that never answered — but they leave
the definition in the same position, with the same question to ask and the same place to
ask it. A handler that catches only `only_once.interrupted` leaves the far more common
`http.timeout` falling through to a plain failure on the very task that can least afford
one.

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

## Implementation note: where the matcher has to live

The declaration check needs to ask "can this pattern match this code", which is
`transport.MatchCode` — and `internal/model` cannot import `internal/transport`, because
transport imports model. Three ways out, in order of preference:

1. **Move the matcher into `errcode`.** It matches codes, `errcode` is the package that
   owns codes, and it has no genroc dependencies, so both model and transport can use it.
   `transport.MatchCode` becomes a one-line forward (it is also used for `accepted_status`
   patterns, which are not codes — a second reason to keep the transport-side name).
2. Do the check in `internal/validation`, which may import both. Rejected: the other
   `only_once` retry rules live in `model/validate.go`, and splitting one rule set across
   two packages is how it drifts.
3. Reimplement a matcher in model. No.

The set itself belongs in `errcode` beside `NotReached`, and the two read as the pair they
are: `NotReached` is the prefix meaning "definitely did not happen", and this is the list
meaning "cannot be known either way". A `Code.IsUnknowable()` method mirrors
`Code.IsNotReached()`.

## Compatibility

- Instances that already failed carry the string `engine.only_once` in `error_code`
  forever; dashboards and alerts filtering on it need updating. The prose in `error` still
  contains "only_once", so `crash_recovery_test.ts`'s existing assertion survives.
- No schema change, no migration. `error_code` is a free-form string column.
- Definitions with no rule for the code behave identically to today.
- **The unknowable set tightens an existing allowance.** Today
  `{code: ["http.%"], retries: 2, not_reached: true}` on an `only_once` task registers
  cleanly, because `not_reached` overrides the pattern check; afterwards it does not,
  since `http.%` can match `http.timeout`. Re-registering such a definition fails with a
  message naming the offending code, and the author narrows the pattern (`http.4%`) or
  drops the retry. Versions already registered keep running — validation happens at
  registration — which is why the runtime refusal is load-bearing rather than belt-and-braces.
- `only_once` is the fallback for APIs that cannot deduplicate. Where the remote accepts an
  idempotency key, sending one from process input (`${ input.order_id }`) makes a retry
  safe outright, and none of this applies. genroc cannot synthesise such a key itself: the
  expression environment is `input`, `outputs`, `self`, `error`, `config` — there is no
  engine-provided run identity (see Open questions).

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
- Go, `internal/model`: a table over the unknowable set × pattern shapes — `%`, `http.%`,
  `only_once.%`, the literal code, and a near-miss that must still register (`http.4%`,
  `pre.%`) — each with and without `not_reached: true`, asserting that the flag never
  rescues a rule that can match the set. Plus the unchanged cases: `pre.%` + retries
  registers, `http.500` + retries + `not_reached` registers.
- Go, `internal/engine`: uncaught → terminal with the code; matched `goto` → routed with
  `$error` populated; a stored rule carrying `retries` (as a pre-rule definition would) →
  routed, never retried — the runtime half of the ban.

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
- **Is `external.timeout` really in the unknowable set?** It is here by the principle
  rather than by demand: an armed occurrence whose deadline passed tells the engine nothing
  about whether the external actor acted, which is the same epistemic state as a response
  that never came. But `only_once` on an external task is a less-trodden combination than
  `only_once` on a fetch, so it is the member of the set most worth a second opinion.
- **Should a stable run identity be exposed to expressions?** The idempotency-key path
  above is the better answer whenever the remote supports it, and today an author can only
  build a key out of their own input. An engine-provided identity (the instance id, or a
  per-task attempt token) would make the key derivable for any process. It is a change to
  the expression environment, so it belongs in its own design rather than smuggled in here.
- **Is there a third outcome between "fail" and "route"?** A definition with no handler for
  an unknowable error currently fails the tree, and recovery is `RetryProcess(force)` — an
  operator verb applied to a dead process. A state meaning "stopped, needs a human, resume
  when decided" would fit this case better than either. It is a lifecycle change, not an
  error-model one, so it is noted rather than proposed.
- **Should `$error` say more than the code?** A handler currently learns *which* task was
  interrupted but nothing about how far it got — there is nothing to learn, since the
  attempt recorded nothing. If action-level metadata (`self.status`, the `fetch` surface
  draft) ever lands, an attempt that got as far as a response would be a different, richer
  case than this one.
- **Does the family stay a family of one?** If a second `only_once.*` code never appears,
  the family is really just a two-word name — which is fine, and cheaper than having
  guessed at a general one.

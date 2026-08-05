# Recovering an interrupted `only_once` task

Status: **implemented 2026-08-02.** Runtime invariants live in
[internal/engine/CLAUDE.md](../internal/engine/CLAUDE.md) and
[internal/model/CLAUDE.md](../internal/model/CLAUDE.md); this file records the design.
Tests: `interrupted_test.go`, `validate_onlyonce_test.go` (the tier matrix),
`idempotent_test.ts`, `crash_recovery_test.ts`.

## The gap

Reclaim-and-re-run is correct at-least-once behaviour, and `only_once: true` opts out —
but the old opt-out failed the instance with a terminal `engine.only_once`. Terminal is
the wrong stopping point: the *engine* cannot know whether the charge went through, but
the author often can — by asking the system of record. The outcome should be
*recoverable but never blindly repeatable*: no automatic retry ever; a re-run only after
the definition has checked and decided.

## The code: `only_once.interrupted`

A catchable code in a family of its own, named after the declaration that produces it.
Rejected families, each a trap worth keeping written down:

- **Not `pre.*`** — that prefix is a retry-safety *assertion* (`IsNotReached`), and a
  `pre.interrupted` would make `pre.%` rules auto-retry the one error that must never be.
- **Not `engine.*`** — documented as never-routed; one catchable member turns a clean
  invariant into an exception list.
- **Not `task.*`/`call.*`** — a general family invites general membership; naming it
  after the declaration keeps the set self-limiting.

So `errcode.go` has a third section: an engine-produced (dotted) code that is
nonetheless catchable. The message keeps the author's vocabulary ("its previous attempt
was interrupted; the engine will not re-run it") — a definition should not know what a
lease is.

## Raise sites, and the pending-pause interaction

Two sites: `prepareAdvance` (claim of a running instance) and the `pausing` branch of
`advance` (crash-recovery claim). The pausing case follows one rule:

> **The interruption is resolved immediately; the pause lands at the next stable
> boundary.**

The two halves have different deadlines: the verdict's evidence (`ReclaimedExpired`,
derived from `worker_id`) does not survive the write that settles a pause, while
*running* the handler answers the same tomorrow. So the instance goes through the normal
router, ignoring the pausing status: unmatched → terminal failure (a failure outranks a
pause); `raise`/`panic`/`goto: end` → terminal; `goto: <task>` → parks at the handler,
**paused**, and runs it on resume — no new Go branch, because the routed checkpoint
writes status `running` and the `UpdateInstance` CASE lands the pause (the "pause lands
in SQL" invariant, applied to a path that used to opt out). `$error` survives the wait
as ordinary persisted context. `settlePausing` keeps only its original job, and a
non-`only_once` interrupted task still just parks and re-runs on resume.

## What a matching rule may do

Everything a call-error rule may: `goto`/`raise`/`panic`/`end`; wildcards and catch-alls
may match it (the author's risk, as with every code — note an already-registered
catch-all on an `only_once` task changes behaviour under this feature, a runtime change
for existing rows); uncaught is the same terminal failure as before. `retries` is
refused — see the unknowable set. Mechanically `handleCallError` minus the retry branch;
`$error.task` names the interrupted task; no `work_started` for it (the handler emits
its own).

## The unknowable set

The retry ban is drawn around the property, not the code: **a retry is refused when the
definition cannot, even in principle, know whether the call took effect** — the request
left and nothing came back. Members: `only_once.interrupted`, `http.timeout`,
`external.timeout` (armed, deadline passed, nothing learned — the member most worth a
second opinion, since `only_once` external tasks are rare). Outside it, `not_reached:
true` keeps working: `pre.*` (never left; safe with no assertion), and any code where a
response *arrived* (`http.<status>`, `output.*`) — there is evidence to assert about.
`not_reached` is an assertion about what an error means, and for the set nothing came
back, so there is nothing to interpret.

**Enforcement: three tiers at declaration, per pattern** (a rule may mix a safe `pre.%`
with a named exception):

1. a pattern that can only match `pre.*` is safe alone;
2. anything else needs `not_reached: true` **and exact codes** — an assertion about a
   wildcard is a hope, not an assertion;
3. an unknowable-set member is refused however named. Checked first, so naming
   `http.timeout` gets "hopeless", not tier-2 advice; and since tier 2 admits only
   literals, tier 3 is exact membership — no wildcard matching in validation.

Alongside: `on_error` and `switch` reject unknown keys naming the right list (`code` vs
`case`) — a silently dropped selector used to turn a rule into a catch-all. Safe over
stored rows because stored definitions are canonical re-marshals. Every rejection names
the offending pattern *and* the fix; the validation matrix asserts both, and runs every
case against a non-`only_once` task where all must pass. The runtime refusal
(`isRetryAllowed`) is not redundant: pre-rule definitions never re-validate.

Wildcards stay legal for **matching**: `{code: ["%"], goto: verify}` is fine;
`{code: ["%"], retry: 3}` is not.

## Recovering: verify, then continue

```yaml
- id: charge_card
  only_once: true
  action: { type: fetch, url: "https://psp.example/charge" }
  on_error:
    - code: [only_once.interrupted, http.timeout]   # both mean "outcome unknown"
      goto: verify_charge
- id: verify_charge
  action: { type: fetch, url: "https://psp.example/charges/${ order_id }" }
  switch:
    - case: $: self.exists
      goto: receipt
    - goto: charge_card        # did not happen — re-run, deliberately
```

Handlers should list **both** unknowable HTTP-path codes: they arise differently but
leave the definition in the same position. The re-entry is sanctioned: the guard runs
once per claim in `prepareAdvance` against the parked task; a routed `goto` ends the
advance, so a later `goto` back executes as an ordinary first attempt — the engine never
repeats on its own, the definition may once it has established safety. Not expressible:
"assume it succeeded and continue with its output" — the lost attempt recorded nothing,
so a handler concluding it happened must produce the value itself (as `verify_charge`
does by reading it back).

## Implementation notes

- The matcher lives in `errcode` (it matches codes; `errcode` owns codes; no
  dependencies) — moved from transport totally, not forwarded. The import cycle that
  forced the question dissolved once tier 2 admitted only literals, so the move stands
  on ownership. `IsUnknowable()` mirrors `IsNotReached()`.
- Compatibility: old rows keep `engine.only_once` in `error_code` forever (dashboards
  update; the prose still says "only_once"). No migration. The set *tightens* an old
  allowance: `{code: ["http.%"], retry, not_reached}` used to register and now fails at
  re-registration with the offending code named; already-registered versions keep
  running, which is why the runtime refusal is load-bearing.
- `only_once` is the fallback for APIs without idempotency keys; where the remote
  accepts one, sending it from input makes retries safe outright. genroc cannot
  synthesise a key — no engine-provided run identity in the expression environment.

## Open questions

- Rejected alternatives for the pause path, recorded because both look reasonable:
  persisting an "interrupted" marker, and parking without clearing `worker_id` (nearly
  works, but leaves a live worker's id for the renewer to renew forever — depends on the
  lease-fencing renewer scoping). Resolving immediately makes both unnecessary.
- Should a stable run identity be exposed to expressions (making idempotency keys
  derivable for any process)? Its own design — a change to the expression environment.
- Is there a third outcome between "fail" and "route" — "stopped, needs a human"? A
  lifecycle change, noted not proposed.
- Should `$error` say more than the code? Only meaningful if action-level response
  metadata lands (fetch-http-surface).
- The family may stay a family of one; that is fine and cheaper than a guessed general
  name.

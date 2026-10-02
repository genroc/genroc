# Recovering an interrupted `only_once` task

Status: **Built.** Runtime invariants live in
[internal/engine/CLAUDE.md](../internal/engine/CLAUDE.md) and
[internal/model/CLAUDE.md](../internal/model/CLAUDE.md).

## The gap

Reclaim-and-re-run is correct at-least-once behaviour, and `only_once: true` opts out. The
engine cannot know whether an interrupted call took effect, but the author often can, by asking
the system of record. So the outcome is *recoverable but never blindly repeatable*: no automatic
retry ever, and a re-run only after the definition has checked and decided.

## The code: `only_once.interrupted`

A catchable code in a family of its own, named after the declaration that produces it. The
rejected families are each a trap:

- **Not `pre.*`** — that prefix is a retry-safety *assertion* (`IsNotReached`); a
  `pre.interrupted` would let `pre.%` rules auto-retry the one error that must never be.
- **Not `engine.*`** — documented as never routed; one catchable member turns an invariant into
  an exception list.
- **Not `task.*`/`call.*`** — a general family invites general membership; naming it after the
  declaration keeps the set self-limiting.

So `errcode` has a third section: an engine-produced (dotted) code that is catchable. The
message keeps the author's vocabulary ("its previous attempt was interrupted; the engine will
not re-run it") — a definition should not know what a lease is.

## Raise sites, and the pending-pause interaction

Two sites: `prepareAdvance` (claim of a running instance) and the `pausing` branch of `advance`
(crash-recovery claim). The pausing case follows one rule:

> **The interruption is resolved immediately; the pause lands at the next stable boundary.**

The verdict's evidence (`ReclaimedExpired`, derived from `worker_id`) does not survive the write
that settles a pause, while *running* the handler answers the same tomorrow. So the instance goes
through the normal router, ignoring the pausing status: unmatched → terminal failure (a failure
outranks a pause); `raise`/`panic`/`goto: end` → terminal; `goto: <task>` → parks at the
handler, **paused** — the routed checkpoint writes `running` and the `UpdateInstance` CASE lands
the pause. `last_error` survives the wait as ordinary context. `settlePausing` must not regain an
`only_once` branch. Rejected: persisting an "interrupted" marker, and parking without clearing
`worker_id` (a live-looking id the renewer would renew forever) — resolving immediately needs
neither.

## What a matching rule may do

Everything a call-error rule may — `goto`/`raise`/`panic`/`end`, and wildcards and catch-alls
match it — except `retry` (the unknowable set). It is routed by the ordinary `handleCallError`,
whose `isRetryAllowed` refuses the retry; uncaught, it is a terminal failure. `error.task` names
the interrupted task, and no `work_started` is audited for it.

## The unknowable set

The retry ban is drawn around the property, not the code: **a retry is refused when the
definition cannot, even in principle, know whether the call took effect** — the request left and
nothing came back. Members (`errcode.Unknowable()`):

- `only_once.interrupted`;
- `http.timeout`;
- `http.disconnected` — the bytes went out and the connection broke before a response, which at
  the client is indistinguishable from a remote that acted and died answering;
- `external.timeout` — armed, deadline passed, nothing learned;
- `external.lost` — a worker held the task and its claim expired without an answer
  (external-task-queue.md §`external.lost`).

Outside it `not_reached: true` works: `pre.*` never left, so is safe with no assertion, and any
code where a response *arrived* (`http.<status>`, `result.*`) has evidence to assert about.
`not_reached` asserts what an error means; for the set nothing came back.

**Enforcement: three tiers at declaration, per pattern** (a rule may mix a safe `pre.%` with a
named exception):

1. a pattern that can only match `pre.*` is safe alone;
2. anything else needs `not_reached: true` **and exact codes** — an assertion about a wildcard is
   a hope;
3. an unknowable member is refused however it is named. Checked first, so naming `http.timeout`
   gets "never retryable", not tier-2 advice; and since tier 2 admits only literals, tier 3 is
   exact membership.

Every rejection names the offending pattern and the fix, and the validation matrix runs every
case against a non-`only_once` task, where all must pass. Wildcards stay legal for **matching**
(`{code: ["%"], goto: verify}`), never with `retry`. **The runtime refusal (`isRetryAllowed`) is
not redundant**: definitions stored before the rule never re-validate.

## Recovering: verify, then continue

A handler should catch all three unknowable HTTP-path codes (`only_once.interrupted`,
`http.timeout`, `http.disconnected`): they arise differently but leave the definition in the same
position. It may route back into the task after verifying: the guard runs once per claim, in
`prepareAdvance`, against the parked task, and a routed `goto` ends the advance, so a later `goto`
back executes as an ordinary first attempt. "Assume it succeeded and continue with its output" is
not expressible — the lost attempt recorded nothing, so a handler concluding it happened must
produce the value itself (by reading it back).

## Implementation notes

- The matcher lives in `errcode`, which owns codes and has no dependencies; `IsUnknowable()`
  mirrors `IsNotReached()`.
- `only_once` is the fallback for APIs without idempotency keys. Where the remote accepts one,
  sending it from input makes retries safe outright; genroc cannot synthesise one, having no run
  identity in the expression environment.

## Open

- Should a stable run identity be exposed to expressions (making idempotency keys
  derivable for any process)? Its own design — a change to the expression environment.
- Is there a third outcome between "fail" and "route" — "stopped, needs a human"? A
  lifecycle change, noted not proposed.
- A failed fetch's `error` carries no headers.

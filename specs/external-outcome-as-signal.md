# An external outcome is a buffered signal

**BUILT 2026-08-24.** Follows [external-task-queue.md](external-task-queue.md), which
unified a result and a failure into one `ExternalOutcome`, and
[object-store.md](object-store.md), whose uniformity this is the last exception to.

## The target

An outcome reaches a parked instance one way: it is appended to `process_signals`, and the
instance is made claimable. Nothing writes an outcome onto the instance row.

## What is there now

Both API paths — `ResolveExternalTask` and `DeliverSignal` — carry **two** delivery mechanisms and
choose between them at runtime:

```go
armed := waitState == external && currentTask == taskID
liveLeased := <a worker lease or an external claim is still live>
if armed && !liveLeased {  withExternalOutcome(externalData, outcome)  // onto the row
} else {                   InsertSignal(...)                          // into the buffer
}
```

Three costs, and the third is the one that is not obvious:

1. **Two mechanisms must agree**, under a condition that is itself subtle (parked at *this* task,
   no live worker lease, no live external claim, paused counts as armed). Both branches then
   converge on `runExternal` phase 2 — but by different routes, one of which is a column write.
2. **It is the store's last asymmetry.** The outcome lives inside `external_data`, so its
   references are rooted at `_external.result` while the context key is `_external_result`. That
   is the only place in the store where an `objects` path does not address the context, and it
   forces an ordering constraint nothing declares: `decodeState` must place references *before*
   it lifts the outcomes onto their own keys.
3. **The outcome is written UNCUT and UNDECLARED.** `SetExternalOutcome` holds only the instance
   row lock and has no reference set to reconcile, so `withExternalKeys` writes the value inline
   whatever its size — no cut, no `objects` entry, no claim. A large submitted result sits on the
   row until some later full write tidies it. Routed through the buffer instead, the engine
   consumes it under lease and writes it through the ordinary `encodeState`: cut, declared,
   claimed, like every other value.

## Design

**The APIs enqueue and un-park.** Validation, the token's task epoch, and the claim binding are
unchanged. Then: `InsertSignal`, and — if the instance is armed at this task — clear `wait_state`
and `wake_at` so the row becomes claimable. One transaction under the instance row lock, as now.
The `armed && !liveLeased` condition survives, but it now decides only *whether to un-park*, not
*where the outcome goes*.

**Phase 2 reads the buffer.** `runExternal` stops reading `_external_result` / `_external_error`
from the context and reads the oldest buffered signal for `(instance, task)` instead. The POP
travels back in `advanceOutcome`, and `persist` applies it in the same transaction as everything
else — advance decides, persist writes.

**[built] Phase 2 runs BEFORE the arm, which was worth more than the design expected.** An answer
that arrived before the task did is consumed on arrival at the task, in ONE advance — the
instance never parks and no second claim is needed. The old shape consumed it inside the arm and
yielded, so the push/early case cost a claim cycle; that cost is now gone rather than merely not
added.

It also narrows what the arm's non-parking branch is *for*: not the ordinary early-signal case,
but purely the race where a delivery lands between phase 2's check and the park write.

**The arm stops consuming.** `ArmExternalOrConsumeSignal` currently pops a buffered signal, writes
it to the row and yields the lease. It becomes a narrower decision — *park only if the buffer is
empty* — still as one read-modify-write under the row lock, because that atomicity is what stops
the race it exists for: a signal that lands between "the buffer looked empty" and "we parked"
would find the row unparked, buffer without un-parking, and leave the instance asleep until its
timeout.

### The one thing to get wrong

**Do not implement phase 2 as consume-then-yield.** Today a signal consumed at arm time lands on
the row as a checkpoint and phase 2 runs on the *next* claim. That is free there — the arm was a
claim that was happening anyway. Doing the same after an un-park would add a full poll interval
(500 ms by default) to **every** external task, which for the evaluator is every script task.

Reading in `advance` and popping in `persist` costs nothing and is ordinary work: the row is
already un-parked and leased by us, so there is no park/consume decision to arbitrate. It is
specifically **not** the keep-the-lease special case
[internal/engine/CLAUDE.md](../internal/engine/CLAUDE.md) says not to reintroduce — that one is
about the arm, where the database arbitrates park-xor-consume under the row lock.

## What goes

`withExternalOutcome`, `withExternalSlot`, `SetExternalOutcome`; the `_external_result` and
`_external_error` context keys; the `result` / `has_result` / `error` / `has_error` keys in the
column; the arm's pop-and-write branch; and `decodeState`'s lift-after-place ordering.
`external_data` becomes the parked bookkeeping and nothing else. `withExternalKeys` stays for the
`lost` marker.

## What must not break

- **The token binds an arming, not an instance.** `ResolveExternalTask` compares the submitted
  `task_epoch` against the row under the same lock that checks the wait state. Buffering must not
  loosen that: a signal for a stale epoch is refused, not queued, or a re-armed task consumes an
  answer to the previous occurrence.
- **`only_once`.** An outcome that is buffered but never consumed must not become a second
  execution. The instance is un-parked, so the next claim reaches phase 2 and pops; if it crashes
  first, the signal is still buffered and the next claim pops it.
- **A paused instance still accepts delivery.** It buffers and stays unclaimable, which is what it
  does today — a pause suspends execution, not delivery.
- **FIFO replaces the error-first read.** Phase 2a reads the failure before the result today, and
  the justification is that the two column writes are mutually exclusive under the row lock. With
  one queue that arbitration is gone: outcomes are consumed in arrival order. That is a behaviour
  change where both a failure and a result were submitted for one arming, and it is the better
  answer — but it needs saying out loud, not discovering.
- **One outcome per arming is consumed per advance.** Extra buffered signals stay for a re-arm,
  exactly as they do now.

## What shipped, against what was written

Two things came out better than the design said, and one worse:

- **Better:** the pre-buffered case costs one advance, not two (above).
- **Better:** `withExternalSlot` / `withExternalOutcome` are gone entirely rather than shrunk, and
  with them the `result` / `has_result` / `error` / `has_error` keys, the `_external_result` and
  `_external_error` context keys, `SetExternalOutcome`, and `decodeState`'s lift-after-place
  ordering. `external_data` now holds one context key, so its `objects` paths address the context
  like every other slot's — the store has no non-uniform corner left.
- **Worse:** a failure to READ the buffer during an advance has nowhere clean to go. `advance`
  returns no error, so it fails the instance with `engine.spawn` (whose remit already covered
  "arming an external task"), which is terminal — a transient database blip would kill the
  instance rather than retrying. Recorded rather than fixed: the honest fix is a way for an
  advance to report a transient failure, which is a bigger change than this one.

## Tests that must bite

- A large submitted result is CUT: after the resolve is consumed, the value is in `objects` with a
  claim, not inline on the row — `TestResolveExternalTask_LargeOutcomeIsCutWhenConsumed`, which is
  the hole the change exists to close and which nothing covered before.
- An outcome submitted to an armed task and one submitted to an unarmed task take the same path
  and produce the same context.
- A resolve on an armed task costs **one** claim cycle, not two. `tests/tick/external_test.ts`
  already asserted this ("one tick after a resolve reaches `completed`") and was verified to fail
  against a consume-then-yield implementation — which otherwise passes every correctness test and
  silently doubles external latency.
- A signal arriving concurrently with the arm is neither lost nor consumed twice — the existing
  arm race test, which must keep passing against the narrowed arm.
- A stale `task_epoch` is refused rather than buffered.

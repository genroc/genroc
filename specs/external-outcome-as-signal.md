# An external outcome is a buffered signal

**Built.** Delivery half of [external-task-queue.md](external-task-queue.md), which owns the
claim, the handle and the outcome's shape.

## The target

An outcome reaches a parked instance one way: it is appended to `process_signals`, and the
instance is made claimable. Nothing writes an outcome onto the instance row.

The reason is the store: only the engine's encode, under lease, can cut a value, declare it in
`objects` and claim it ([object-store.md](object-store.md)). An API handler writing the outcome
onto the row holds only the row lock and has no reference set to reconcile, so it would store the
value uncut and undeclared, whatever its size.

## Design

**The APIs enqueue and un-park**, in one transaction under the instance row lock.
`DeliverSignal` (by instance + task) inserts the signal and un-parks — clears `phase` and
`wake_at`, `UnparkExternal` — only if the instance is parked at this task with no live engine
lease or external claim; that condition decides *whether to un-park*, never where the outcome
goes. `ResolveExternalTask` addresses a live arming by token, so it refuses rather than defers
and always un-parks. An answer that deferred to a claim is un-parked when the claim ends
([external-task-queue.md](external-task-queue.md) §Renew and release).

**Phase 2 runs before the arm.** `runExternal` peeks the oldest signal for `(instance, task)`,
acts on it in the same advance, and hands its id back on `inst.ConsumedSignalID` so `persist`
deletes it in the same transaction as the state it produced. An answer that arrived before the
task did is consumed on arrival, in one advance.

**The arm consumes nothing.** `ArmExternalUnlessSignalled` parks only if the buffer is empty,
read-modify-write under the row lock; otherwise it writes an ordinary checkpoint and the next
claim's phase 2 pops. The atomicity is the point: a signal landing between "the buffer looked
empty" and the park would find the row unparked, buffer without un-parking, and sleep until the
timeout.

### The one thing to get wrong

**Do not implement phase 2 as consume-then-yield.** A checkpoint after an un-park adds a full poll
interval to **every** external task — for the evaluator, every script task — and passes every
correctness test. `tests/tick/external_test.ts` catches it by asserting one tick after a resolve
reaches `completed`. This is not the keep-the-lease case
[internal/engine/CLAUDE.md](../internal/engine/CLAUDE.md) forbids; that one is about the arm.

## What must not break

- **The token binds an arming, not an instance.** A stale `task_epoch` is refused under the row
  lock, never queued, or a re-armed task consumes an answer to the previous occurrence.
- **`only_once`.** A buffered, unconsumed outcome is not a second execution: if the engine crashes
  before popping, the signal is still buffered and the next claim pops it.
- **A paused instance still accepts delivery**: it buffers and stays unclaimable.
- **FIFO.** Outcomes are consumed in arrival order, one per advance; extras stay for a re-arm. A
  failure and a result submitted for one arming resolve as whichever came first.
- **A buffer read that fails** retries (`retryRead`, 3 attempts 50 ms apart) and then fails the
  instance with `engine.spawn`, which is terminal (§Open).

## Tests that must bite

- `TestResolveExternalTask_LargeOutcomeIsCutWhenConsumed`: a large submitted result ends up in
  `objects` with a claim, not inline.
- An outcome to an armed task and one to an unarmed task produce the same context
  (`TestSignals_*`).
- One claim cycle per resolve (`tests/tick/external_test.ts`, above).
- A signal racing the arm is neither lost nor consumed twice.
- A stale `task_epoch` is refused rather than buffered.

## Open

- **An advance cannot report a transient failure**, so a buffer read that outlasts `retryRead` is
  terminal. Build that channel when such failures show up in practice; it is bigger than this design.

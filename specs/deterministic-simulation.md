# Deterministic simulation: one seed, one interleaving, one verdict

## 0. Status

**Unbuilt.** The seams exist: `advance()` is a step function persisted in one transaction, `--poll 0`
removes the pump's ticker, `dbgen.DBTX` is an interface (already decorated by `pgRewriter`),
`engine.WithWorkerID` makes two engines in one process distinct, and `internal/archtest` bounds
package-level state.

## 1. What it adds

The counting oracle already exists: `only_once` is asserted against services that count their own
invocations ([crash_recovery_test.ts](../tests/integration/crash_recovery_test.ts),
[transport_disconnect_test.ts](../tests/integration/transport_disconnect_test.ts),
[interrupted_test.go](../internal/engine/interrupted_test.go)). What those cannot do is make the
crash points and interleavings exhaustive: SIGKILL lands where the OS puts it, and each lease race
costs real processes and real seconds, so the suites test a handful of orderings.

## 2. Tier 1 — simulated time, injected faults, real goroutines

- An injected `Clock` (replacing the tickers, `db.Now()` and the fetch timeout), a `Sender`
  (replacing transport's package-level `client`) and an RNG (replacing backoff's `math/rand/v2`).
- A fault-injecting `DBTX`, composed like `pgRewriter`: fail before commit, fail after commit (the
  lost reply), connection death mid-transaction.
- Crash = discard the process image and reopen the same database.

## 3. The baton

The races that change outcomes are over database state, not memory (memory races are `-race`'s
job). Each actor — worker step, renewer, flusher, API caller, service — must hold a baton to start
a transaction or an external call; a single-threaded scheduler hands it out, and its decision list
is the seed. **Schedule at transaction boundaries only, never per statement**: handed the baton
while another actor holds a row lock, an actor blocks in the driver still holding it. This gives
replayable cross-worker interleaving without tier 2. SQLite serialises writes, so it exercises the
fence but not `SKIP LOCKED`; that needs Postgres.

## 4. Recipes

| race | schedule | oracle |
|---|---|---|
| claim vs claim | `A.claim, B.claim` on one row | one winner; epoch bumped once |
| the fence | `A.claim → A.action → B.claim(takeover) → A.persist` | A refused (`ErrLeaseLost`), no regression |
| frozen self-reclaim | freeze A's clock between claim and persist | needs per-worker clocks (§6) |
| signal vs arm | signal before vs after `ArmExternalOrConsumeSignal` | consumed exactly once |
| spawn atomicity | child completion between the inserts and the park | unreachable: one transaction |
| pause vs `FinishChild` | pause between a child's persist and `WakeParent` | a stress suite today; here 3 events |
| lost reply | service succeeds, reply dropped, worker retries | `interrupted`; count stays 1 |

The last needs scheduling and fault injection together — the interesting cases are products.
Search with PCT, enumerate small configurations exhaustively, and shrink the decision list.

## 5. Oracles

`lease_epoch` never decreases and no two workers hold one instance at one epoch; an `only_once`
task executes at most once, asked of the service, not the DB; a parent un-parks exactly when its
last child settles; child result count matches spawn count, `child_list` in `_spawn_index` order;
a terminal instance never transitions; everything quiesces; a recovered state is one reachable
without the crash. **Not sound: audit-trail ordering** — log rows are buffered and a crash drops
them by design, so log-based checks belong only in crash-free runs.

## 6. Decide first

- **The per-worker clock.** `db.clockOffset` is global, and the lease gate exists for a skew
  between one worker and the DB, which a global offset cannot express. `AdvanceClock` also only
  moves forward, and `api` calls `db.Now()` too — whether the clock rides on `*db.DB`, a context or
  an argument is the one decision the sim cannot route around.
- **The fetch timeout as an event.** Production keeps `context.WithTimeout`; the sim races
  `fetchReturns` against `fetchTimesOut`. Two implementations of one rule need a test that they agree.
- **What a crash drops.** The `Engine`, `defCache` and the log buffer together — dropping only the
  `Engine` leaves `defCache` warm and the run quietly unfaithful — plus resetting package state
  (the archtest allow-list is that inventory).
- **For tier 2,** a `goleak` check or a lint banning new `go` statements, or it decays silently.

## 7. Deferred and rejected

- **Tier 2, the event loop** (one goroutine, a queue of events, an engine `Step()`): deferred. Its
  remaining prize is intra-worker timing. Reopen when tier 1 finds failures whose schedules
  reproduce but whose outcomes do not.
- **A storage-fault VFS** (torn writes, a lying `fsync`, via pure-Go `modernc.org/sqlite`): its
  reopen trigger, durability levels, has landed; it stays rejected. The `DBTX` decorator covers the
  failures genroc reasons about, the VFS would cover SQLite only, and the driver swap has its own
  compatibility surface.

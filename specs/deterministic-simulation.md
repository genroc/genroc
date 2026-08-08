# Deterministic simulation: one seed, one interleaving, one verdict

## 0. Status

**Proposal.** The simulator is not built. Some of its prerequisites are, because writing
this doc surfaced them as ordinary defects rather than sim-only work:

- **`schema.pendingNodes` is gone** (2026-08-08). A process-global map with a mutex,
  routing solver sentinels back to their `Solver` because `deref` had no solver parameter.
  The routing moved onto `node.pending`; the map, the mutex and a permanent leak went with
  it. It was the last *load-bearing* shared cell outside the two below.
- **`engine.WithWorkerID`** (2026-08-08) — §6a, which was a blocker, is now a seam.
- **The pruner goroutine is gone**, folded into the pump's loop as a due-time check.
- **[internal/archtest](../internal/archtest)** now fails the build on new package-level
  mutable state, with an allow-list that must name the owner it could not have. That
  list doubles as the process-image inventory a crash has to reset (§8c).

Substrate that already existed and was not written for this — that it lines up is the
reason the idea is cheap enough to record:

- `advance()` is already a step function ([internal/engine/advance.go](../internal/engine/advance.go)):
  read the row, take at most one external action, return an `advanceOutcome` that
  `persist` writes in one transaction.
- A virtual clock exists — `clockOffset` / `db.Now()` /
  `AdvanceClock` ([internal/db/db.go](../internal/db/db.go)), driven by `POST /tick`.
- Manual-tick mode (`--poll 0`) already removes the pump's ticker from the loop, and
  `tests/tick/` is thirteen suites written against it.
- `dbgen.DBTX` is an interface and is already decorated once, by
  [pgRewriter](../internal/db/pg_rewriter.go) — the fault-injection seam is cut.

This doc argues for two tiers, recommends building the first, records how far the first
now reaches (§5 — further than the original draft claimed), and rejects the third.

## 1. What the current suites cannot reach

`tests/integration/` and `tests/tick/` drive a real server over HTTP; `tests/stress/`
adds SIGKILL, a Postgres fleet, and lease pressure. Between them they cover the shapes
somebody thought to write down. What they structurally cannot do:

- **Crash between two writes and check the result.** SIGKILL lands wherever the OS puts
  it. The interesting crash points — after the action left, before the persist; after the
  persist, before the parent wakes — are reachable only by luck.
- **Observe the outside world's truth.** `only_once` promises a task executes at most
  once. No test asserts that today, because the assertion needs a service that *counts*
  its own invocations per `(instance, task)` and is believed. This is the single biggest
  gain and it is available at tier 1.
- **Run a thousand worker interleavings in a second.** Lease takeover, self-reclaim, the
  frozen-worker grace ([lease-fencing.md](lease-fencing.md)) — each currently costs a real
  process, a real Postgres, and real seconds, so the suites test a handful of orderings.
- **Reproduce.** A stress failure today is a log and a shrug.

## 2. The non-determinism inventory

| source | where | state |
|---|---|---|
| DB clock | `db.Now()`, 13 call sites in engine + api | already virtual, forward-only, **process-global** (§6b) |
| pump / renewer tickers | [engine.go](../internal/engine/engine.go) | real `time.NewTicker` |
| log-flush ticker | [db_logs.go:113](../internal/db/db_logs.go#L113) | real `time.NewTicker`, 5ms — a latency drain, not a janitor (§4a) |
| fetch timeout | [action.go:30](../internal/engine/action.go#L30) | **deliberately real** — see §4c |
| retry jitter | [backoff.go](../internal/engine/backoff.go), one global `math/rand/v2` site | seed it |
| instance ids | `crypto/rand` + wall clock, via UUIDv7 ([internal/idgen](../internal/idgen)) | needs a seeded source that keeps v7 ordering (§7c) |
| HTTP | package-level `client` in [transport.go](../internal/transport/transport.go) | no seam |
| goroutines | 7 in `internal/`, 5 in `cmd/genroc` — four of them matter (§4a) | the difference between the tiers, but see §5 |
| package-level mutable state | `template.cache`, `db.clockOffset`, two `sync.Once` pairs in `api` | bounded and enforced by `internal/archtest`; the reset problem is §8c |
| storage faults | real SQLite / Postgres | §4d |

Everything above the goroutine row is a day's work each at most.

## 3. Tier 1 — simulated time, injected faults, real concurrency

An in-process harness that drives the engine through `Tick` with:

- an injected `Clock` (§7a) replacing the tickers and the fetch timeout,
- a simulated `Sender` (§7b) replacing `transport.Send`,
- seeded rand and ids (§7c),
- a fault-injecting `DBTX` decorator — error before the write, error after the write
  succeeded (the lost-reply case), connection death mid-transaction,
- crash modelled as *discard the process image and reopen the file* (§8c).

Goroutine interleavings still vary, so on its own this is **randomized fault testing with
a fast clock**, not replay — but §5 shows the gap is narrower than that sounds, because
the interleavings that decide outcomes are all mediated by the database.

The oracles (§9), the fake service (§8a), and the workload generator (§10) are all built
here and all transfer to tier 2 unchanged. That is the argument for this ordering: the
parts that take design thought are tier-independent, and the part that takes refactoring
is the part that might turn out not to be worth it.

## 4. Tier 2 — the event loop

One goroutine holding a virtual clock, a priority queue of pending events, and an RNG that
chooses which *runnable* event fires next. **That choice is the interleaving**: seed it and
a run replays exactly, including intra-worker timing that §5's baton cannot reach.

### 4a. What has to become an event

| today | event |
|---|---|
| the per-instance fan-out — `dispatch` ([engine.go:372](../internal/engine/engine.go#L372)) for the pump, [engine.go:457](../internal/engine/engine.go#L457) for `Tick` | one `stepInstance(id)` per claimed row |
| `leaseRenewer` | `renewLeases(worker)` at its interval |
| the pump's prune due-check | `prune(worker)` — already inline, so already an event in all but name |
| `db` log flusher ([db_logs.go:113](../internal/db/db_logs.go#L113)) | `flushLogs()` |
| the in-flight `transport.Send` | `fetchReturns(instance, task)` at a chosen virtual instant |

The engine gains a `Step()` that claims, advances and persists **one** instance with no
goroutine of its own. This is a mechanical extraction rather than a redesign, precisely
because `advance()` already has the shape; what is being removed is the fan-out around it,
not any logic inside it. The sim build needs a lint or a `goleak` assertion forbidding new
`go` statements, or the property decays silently the first time someone adds one.

The flusher is the awkward one, and it is awkward in production terms rather than sim
terms: it ticks every **5ms** because it is what keeps the audit trail near-live while
still batching inserts, and it belongs to `*db.DB`, which the API also appends through. It
cannot be folded into an engine loop — that would make the db layer's batching depend on
an engine existing. In the sim it becomes an event like the rest; in production it stays a
goroutine.

### 4b. What multiple workers still buys

The fence, the epoch bump, the grace window, `SkipTakeover` — all of
[lease-fencing.md](lease-fencing.md) — need two workers racing on one row, and that is what
the current suites reach least. §5 argues most of those races are reachable *without* the
event-loop refactor, which narrows tier 2's remaining prize to intra-worker timing: the
renewer firing between a claim and its dispatch, the flusher interleaving with a read, a
timer landing mid-persist. Real, but a smaller prize than the original draft claimed.

### 4c. The fetch timeout stops being a context

[action.go:30](../internal/engine/action.go#L30) records why the action timeout is applied
as a *duration* via `WithTimeout` rather than a deadline instant: it was read off
`db.Now()` while `context` compares against real `time.Now()`, and subtraction cancels the
offset where an instant would keep it. Under a simulated transport the reasoning inverts —
the call never blocks on real time at all, the sim decides when (or whether) it returns, so
`context` can no longer be what enforces the timeout. The timeout becomes a competing event:
whichever of `fetchReturns` and `fetchTimesOut` the queue reaches first wins, and the loser
is cancelled. **The production path must keep using the context**, so this is a second
implementation of the same rule, and the two agreeing is itself worth a test.

### 4d. Storage-fault accuracy — rejected

Torn writes, reordered writes, a partial WAL, and an `fsync` that lies would mean replacing
cgo `mattn/go-sqlite3` with pure-Go `modernc.org/sqlite` so a Go VFS can be installed under
it. That is a driver swap with its own compatibility surface, it covers only SQLite (leaving
Postgres to the existing fleet suite), and it buys failure modes the `DBTX` decorator already
approximates: the write failed, the write succeeded but the reply was lost, the connection
died mid-transaction. Not worth it. Reopen if
[durability-levels.md](durability-levels.md) lands — moving the fsync onto boundaries makes
"which commits survive a power loss" a question with a *design* answer, and a design answer
deserves a mechanical check.

## 5. Racing: the baton

### 5a. Two classes of race, one of them the sim's job

**Go memory races** — two goroutines touching `inflight`, `held`, `logBuf` — are `-race`'s
job. Build the sim with it and they compose; the sim just drives more paths through the
detector. **Transaction-ordering races** — two workers claiming one row, a stale write
landing after a takeover, a signal arriving mid-arm — are where the bugs are, and they are
not goroutine races at all. They are races over database state.

### 5b. The premise: workers share only the database

Outside `template.cache` and `db.clockOffset` there is no mutable package state in
`internal/`. Every `Engine` owns its `sem`, `wake`, `inflight`, `held`, `schemaCache`;
every `*db.DB` its `defCache` and log buffer. `template.cache` memoises a pure function, so
it can change *timing* but never an outcome.

Two caveats, both earned the hard way. This audit was wrong twice before
`internal/archtest` existed — it missed `schema.pendingNodes`, then missed
`api.processSchemaBytes`/`specBytes`. Trust the test, not a reading. And the premise holds
for *state*, not for identity: §6a's worker id had to be made injectable before two engines
in one process meant anything.

### 5c. Boundary scheduling

Each simulated actor — worker step, renewer, flusher, API caller, external service — runs
in a real goroutine but must hold a baton to begin a transaction or an external call. A
single-threaded scheduler decides who gets it. Real goroutines used as coroutines; the
interleaving is a list of scheduler decisions, and that list is the seed.

**Schedule at transaction boundaries, not per statement.** Per-statement deadlocks: hand
the baton to B while A holds a row lock inside an open transaction, and B blocks in the
driver while still holding the baton. Boundary scheduling avoids it, and what it gives up
is the DB's own concurrency control — testing that is testing SQLite and Postgres. Four
interposition points per step suffice: before claim, after claim, before the action, before
persist.

Because of §5b, this controls every race that can change an outcome — which means
replayable cross-worker interleaving is available at roughly tier-1 cost.

### 5d. Recipes

| race | schedule | oracle |
|---|---|---|
| claim vs claim | `A.claim, B.claim` on one runnable row | exactly one wins on PG (`SKIP LOCKED`); epoch bumped once |
| **the fence** | `A.claim → A.action → B.claim(takeover) → A.persist` | A's write refused, `ErrLeaseLost`, no state regression |
| self-reclaim frozen | `A.claim → freeze A's clock → A.claim → A.persist` | needs per-worker clocks (§6b) |
| signal vs arm | deliver the signal before vs. after `ArmExternalOrConsumeSignal` | consumed exactly once either way; never lost, never doubled |
| spawn atomicity | attempt child completion between the inserts and the parent's park | unreachable by construction — `SpawnChildrenAndWait` is one transaction; the test asserts the boundary is real |
| pause vs `FinishChild` | operator pause between a child's persist and `WakeParent` | already a stress suite; here a 3-event schedule |
| lost reply | action succeeds at the service, reply dropped, worker retries | `only_once` reports `interrupted`; execution count stays 1 |

The last is the pattern worth noticing: scheduling alone does not reach it and fault
injection alone does not either. The interesting cases are products of the two.

### 5e. Searching, and shrinking

Uniform-random interleaving finds shallow bugs and stalls. **PCT** — random actor
priorities plus `d-1` random priority-change points — gives probability at least
`1/(n·k^(d-1))` of hitting any bug of depth `d`, and is about twenty lines on top of a
baton scheduler. For small configurations (two workers, one instance, three steps) the
space is small enough to enumerate exhaustively; do that for the fence and the signal/arm
pair specifically, where "we tested some orderings" is not a satisfying claim.

Then **shrink**: delta-debug the decision list while the failure persists. A 10,000-event
failure nobody can read becomes a six-event one that fits in the table above. This is the
step people skip and then abandon the simulator over — a reproducible failure is still not
a debuggable one.

### 5f. Limits

**SQLite hides the race you most want.** `SetMaxOpenConns(1)` serialises writes, and the
claim is a different code path per dialect — `FOR UPDATE SKIP LOCKED` on Postgres against
select-then-update under the single writer on SQLite. The baton still orders
`A.claim, B.claim, A.persist` on SQLite, so the *fence* is fully testable there; what is
not is `SKIP LOCKED`'s own semantics. **Intra-transaction interleaving stays untested**, by
choice — that is the DB's contract, not genroc's.

## 6. Blockers in front of multi-worker

**a. Worker identity — resolved.** `New` built the id as `hostname-pid`, so two Engines in
one process were indistinguishable to every lease predicate — collapsing the very
distinction the fence rests on, since `lease_epoch` fences a stale *grant* while
`worker_id` answers self-reclaim-vs-takeover. `engine.WithWorkerID` (2026-08-08) makes it
injectable; the default is unchanged.

**b. The clock is a process global, so no worker can freeze alone.** `clockOffset` is a
package var in `db`. The lease gate exists to survive a worker whose host slept while the
database's clock kept running — a *skew between one worker and the DB*, which a single
global offset cannot express. Simulating the incident that motivated the feature therefore
requires per-worker clocks: the DB keeps the authoritative one, each worker carries its own
view, and `db.Now()` becomes a method on something the caller holds rather than a package
function. This is the one to decide before writing the sim, not after.

**c. `AdvanceClock` only moves forward, additively.** Correct for `/tick`, where a test
only ever needs to expire something. A sim wants to *set* the clock to the next event's
instant, and per-worker skew wants an offset that can go backwards relative to the DB.

## 7. Seams to cut

**a. `Clock`** — `Now`, `NewTicker`, `After`, `WithTimeout` — injected into `Engine` and
into `db`, replacing the package global. Thirteen `db.Now()` call sites, three tickers.

**b. `transport.Sender`** — an interface with `Send`, injected into `Engine`, replacing the
package-level `client`. The real implementation is the current function unchanged.

**c. An id source in `idgen`.** Two properties must survive the substitution or the sim
tests something other than production: ids sort in creation order, and a child's id sorts
strictly after its parent's — the DB orders and locks a tree by id alone. A seeded
generator that keeps the v7 layout (fake millis from the injected clock, seeded random
tail) preserves both, and `Add`/`After`/`ChildBase` need no change.

**d. An RNG on `Engine`** for backoff jitter, replacing the `math/rand/v2` global.

**e. A fault-injecting `DBTX`** wrapping the existing interface, composed with
`pgRewriter` the same way.

**f. A worker id** — done, §6a.

None of these change production behavior; (a) and (b) are the only ones that touch more
than one file.

## 8. The simulated world

**a. The fetch service.** Not a stub — a model. It records every request keyed by
`(instance, task, attempt)`, so it can answer *how many times did this task actually
execute*, which is the `only_once` oracle and the thing no real test can assert. It can
respond after k virtual milliseconds, return a chosen status, never respond, respond
*after* the caller timed out (the lost-reply case that `only_once.interrupted` exists for),
or respond twice.

**b. External actors.** External tasks and signals arrive over HTTP today. In the sim they
call the handler functions directly with an `Envelope` — an approval that fires at a
virtual instant, a signal delivered while the process is still mid-task (the race
`ArmExternalOrConsumeSignal` resolves). No listener, no port.

**c. Crash, and what it cannot reach.** Discard the worker and rebuild from the same
database. Two traps:

- Object-graph state must be dropped *together*: `schemaCache` on the `Engine`, `defCache`
  and the log buffer on `*db.DB`. Dropping only the `Engine` leaves `defCache` warm and the
  simulation is quietly unfaithful.
- **Package-level state cannot be dropped at all**, because no object graph reaches it. A
  restarted worker inherits a warm `template.cache`. So in-process crash needs an explicit
  reset registry — and `internal/archtest`'s allow-list is exactly the set it must cover,
  which is why that list is required to stay small and justified. Alternatively the crash
  is a subprocess, at the cost of the in-process speed that makes tier 1 attractive.

## 9. Oracles

A simulator without oracles finds panics and deadlocks and nothing else. These are the
invariants worth checking after every step and at quiescence:

- `lease_epoch` never decreases, and no two workers hold one instance at the same epoch.
- An `only_once` task executed at most once per instance — asked of §8a, not of the DB.
- A parent parked on children un-parks exactly when the last child settles. No parent left
  parked with every child terminal.
- Child result count matches spawn count for `child_map` / `child_list`; `child_list`
  results stay in `_spawn_index` order.
- A terminal instance never transitions again.
- Every instance quiesces once the event queue drains — no instance parked on a wake that
  can never come.
- After a crash at any point, the recovered state is one reachable without the crash.
  `only_once.interrupted` and pause-resume are this property with names.

**One oracle that is not sound: audit-trail causal ordering.** Log rows are buffered and
best-effort by design (migration 008) — "a crash drops only buffered rows". So under crash
injection the trail is legitimately incomplete, and any oracle reading it is asserting
something the system never promised. Log-based checks belong only in crash-free runs.

## 10. Workload generation

Hand-written definitions test what someone imagined. The sim needs generated ones: random
task graphs over the action types (`fetch`, `child`, `child_map`, `child_list`, `external`,
`delay`) with `on_error` routes, retry policies, and `only_once` flags. Generating *valid*
definitions is tractable because `internal/validation` will reject the rest, and it has no
`db`/`engine`/`api` dependency. Bias toward the shapes that are hard: nested children, a
catch across a fan-out, `delay` interacting with a `timeout`, a loop that re-enters a task
whose output a later task reads.

## 11. Out of scope

The HTTP layer, `genctl`, the expression evaluator, the schema/compat machinery. All are
deterministic already or tested better by ordinary means; the sim exists for the part where
time, crashes, and two workers meet.

## 12. Open questions

- Does tier 2 pay for itself? Less clear-cut than the first draft assumed, in tier 2's
  disfavour: §5's baton reaches cross-worker replay without the refactor, leaving
  intra-worker timing as the prize. The signal is tier 1 finding bugs whose *schedules*
  reproduce but whose failures do not.
- Per-worker clocks (§6b) are a change to the `db` package's shape, and `db.Now()` is
  called from `api` too. Whether the clock rides on `*db.DB`, on a context, or on an
  explicit argument is unsettled, and it is the one decision the sim cannot route around.
- Where does the sim live — a `sim` package under `internal/`, or its own module so its
  fault-injection types cannot leak into production builds?
- Postgres in the sim. SQLite-on-tmpfs is the obvious substrate, but the claim query
  differs by dialect (§5f), so a SQLite-only sim leaves the Postgres claim path exactly as
  tested as it is today.
- Does the crash-reset registry (§8c) belong in the sim, or is it a general facility the
  archtest allow-list should be required to register into?

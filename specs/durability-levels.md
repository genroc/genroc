# Durability levels: move the fsync from every commit to a few boundaries

Today every persist is an fsync. That is the strongest guarantee available and it is not
the one the product promises — the contract is already at-least-once, so most commits buy
durability nobody asked for. This records what an fsync actually costs (measured, after
discovering the benchmarks were measuring a no-op), why a handful of boundaries is
sufficient, which boundaries, and why the answer differs per engine.

## 0. Status

**Proposal.** One piece is built: `--sqlite-fullfsync` / `db.WithFullFsync()`
(2026-08-06), which exists only so the benchmarks stop lying — it changes no default and
implements none of the design below. §1 and §2 are measurements, not proposals: they are
reproducible today (§9).

## 1. Every benchmark to date measured a no-op

macOS `fsync(2)` returns before the drive flushes its write cache; `F_FULLFSYNC` is the
call that does not. `pg_test_fsync` on the M1:

| method | µs/op | ops/sec |
|---|---|---|
| `fsync` / `fdatasync` | 22 | 45,200 |
| `fsync_writethrough` (F_FULLFSYNC) | **4,070** | **245** |

**185×.** So `--sqlite-synchronous=FULL` — the shipped default, documented in
[internal/db/db.go](../internal/db/db.go) as "power-loss durable, matching Postgres" — is
not power-loss durable on any Apple machine, and no number collected on one says anything
about production. Dockerized Postgres lies too (~0.23 ms): it runs in the LinuxKit VM,
where `fsync_writethrough` does not exist as an option.

The methodology that survives a lying filesystem is to **count fsyncs, not time them**.
The count is a property of the code and is deterministic enough to assert in CI; latency
is a property of the target hardware, measured once. Throughput is their product.

## 2. What it costs

`make bench-drain` — 5,000 independent two-task roots, the purest queue-throughput
workload (one claim + one terminal write each), M1:

| config | inst/s |
|---|---|
| SQLite `FULL` (shipped default, fake fsync) | 5,133 |
| SQLite `NORMAL` (fake) | 6,083 |
| SQLite `FULL` + F_FULLFSYNC — **honest** | **183** |
| SQLite `NORMAL` + F_FULLFSYNC (checkpoint syncs only) | 3,858 |

The honest run is 6,706 fsyncs across the drain phase, 1.34 per instance — already below
one-per-commit because `ClaimInstances` batches. Wall time is `fsync_count × 4.07 ms` to
within 1%: 27.295 s / 4.07 ms = 6,706, and 246 / 1.34 = 183 against 183 measured. These
workloads are entirely fsync-bound; the CPU never enters into it.

**The prize is 21×** — 183 → 3,858 — and `NORMAL`'s 3,858 is the hard ceiling. No
durability scheme beats never syncing, and WAL checkpointing is the floor beneath it.

## 3. Why a boundary is enough: prefix durability

Both engines append commits to a single WAL, so an fsync at commit N hardens 1..N-1. Read
it backwards, which is the form that matters: **no later state can survive without its
predecessor.** A parent cannot have advanced past a child that is un-finished; a spawned
sibling cannot exist without the spawn that made it. The inconsistent state is
unreachable, not merely unlikely.

This is what licenses skipping the fsync on a process end. Losing a terminal write costs a
replay, and replay is what the contract already sells.

**Rejected: "something downstream will fsync anyway."** True, and unusable. It makes
correctness depend on reasoning about what happens *after* the commit in question, which
is fragile under refactoring and would force every child spawn to become a boundary. The
backwards form needs no such reasoning.

## 4. The boundaries are ingress, not egress

> fsync where work **enters** the system from a party that will not re-send it, and around
> anything that cannot be replayed. Nowhere else.

Egress is derivable from what is already durable. Ingress is not: lose it and the work is
not repeated, it is **forgotten** — a permanent hang, which is strictly worse than the
failure mode the contract buys.

| boundary | why | batches? |
|---|---|---|
| process create from outside | we 2xx'd a caller who will not re-submit | yes, across callers |
| delivery into a park with **no deadline** | see below | yes |
| `only_once` execute (before + after) | not replayable | rare by construction |
| everything else | replay covers it | — |

**The deadline refinement.** [internal/engine/action.go](../internal/engine/action.go)
records that an external task has no default timeout — "parking indefinitely is what it is
for." So inbound delivery splits: if the park has a deadline, a lost delivery degrades to
`external.timeout`, `on_error` routes it, and the user can retry — recoverable, no fsync
needed. If it has none, the instance parks forever and nobody re-delivers. `runExternal`
already computes `hasDeadline` at arm time, so the rule is exactly expressible.

**`only_once` cannot be dropped, and costs nothing to keep.** The evidence it runs on is
the claim — `worker_id` plus task position, durable before the request leaves — which is
what `interruptedOnlyOnce` reads on both reclaim paths
([internal/engine/advance.go](../internal/engine/advance.go)) and what
[internal/db/CLAUDE.md](../internal/db/CLAUDE.md) means by "an unlisted row expires with
`worker_id` intact." Lose that write to a power cut after the request went out and
recovery does not see an interrupted task; it sees an instance at an earlier position,
unclaimed, and re-runs it. That is not `interrupted` degrading — `interrupted` is a *true*
answer a definition can route on — it is a confident wrong one.

Nor is it a contract relaxation, because `only_once` **is** the opt-out from "tasks may
repeat." And the bracket only fires on tasks that carry the flag, so a definition without
them runs at the full 3,858. There is no throughput argument for dropping it.

## 5. The ladder

Levels are strictly increasing; each adds fsync points to the one above.

| level | guarantee | adds | drain |
|---|---|---|---|
| `none` | consistency only; accepted work can vanish | — | 3,858 |
| `accepted` | handed work is never forgotten | create, deadline-less delivery | 3,858 ¹ |
| `only-once` | + `only_once` never runs twice | bracket around `only_once` | 3,858 ² |
| `terminal` | + a finished process stays finished | process end | ~3,000 |
| `strict` | + no completed task ever repeats | every commit | 183 |

¹ creates sit in drain's untimed load phase; steady state costs 1 fsync per accepted item,
batched across concurrent callers. ² free unless the definition uses `only_once`.

**Default: `only-once`.** It is the strongest guarantee that costs nothing over `accepted`,
and 21× faster than what ships today.

`terminal` exists as its own rung because it is a real and much cheaper guarantee than
`strict` — one sync per process rather than one per task — and it is the level that stops
an external poller from seeing `completed` and then `running` again after a power cut.

**The rule that has to hold in the code:** every write path declares which level makes it
durable, and **an unclassified path syncs**. Forgetting to classify a new endpoint must
cost throughput, never a guarantee. This is deliberately the inverse of how the flag reads
to an operator — the flag is a ceiling they lower, the call site is a floor the author
raises. Mechanically it is one lever per engine: `PRAGMA synchronous` flipped around the
transaction on SQLite (safe only while the pool is pinned at 1), `SET LOCAL
synchronous_commit` on Postgres.

## 6. Group commit is Postgres-only, and it changes the priority

Concurrent Postgres committers coalesce into one flush automatically; `commit_delay`
widens the window deliberately. Fsyncs per 10,000 commit-units (5,000 creates + 5,000
drains), Docker PG 16:

| config | fsyncs | commits/fsync | durability |
|---|---|---|---|
| `commit_delay=0`, pool=50 — current default | 2,021 | 4.9 | full |
| `commit_delay=2000µs`, pool=50 | 1,264 | 7.9 | full |
| `commit_delay=2000µs`, pool=300 | 994 | 10.1 | full |
| `synchronous_commit=off` | 79 | 127 | relaxed |

Batch width is `arrival_rate × flush_window`, capped by **`--pg-max-open-conns`** (default
50) — not by `--max-concurrent`, since only transactions simultaneously in flight can
coalesce. The pool is the group-commit ceiling, which its flag help does not say.

These ratios are a **floor**. Docker's window is 0.23 ms against real storage's 4.07 ms, so
an honest disk collects a queue ~18× deeper. In the limit where flush time dominates, batch
width tends to the pool size and throughput to `pool / T` — order 12,000 commits/s at
pool=50 — against SQLite's strictly serial `1 / T` = 246/s.

Three consequences:

- **`strict` is affordable on Postgres and ruinous on SQLite.** The ladder is mostly a
  SQLite feature. `SetMaxOpenConns(1)` ([internal/db/db.go](../internal/db/db.go)) makes
  commits serial; no knob creates group commit there.
- **The SQLite analogue is app-level.** Coalesce several instance advances into one
  transaction in the poll loop. `ClaimInstances` already returns a batch, so the shape
  exists. Full durability retained.
- **Group commit fixes throughput, never latency.** One process's tasks are causally
  sequential — commit K+1 cannot be issued until K returns, so nothing batches with it. A
  50-task process under `strict` pays 50 × 4.07 ms ≈ 200 ms of pure fsync on either engine.
  Only the boundary scheme touches that number.

So the cheapest wins spend no guarantee at all, and the ladder should land after them:
expose `commit_delay` and document the pool as batch width; batch advances on SQLite; then
the ladder.

## 7. Hazards

**Lease epoch reuse across a rewind.** [internal/db/CLAUDE.md](../internal/db/CLAUDE.md)
has `lease_epoch` moving only in `ClaimInstances`, with every leased write carrying `AND
lease_epoch = ?`. That fence assumes epochs are monotonic. A rewind can un-issue a claim
while the worker that won it is still running and still writing at that epoch; the next
claim re-issues the same number to a different worker, and both pass the fence. Fewer
fsyncs mean a wider rewind window, so this design sharpens an existing exposure rather
than creating one. Candidate fixes: add `worker_id` to the fence predicate, or bump all
epochs by a large constant after an unclean shutdown. Unresolved.

**Reader-visible rewind.** Below `terminal`, a client polling an instance can see
`completed` and later `running` again. Consistent with at-least-once, but it means a read
result is not stable, which is a different promise from "tasks may repeat" and should be
documented as such.

## 8. Open questions

- **Who sets the level** — an operator flag on `genroc`, a `durability:` field on the
  process definition, or both (flag sets default and minimum, definition raises its own
  floor). Per-definition is far more useful — a payment flow at `strict` beside a
  log-shipper at `none` — but touches `internal/model` validation, the JSON schema, the
  editor schema, and needs rules for child inheritance and for one transaction committing
  work for two instances at different levels. Undecided; blocks implementation of §5.
- **The Postgres projection is unverified on honest storage.** Every number in §6 came off
  a 0.23 ms disk. Native Homebrew Postgres on macOS *can* do
  `wal_sync_method = fsync_writethrough`, which would give the matched comparison against
  SQLite's F_FULLFSYNC run. Note PG 18 moved `wal_sync` out of `pg_stat_wal` into
  `pg_stat_io`.
- **Is `terminal` worth shipping**, or does `only-once` → `strict` cover the real demand?

## 9. Reproducing

    # real fsync cost for this disk
    pg_test_fsync -s 2

    # SQLite, honest
    GENROC_SQLITE_SYNCHRONOUS=FULL GENROC_SQLITE_FULLFSYNC=1 make bench-drain
    GENROC_SQLITE_SYNCHRONOUS=NORMAL GENROC_SQLITE_FULLFSYNC=1 make bench-drain

    # Postgres fsync count (PG <= 17)
    psql … -c "select pg_stat_reset_shared('wal')"
    POSTGRES_DSN=… make bench-drain
    psql … -c "select wal_sync from pg_stat_wal"

**Blocker for the Postgres path:** `anyWithStatus(client, "failed")` in
[tests/bench/run.ts](../tests/bench/run.ts) is unscoped, so `make bench-*` against any
database that has ever run the test suite aborts on leftover failed rows before measuring.
SQLite gets a fresh temp file per run and never trips it. Bench against a fresh database,
or scope the check to the workload's own definition name.

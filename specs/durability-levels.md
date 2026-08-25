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

**2026-08-25.** Every measurement below was re-run on the current tree and reproduces
(§2). §9's blocker is fixed, and its stated cause was wrong — see there. §8's first
question is decided: **flag + per-definition field**, and the two sub-questions it called
blocking are answered (child inheritance turns out not to be a question at all). §7's
`lease_epoch` hazard is **closed** — it was the one item that had to land before any level
below `strict` ships, so it did. The ladder itself is still unbuilt.

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

Re-run 2026-08-25 on the current tree, same M1, same workload — every figure holds, and
Postgres is added as the matched fourth row:

| config | inst/s | 2026-08-06 |
|---|---|---|
| SQLite `FULL` + F_FULLFSYNC — **honest** | **177** | 183 |
| SQLite `NORMAL` + F_FULLFSYNC | **3,909** | 3,858 |
| SQLite `FULL`, plain fsync (shipped default, fake) | 5,429 | 5,133 |
| Postgres 16 (Docker, 0.23 ms fake fsync) | 2,138 | — |

Postgres drained 5,000 instances on 2,149 `wal_sync`s across ~10,000 commit-units — 4.65
commits per fsync, against §6's independently measured 4.9. The bench now prints
`fullfsync=on|off` on its durability line: a throughput number that does not say which
fsync produced it is exactly the number §1 is about.

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

### 5a. Measured (2026-08-25) — and a rung's value is a property of the WORKLOAD

Honest SQLite (`FULL` + F_FULLFSYNC, 4.06 ms/fsync), levels as built. Two workloads,
because one of them cannot see the difference the other is entirely about:

| level | `bench-drain` (inst/s) | `bench-iterate` (wall ms) |
|---|---|---|
| `strict` | 178 | 11,666 |
| `terminal` | 190 (1.07×) | 2,368 (**4.9×**) |
| `only-once` | 970 (5.5×) | 2,134 (5.5×) |

**`only-once` is ~5.5× on both. `terminal` is worth nothing on one and nearly everything on
the other.** The variable is **yields per process**, not tasks and not iterations:
`advance()` collapses a call-less chain into one write
([advance.go](../internal/engine/advance.go), `maxInlineTasks`), so a switch loop of any
length still costs one flush. Only a task that PARKS, SPAWNS or CALLS forces its own. Drain
is two tasks and one terminal write, so at `terminal` every instance still flushes once and
nothing improves; `iterate` parks 20 times per process, so `terminal` replaces 40 flushes
with one.

This answers §8's "is `terminal` worth shipping?" — **yes**, and the earlier reading that it
was speculative came from measuring only the shape that cannot show it. Every workload in
the suite before `bench-iterate` was that shape: `drain` is two tasks, `deep` and
`recursive` are trees whose instances each run once. A rung looking useless is evidence
about the benchmark until a workload of the opposite shape agrees.

It is also the strongest argument for the per-definition field: with one global level, a
deployment running both shapes has no setting that is right for both.

**What still floors all three levels: the claim.** `ClaimInstances` syncs at every level —
conservatively, since §4 only requires it for tasks that actually carry `only_once` and the
engine, not the DB, is what knows which those are. A parking workload re-claims after every
resume, so `iterate`'s `only-once` sits at 2,134 ms rather than near zero. Relaxing it
means moving the `only_once` bracket into the engine, and is the next real win if `only-once`
is not fast enough.

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

### 6a. Measured on honest storage (2026-08-25) — and the shape is not what §1 predicts

Native PG 18 on the M1 with `wal_sync_method = fsync_writethrough` (`pg_test_fsync`: 4,063
µs/op against `fdatasync`'s 21 µs — the same 185× lie §1 found for SQLite). This is the
matched comparison §8 asked for. `synchronous_commit=on` throughout, pool 200,
`bench-drain`, median of 3:

| `commit_delay` | inst/s | fsyncs | commits/fsync | vs 0 |
|---|---|---|---|---|
| 0 (today's default) | 1,663 | 1,163 | 8.6 | 1.00× |
| 200 µs | 1,883 | 982 | 10.2 | 1.13× |
| **500 µs** | **2,206** | 833 | 12.0 | **1.33×** |
| 1000 µs | 2,100 | 774 | 12.9 | 1.26× |
| 2000 µs | 1,928 | 749 | 13.4 | 1.16× |
| 5000 µs | 1,682 | 598 | 16.7 | 1.01× |
| 10000 µs | 1,228 | 644 | 15.5 | 0.74× |

**Throughput peaks at 500 µs and then falls while the fsync count keeps dropping.** At 5,000
µs the run made the second-fewest fsyncs of any row and was no faster than doing nothing; at
10,000 µs it was 26% slower. So §1's "count fsyncs, not time them" is **right for comparing
durability schemes and wrong for tuning a delay**: the delay is not free, it lands on the
critical path, and fsyncs-per-commit is not the objective function. Optimising the count
here makes the system slower. Anything tuning `commit_delay` must be timed.

**The size of the gain did not reproduce, and that is the more useful result.** The table
above was taken with other load on the machine (a second Postgres in Docker). Re-measured
through the shipped flag on a quiet machine, 3 interleaved reps: 0 µs → 2,164 inst/s
(2,164/1,990/2,235), 500 µs → 2,231 (2,287/2,217/2,231) — **1.03×, not 1.33×**. Note which
number moved: the 500 µs result is the same in both sessions (2,206 then 2,231) while the
baseline went 1,663 → 2,164. So `commit_delay` is not raising a ceiling, it is **removing a
downside** — it makes throughput under contention look like throughput without it. Quote it
that way; a single A/B on an idle laptop will show almost nothing and a single A/B on a busy
one will show a third, and both are the same effect.

**Concurrent flushes overlap.** 1,163 fsyncs in 2.9 s is ~400/s on a device that does 246/s
serially, so `F_FULLFSYNC` calls from different backends coalesce at the drive. Postgres
therefore beats what `count × latency` says is possible — and SQLite, one connection and
strictly serial, cannot: its wall time matched `count × latency` to 1% in §2. That is a
second, independent reason the two engines diverge here, on top of group commit.

**`commit_siblings` is the safety valve, and it is why the latency warning below overstates
the risk.** Postgres skips the delay entirely unless at least `commit_siblings` (default 5)
transactions are open. On `bench-deep` — deliberately "mostly 1-2 instances wide" — the
delay is unobservable: **501 / 499 / 526** inst/s at 0 / 500 / 2000 µs. The knob disables
itself on exactly the causally-sequential shapes it would otherwise slow down, so it is safe
to enable without knowing the workload. It still must not be defaulted on: the optimum is a
fraction of the storage's flush latency (~12% of 4.06 ms here) and would be wrong on an NVMe
that flushes in 50 µs.

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

### 6b. What was built (2026-08-25), and what it means for the rest of this doc

The decision taken was to **keep every commit synchronous and attack the number of flushes
instead**, which is the first of those three and none of the ladder. Built:
`--pg-commit-delay` (µs, default 0), applied with `SET` on each pooled connection rather
than in `postgresql.conf` so it scopes to genroc's own connections and taxes no other
database on the server; `--pg-max-open-conns` help now states it is the group-commit
batch-width ceiling; `--sqlite-synchronous` help now states SQLite's ceiling. `commit_delay`
is superuser-context, so a connection that cannot set it fails rather than silently not
applying a flag the operator asked for.

**SQLite is left at its ceiling, deliberately.** Under "synchronous always" it has one
writer, no group commit and no knob: 246 serial fsyncs/s ÷ 1.34 per instance ≈ 180 inst/s,
which is what §2 measures. The app-level batching above is the only lever that would move it
without spending durability, and it was **not** built — SQLite is positioned as the
single-node and development engine, Postgres as the throughput one, and §6a is the evidence
for that split (1,663 against 177 at identical durability, ~9.4×).

This leaves §5's ladder unbuilt and, for now, unscheduled rather than rejected: nothing in
§1–§4 is contradicted, and the reason to reopen it is unchanged — a deployment that needs
more than full-durability Postgres can give, or a SQLite deployment that needs more than
~180 inst/s.

## 7. Hazards

**Lease epoch reuse across a rewind — CLOSED 2026-08-25, before the rest of this design.**
`lease_epoch` moves only in `ClaimInstances`, and every leased write carried `AND
lease_epoch = ?`. That fence assumed epochs are monotonic. A rewind can un-issue a claim
while the worker that won it is still running and still writing at that epoch; the next
claim re-issues the same number to a different worker, and both passed. The fence now also
carries `AND COALESCE(worker_id,'') = ?`, so one epoch is a grant to one worker
([lease-fencing.md](lease-fencing.md) records why this is not the `worker_id`-as-token
option that doc rejects, and the default `worker_id` gained a random suffix so two live
workers cannot share one).

Reachability, which is why it was worth closing first rather than only widening: a rewind
needs the DB to lose a commit **while a worker survives it**. SQLite's is in-process, so a
rewind takes the worker with it — unreachable there at any level. On Postgres it is
reachable today only through failover to a lagging replica; what this design adds is that
an ordinary unclean shutdown of the DB host does it too. One narrow case stays open by
choice: `runAdvance` drops its in-flight marker before persisting, so a rewind inside that
gap can let one worker's two advances both match. Closing it needs rewind *detection*
(`pg_postmaster_start_time()` moves iff commits were lost), rejected for now as a second
mechanism to keep true for a window this small.

**Reader-visible rewind.** Below `terminal`, a client polling an instance can see
`completed` and later `running` again. Consistent with at-least-once, but it means a read
result is not stable, which is a different promise from "tasks may repeat" and should be
documented as such.

## 8. Open questions

- ~~**Who sets the level**~~ — **decided 2026-08-25: both.** The flag sets the default and
  the minimum; a `durability:` field on the definition may only raise its own floor, never
  lower it, so `effective = max(flag, definition)` and an operator's guarantee cannot be
  weakened by something they did not write. The two sub-questions it was blocked on:

  **Child inheritance is not a question.** §3 already answers it: an fsync at commit N
  hardens 1..N-1, so a later sync covers every earlier commit whoever wrote it. A `strict`
  parent spawning a `none` child does not need to lift the child, because the parent's own
  next sync hardens the child's writes anyway; and if the crash lands before that sync, the
  parent is still parked on the child and the child replays — at-least-once, the contract.
  The parent's guarantee is about the parent's tasks, and it survives intact. So a child
  takes its own definition's level against the flag, exactly as a root does, and there is
  no inheritance rule to write. `only_once` inside a child is likewise the child
  definition's business, and the default level already covers it.

  **A transaction spanning two instances takes the max of their levels.** It is the
  conservative direction (more durable, never less) and needs no reasoning about which
  instance "owns" the commit. The paths that span instances are `SpawnChildrenAndWait`,
  `FinishChild`, `FailInstanceAndAncestors`, and the subtree verbs
  (`PauseProcess`/`ResumeProcess`/`RetryProcess`) — the last three are operator-driven and
  rare, so the max costs them nothing.

  What remains is mechanical, not a design question: `internal/model` validation, the JSON
  schema, the editor schema, and where the level is read from in the delivery path
  ([internal/db/db_signals.go](../internal/db/db_signals.go) holds an instance id, not a
  definition — either look the definition up or denormalize the level onto the row).
- **The Postgres projection is unverified on honest storage.** Every number in §6 came off
  a 0.23 ms disk. Native Homebrew Postgres on macOS *can* do
  `wal_sync_method = fsync_writethrough`, which would give the matched comparison against
  SQLite's F_FULLFSYNC run. Note PG 18 moved `wal_sync` out of `pg_stat_wal` into
  `pg_stat_io`.
- ~~**Is `terminal` worth shipping**~~ — **answered yes, 2026-08-25 (§5a).** It is worth
  4.9× on a process that parks 20 times and 1.07× on a two-task one, so the rung is not
  redundant with `only-once` → `strict`; it is the one that pays for exactly the shape
  `only-once` would otherwise be needed for. The question read as open for as long as it
  did because every workload in the suite was the shape that cannot distinguish it.

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

    # Postgres, honest — Docker cannot do this (§1), so use a native cluster. This is what
    # §6a was measured on and what makes any Postgres durability number meaningful.
    initdb -D "$PGDATA" -U genroc --auth=trust
    cat >> "$PGDATA/postgresql.conf" <<'CONF'
    port = 5433
    wal_sync_method = fsync_writethrough   # macOS F_FULLFSYNC; the whole point
    max_connections = 400                  # must exceed --pg-max-open-conns
    CONF
    pg_ctl -D "$PGDATA" -l pg.log start
    pg_test_fsync -s 2                     # expect ~4ms for fsync_writethrough, ~21us for fdatasync

    # PG 18 moved the counter: pg_stat_wal.wal_sync -> pg_stat_io.fsyncs
    psql -p 5433 … -c "select pg_stat_reset_shared('io')"
    POSTGRES_DSN=… GENROC_PG_COMMIT_DELAY=500 GENROC_PG_MAX_OPEN_CONNS=200 make bench-drain
    psql -p 5433 … -c "select sum(fsyncs) from pg_stat_io where object='wal'"

    # The ladder, on both shapes — one of them cannot show `terminal` at all (§5a)
    GENROC_SQLITE_SYNCHRONOUS=FULL GENROC_SQLITE_FULLFSYNC=1 GENROC_DURABILITY=terminal make bench-drain
    GENROC_SQLITE_SYNCHRONOUS=FULL GENROC_SQLITE_FULLFSYNC=1 GENROC_DURABILITY=terminal make bench-iterate

**Interleave the A/B and take a median.** Single runs are worthless here: the same 500 µs
config measured 1.33× and 1.03× in two sessions on one machine, and the variance is in the
baseline, not the treatment (§6a). `bench-deep` is the control — a workload narrow enough
that `commit_siblings` gates the delay off entirely, so it must show no change.

**The Postgres blocker is fixed (2026-08-25), and its diagnosis above was wrong.** The
symptom was right — `anyWithStatus` in [tests/bench/run.ts](../tests/bench/run.ts) was
unscoped, so `make bench-*` against a database the test suite had touched aborted on
leftover `failed` rows before measuring. But scoping it to a time window does **not** fix
it: `dbtest` fixtures call `db.AdvanceClock`, so their rows carry timestamps in the
*future* and outlive any `created_after`. The check now carries both bounds — the
workload's own definition name (`process=`, new on `GET /instances`) excludes foreign
fixtures whatever their clock says, and `created_after` excludes an earlier failed bench of
the same workload, which would otherwise poison every run after it. Neither bound alone is
enough, which is why both are there.

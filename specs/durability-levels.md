# Durability levels: move the fsync from every commit to a few boundaries

The contract is at-least-once, so most commits buy durability nobody asked for. `--durability`
fsyncs only where losing a commit would break a promise, and leaves the rest to replay.

## 0. Status

Status: **Built, except the per-definition `durability:` field (§8), the `none` / `accepted`
rungs (§5) and the deadline refinement for deliveries (§4).**

## 1. macOS `fsync` does not flush

macOS `fsync(2)` returns before the drive flushes its write cache; `F_FULLFSYNC` does not. On the
M1, `pg_test_fsync`: 22 µs against **4,070 µs** — **185×**. So `--sqlite-synchronous=FULL` is not
power-loss durable on Apple hardware without `--sqlite-fullfsync` (which exists for the
benchmarks), Dockerized Postgres lies too (~0.23 ms, no `fsync_writethrough` in the LinuxKit VM),
and no throughput number that does not say which fsync produced it means anything — the bench
prints `fullfsync=on|off`.

To compare durability *schemes*, count fsyncs rather than timing them: the count is a property of
the code, latency of the hardware. (Not for tuning a delay — §6a.)

## 2. What it costs

`make bench-drain` (5,000 two-task roots, one claim + one terminal write each), honest SQLite on
the M1: `FULL` + F_FULLFSYNC **177–183 inst/s**, `NORMAL` + F_FULLFSYNC **3,858–3,909**. The
workload is entirely fsync-bound — wall time is `fsync_count × 4.07 ms` to within 1% — so the
prize is **21×**, and `NORMAL` is the ceiling: no scheme beats never syncing.

## 3. Why a boundary is enough: prefix durability

Both engines append commits to one WAL, so an fsync at commit N hardens 1..N-1. Read it
backwards, which is the form that matters: **no later state can survive without its
predecessor.** A parent cannot have advanced past an un-finished child; a spawned sibling cannot
exist without its spawn. Losing an unflushed write costs a replay, which the contract already
sells.

Rejected: "something downstream will fsync anyway". True, and unusable — it makes correctness
depend on what happens *after* the commit in question, fragile under refactoring.

## 4. The boundaries are ingress, not egress

> fsync where work **enters** the system from a party that will not re-send it, and around
> anything that cannot be replayed. Nowhere else.

Egress is derivable from what is already durable. Lost ingress is not repeated but **forgotten** —
a permanent hang, strictly worse than the failure the contract buys.

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
already computes `hasDeadline` at arm time, so the rule is exactly expressible. [unbuilt:
every delivery syncs, deadline or not]

**`only_once` cannot be dropped.** Its evidence is the claim — `worker_id` plus the task the row
names, durable before the request leaves — which `interruptedOnlyOnce` reads on reclaim. Lose that
write to a power cut and recovery sees an earlier, unclaimed position and re-runs the request: not
`interrupted` degrading, but a confident wrong answer. It costs nothing on a definition that does
not use the flag.

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

`none` and `accepted` were not built; the shipped ladder starts at `only-once`.

**Default: `only-once`.** It is the strongest guarantee that costs nothing over `accepted`,
and 21× faster than `strict`. `terminal` is the level that stops a poller seeing `completed` and
then `running` again after a power cut — one sync per process rather than one per task.

**The rule that has to hold in the code:** every write declares the weakest level at which it
still syncs (`beginTxAt` / `withTxAt`), and **an unclassified path syncs** (`beginTx` /
`withTx`). Forgetting to classify costs throughput, never a guarantee — the operator's flag is a
ceiling they lower, the call site a floor the author raises. The lever is per transaction:
`SET LOCAL synchronous_commit = off` on Postgres; on SQLite `PRAGMA synchronous = NORMAL` on a
pinned connection, restored before it returns to the pool, or the next write silently inherits
the relaxed level.

### 5a. A rung's value is a property of the workload

Honest SQLite, levels as built:

| level | `bench-drain` (inst/s) | `bench-iterate` (wall ms) |
|---|---|---|
| `strict` | 178 | 11,666 |
| `terminal` | 190 (1.07×) | 2,368 (**4.9×**) |
| `only-once` | **3,551 (19.8×)** | **419 (27×)** |

The variable is **yields per process**. `advance()` collapses a call-less chain into one write, so
only a task that parks, spawns or calls forces its own; drain flushes once per instance at
`terminal` and gains nothing, while iterate parks 20 times per process and `terminal` replaces 40
flushes with one. A rung that looks useless is evidence about the benchmark until a workload of
the opposite shape agrees — and with one global level, a deployment running both shapes has no
setting right for both (§8).

### 5b. The `only_once` bracket

`ClaimInstances` is an ordinary relaxed write (on Postgres it must not be autocommit, or the level
could not reach it). The `only_once` bracket lives in the engine, where the knowledge is:

- **`hardenClaims`** flushes once per batch, after the claim and before anything dispatches, if
  any claimed instance is at an `only_once` task;
- **`runAdvance`** flushes after the write that records that task's result.

**Keyed on the CLAIMED task.** Recovery reads `inst.Task` as stored, so a flush at the action —
reached inline from an earlier claimed task — protects nothing. That is why `advance()` **bails
out before an `only_once` action it moved to in this advance** (`i > 0`), checkpointing so the row
names it; the next claim is then the protected case. The checkpoint need not be durable (losing it
rewinds before the action ran), and `i > 0` also catches a loop re-entering the task within one
advance. Without it a crash mid-request read as "never started" and re-ran the request.

The condition is a column, `next_replayable`, not a definition lookup: the claim path is the
hottest there is and runs outside the panic barrier that user-data definitions need. It is stored
in the **replayable** direction so false — Go zero value and column default — is the safe answer:
a forgotten create path costs an fsync, not at-most-once. `persist` re-derives it on every write;
both create paths (the API and `newChildInstance`) must set it, or every instance flushes on its
first claim.

The primitive is `db.Flush` — any flushed commit hardens every commit before it (§3), so being a
commit is the whole job; a no-op at `strict`. Postgres runs `SELECT pg_current_xact_id()`, a real
commit with no row — a shared marker row would serialise every worker's flush behind one lock
held across the fsync. SQLite has no equivalent (a page must change) and serialises commits
anyway, so it bumps `durability_marker`. Tests assert the in-process `FlushCount`, never the row.

Losing the **after** half does not break at-most-once — recovery reads the durable claim and
reports `only_once.interrupted` — it loses the work done, which is why both halves exist.

## 6. Group commit is Postgres-only, and it changes the priority

Concurrent Postgres committers coalesce into one flush; `commit_delay` widens the window. Batch
width is capped by **`--pg-max-open-conns`** (default 50), not `--max-concurrent` — only
transactions in flight together coalesce. At defaults on Docker PG 16 that is 4.9 commits per
fsync.

- **`strict` is affordable on Postgres and ruinous on SQLite.** The ladder is mostly a
  SQLite feature. `SetMaxOpenConns(1)` ([internal/db/db.go](../internal/db/db.go)) makes
  commits serial; no knob creates group commit there.
- **The SQLite analogue is app-level.** Coalesce several instance advances into one
  transaction in the poll loop. `ClaimInstances` already returns a batch, so the shape
  exists. Full durability retained.
- **Group commit fixes throughput, never latency.** One process's tasks are causally sequential,
  so a 50-task process under `strict` pays 50 × 4.07 ms ≈ 200 ms of fsync on either engine. Only
  the boundary scheme touches that.

### 6a. `commit_delay` on honest storage

Native PG 18, `wal_sync_method = fsync_writethrough`, pool 200, `bench-drain`:

- **Throughput peaks at a small delay (500 µs) and falls while the fsync count keeps dropping**
  (10,000 µs was 26% slower than none). The delay sits on the critical path, so tune it by time,
  never by fsync count.
- **It removes a downside rather than raising a ceiling**: 1.33× on a loaded machine, 1.03× on a
  quiet one, with the 500 µs figure identical in both — only the baseline moved.
- **`commit_siblings` (default 5) gates it off** on narrow, causally sequential workloads
  (`bench-deep` unchanged), so enabling it is safe without knowing the workload. It is still not
  defaulted on: the optimum is a fraction of the device's flush latency (~12% of 4.06 ms here).
- Concurrent `F_FULLFSYNC`s from different backends overlap at the drive, so Postgres beats
  `count × latency`; SQLite, strictly serial, cannot. At identical durability: 1,663 against 177
  inst/s.

### 6b. What was built

`--pg-commit-delay` (µs, default 0) is applied with `SET` on each pooled connection, so it taxes
no other database on the server. `commit_delay` is superuser-context, so a connection that cannot
set it fails rather than silently not applying a flag the operator asked for.

**SQLite is left at its ceiling, deliberately.** Under "synchronous always" it has one
writer, no group commit and no knob: 246 serial fsyncs/s ÷ 1.34 per instance ≈ 180 inst/s,
which is what §2 measures. The app-level batching above is the only lever that would move it
without spending durability, and it was **not** built — SQLite is positioned as the
single-node and development engine, Postgres as the throughput one, and §6a is the evidence
for that split (1,663 against 177 at identical durability, ~9.4×).

## 7. Hazards

**Lease epoch reuse across a rewind** is closed by the `worker_id` conjunct on the fence —
[lease-fencing.md](lease-fencing.md) §The model. What this design adds is reach: below `strict`,
an ordinary unclean shutdown of the Postgres host can lose commits a surviving worker acted on,
where before only failover to a lagging replica could. SQLite's database is in-process, so a
rewind takes the worker with it.

**Reader-visible rewind.** Below `terminal`, a client polling an instance can see `completed` and
later `running` again — consistent with at-least-once, but a different promise from "tasks may
repeat"; the `--durability` help says so.

## 8. Open questions

- ~~**Who sets the level**~~ — **decided 2026-08-25: both. The flag SHIPPED; the
  per-definition field is deferred, not queued.** The flag sets the default and the
  minimum; a `durability:` field on the definition may only raise its own floor, never
  lower it, so `effective = max(flag, definition)` and an operator's guarantee cannot be
  weakened by something they did not write.

  Deferred because it adds a per-instance column and a rule operators must understand, for
  a benefit no deployment needs yet. **The trigger to build it** is one deployment running
  both process shapes at once: §5a shows `terminal` is worth 4.9x on a parking-heavy
  process and nothing on a two-task one, so a single global level serves such a deployment
  badly. Sizing, from having built the rest: the mechanism is done, so the work is the
  level reaching ~6 instance-scoped writes (the `next_replayable` denormalisation pattern,
  including its 3 scan sites and 2 inline param builders), the model/validation surface,
  and one widened condition in `hardenClaims` for a `strict` definition's claim.

  Two traps worth knowing before starting. The **zero value must be `strict`**, which is
  the EXPENSIVE level -- so every create path must set it explicitly or throughput
  collapses; that is the bug shape hit on 2026-08-25 with `next_replayable`, except the
  fail-safe default costs 18x rather than one fsync. And `max(flag, definition)` means
  **lowering the flag cannot speed up a `strict` definition**, which is correct and
  surprising, so it is a documentation problem as much as an implementation one.

  A cheaper variant that covers the motivating case: since the flag is already a floor, the
  only useful thing a definition can do is raise itself, so `durability: strict` as a single
  opt-in needs no ordering rules at all.

  The two sub-questions it was blocked on:

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
  `RespawnSlotsAndWait`, `FinishChild`, `FailInstanceAndAncestors`, and the subtree verbs
  (`PauseProcess`/`ResumeProcess`/`RetryProcess`) — the last three are operator-driven and
  rare, so the max costs them nothing.

  What remains is mechanical, not a design question: `internal/model` validation, the JSON
  schema, the editor schema, and where the level is read from in the delivery path
  ([internal/db/db_signals.go](../internal/db/db_signals.go) holds an instance id, not a
  definition — either look the definition up or denormalize the level onto the row).

## 9. Reproducing

    pg_test_fsync -s 2                    # this disk's real fsync cost

    # SQLite, honest; add GENROC_DURABILITY=… and run bench-iterate too (§5a)
    GENROC_SQLITE_SYNCHRONOUS=FULL GENROC_SQLITE_FULLFSYNC=1 make bench-drain

    # Postgres, honest: a native cluster (Docker cannot, §1), with in postgresql.conf
    #   wal_sync_method = fsync_writethrough   max_connections = 400 (> --pg-max-open-conns)
    psql … -c "select pg_stat_reset_shared('io')"          # PG 18; PG <= 17: 'wal'
    POSTGRES_DSN=… GENROC_PG_COMMIT_DELAY=500 GENROC_PG_MAX_OPEN_CONNS=200 make bench-drain
    psql … -c "select sum(fsyncs) from pg_stat_io where object='wal'"   # PG <= 17: pg_stat_wal.wal_sync

**Interleave the A/B and take a median**: the same config measured 1.33× and 1.03× in two
sessions, and the variance is in the baseline. `bench-deep` is the control — `commit_siblings`
gates the delay off there, so it must show no change.

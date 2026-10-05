# Instance id lists: what a group of pause/resume/cancel/retry means

Status: **Built.** Code: `eachInstance`/`instanceIDsAndFlags` in `cmd/genctl`, `assert` in
`http.go`, `LifecycleResult` in `db_lifecycle.go`, `Reply.Outcome`/`statusOfOutcome`/
`actionDef.AltSuccess` in `internal/api`.

`genctl pause`, `resume`, `cancel` and `retry` take several instance ids, iterating client-side
like `upgrade`, and the endpoints behind them are assertions rather than 409-on-no-op (§The API
change). The verbs' own semantics are [pause-resume.md](pause-resume.md)'s.

## Motivation

`upgrade <id> ...` can be casually best-effort because an upgrade is idempotent: a partial sweep is
repaired by running it again. A group of verbs that refused a no-op would not converge — `pause a
b c` where `b` fails leaves `a` and `c` paused, and re-running the line then fails on `a` and `c`.
The group needs a taxonomy finer than succeeded/failed, one under which re-running converges.

## The model

**Exit 0 is a promise about every id named**, stated per verb, and it generates everything below:

| verb | exit 0 promises |
|---|---|
| `pause` | every tree you named has been asked to stop and will start no new work (a task already in flight still finishes — see §202) |
| `resume` | every tree you named is advancing |
| `cancel` | every tree you named has been asked to stop for good, or had already settled |
| `retry` | every tree you named got a fresh attempt |

Each id lands in one of three outcomes; only the third fails the command:

- **done** — the state changed (`applied`, or `accepted` where a task in flight defers it; the
  line then adds "(draining a task already in flight)").
- **already** — the verb's promise already held. Reported, never fatal.
- **refused** — the promise does not hold and the operator has a decision to make.

`already` is what restores convergence: re-running a partially applied line exits 0.

## The decisions

1. **The group is the primitive; the single command is `N=1`.** One code path, so the two cannot
   drift. The per-id line prints for every N, the summary only for `N>1`.

2. **`already` means the verb's promise holds — not "close enough".** For `pause` a `completed`
   tree counts (it is not advancing and never will); for `resume` it does not (it is not advancing
   either). The asymmetry is the point. Rejected: every conflict a refusal — it re-creates the
   non-convergence and trains the operator to ignore exit 1.

3. **Classify on the outcome, never on prose.** `done` versus `already` is `Outcome`, which the HTTP
   status renders.
   Every error is a refusal. Nothing may key on a message: a reworded server string must not be
   able to reclassify an outcome.

4. **`already` is not an error, so the API does not return one** (§The API change). It is a fact
   only the server holds, under the lock that decided the outcome. Rejected: the CLI classifying a
   409 by re-reading the row — it rebuilds a judgement the server already made, from state that
   has since moved.

5. **`not_found`, not-a-root (`invalid`), transport and internal errors are `refused`** for every
   verb: mistakes in the command, not answers from the state.

6. **The group is N transactions and never claims otherwise.** Ids are acted on in the order
   named; a refusal does not stop the ones after it; nothing is rolled back. No batch endpoint:
   five unrelated trees are five logical changes, each already taking `FOR UPDATE` over a whole
   subtree in id order, and holding N of those is unbounded lock-holding plus a deadlock surface
   (the reason `upgrade`'s sweep is client-side too). Contrast `applyBatch`, which earns its
   endpoint because an apply **is** one logical change (`internal/api/CLAUDE.md`).

7. **Duplicates are not deduplicated and `@last` may appear among the ids.** Each positional
   resolves on its own; `pause a a` is `done` then `already`.

8. **The single-read commands refuse a second positional.** `get` and `logs` reject `get a b`
   rather than acting on `a` — a silently dropped id reads as if it had been shown. `signal` stays
   single (it addresses one instance's `--task`).

9. **`--force` applies to every id in a `retry` group**, like `rm -f a b c`.

10. **A malformed argument aborts the whole command; a refusal does not.** Every positional is
    shape-checked against `isInstanceRef` before the first call, so a table substituted in where
    ids were meant touches nothing (instead of pausing whichever cell parses as an id). What can
    be known without asking the server must not be discovered halfway through mutating. The shape
    test accepts minted ids (an opaque digit-led token) and legacy UUIDs; the leading DIGIT keeps a
    process name from matching where a positional may be either
    (`TestIsInstanceRefAcceptsBothMintedAndLegacyIDs`).

11. **The list a group is fed from names only what the group can act on.** `instances` lists
    **roots only** (`children=true` opts in), since every one of these verbs is root-only. `-q`
    prints bare ids, so nothing but ids may reach its stdout: an empty list prints **nothing**, and
    the cap notice stays on stderr (a silently truncated list would pause 20 of 50). Every row
    carries `parent_id`.

## Surface

```
stdout  paused: <id>                                    # done — unchanged from today
stdout  already: <id>                                   # a 204 carries no message
stderr  genctl: <id>: <reason>                          # refused
stderr  5 named: 3 paused, 1 already, 1 refused         # summary, N>1 only
```

Refusals and the summary go to stderr, so `2>/dev/null` yields only the ids that are fine. Exit is
**0 iff no id was refused**, never a count. `already` writes nothing server-side, so it leaves no
audit entry.

## The API change

Pause, resume and cancel are **assertions** — "make this tree paused". An assertion that already
holds is a success that changed nothing, so returning 409 for it is a modelling error. `retry` is
not an assertion (nothing you already are is "given a fresh attempt"), so its refusals stay 409 —
the same asymmetry [pause-resume.md](pause-resume.md) §1 gives for never re-merging the verbs.
A retried POST after a network blip is therefore idempotent for every client, not just genctl.

### The outcome belongs on `Reply`, and the status is derived from it

`Reply.Code` carries the failure classification in the body; the HTTP status is derived from it
(`statusOf`). The success half mirrors it: `Reply.Outcome`, mapped by `statusOfOutcome`, so the
handler decides the outcome and HTTP only renders it. `actionDef.AltSuccess` documents the extra
statuses in OpenAPI.

| outcome | HTTP | when |
|---|---|---|
| `applied` | **200** | the call made the assertion hold (`pause`/`cancel` settled the whole tree, `resume` flipped it, `retry` revived it) |
| `accepted` | **202** | `pause`/`cancel` left rows draining — a worker holds a task |
| `unchanged` | **204** | the assertion already held; nothing was written (no audit entry, no `updated_at` bump) |
| *(refused)* | **409** | via `Code` — `resume` on a settled tree, every `retry` refusal |

A 200/202 body is `{"outcome", "status", "instances"}` — the root's status after the call and the
rows this call wrote, both already computed. A 204 has no body.

### 202 — asked is not stopped

`pause` cannot promise synchronously that a tree has stopped: a leased row goes to `pausing` and
lands only when the worker's own write releases the lease. A second `pause` on a draining tree
selects nothing (`status = 'running'` only), so without `CountDrainingInTree` it would report
`unchanged` while a worker is still inside a task. Hence the promise "asked to stop and will start
no new work", and 202 to tell asked-and-stopped from asked-and-draining. `cancel` works the same
way. `resume` never returns 202 (a resume is atomic — pause-resume.md §7), and neither does
`retry`.

### The 204 trade

HTTP has no status meaning "already in the desired state"; 204 means *no content*, and using it
for `unchanged` is convention, not standard. It costs the body, so an HTTP client learns
`unchanged` but not which already-state (paused, or settled); `GET /instances/{id}` answers that.
The alternative, `200 {"outcome": ...}` for both, gives up switching on the status line.

### Which 409s survive

| verb and case | status |
|---|---|
| `pause`, nothing running in the tree | **204** |
| `pause`, some rows left draining | **202** |
| `resume`, tree is live (`running`, `failing`) | **204** |
| `resume`, tree is settled (`Status.Terminal()`) | **409** — retry it, or start a new instance |
| `cancel`, nothing live in the tree | **204** |
| `retry`, any non-`failed` status, or `only_once` without `force` | **409** |
| any verb: unknown id, non-root id | 404 / 400 |

The `resume` split is decided inside `ResumeProcess` under the lock that read the tree — from the
same snapshot as the outcome, which no client re-reading after the fact can reproduce.

The taxonomy is the standard one: an object that does not exist is an error by default (`rm`
without `-f`), and one already in the target state is success (`systemctl stop` on a stopped
unit). Pause/resume/cancel are that assertion; `retry` has no counterpart there.

## Coverage

Pinned: convergence of a re-run partial group, a refusal between two workable ids, every row of
§The API change's tables at both the endpoint and the CLI (`resume` on a live tree against a
settled one carries the split), `unchanged` writing nothing, a second `pause` on a draining tree
being `accepted`, `get a b`/`logs a b` refused, a malformed list mutating nothing, `-q` printing
nothing for an empty list, and the default listing excluding children. Tests:
`tests/cli/instances_test.ts`, `tests/integration/lifecycle_outcomes_test.ts`,
`tests/tick/tree_pause_test.ts`, `tests/tick/pause_retry_test.ts`,
`internal/db/dbtest/pause_retry_test.go`.

## Open

- `--json`: `{id, outcome, reason}` per id; until then the per-id lines and exit code are the
  contract. Trigger: the first script that branches per id rather than on the aggregate.

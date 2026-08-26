# Instance id lists: what a group of pause/resume/retry means

Status: **agreed and implemented 2026-08-26.** Code: `eachInstance`/`instanceIDsAndFlags`
in `cmd/genctl`, `assert` in `http.go`, `LifecycleResult` in `db_lifecycle.go`,
`Reply.Outcome`/`statusOfOutcome`/`actionDef.AltSuccess` in `internal/api`.

`genctl pause`, `resume` and `retry` accept several instance ids, the way `upgrade`
already does — and the endpoints behind them became assertions rather than 409-on-no-op
(§The API change), which is what the id list turned out to need. The verbs' own semantics
are unchanged: their design is [pause-resume.md](pause-resume.md).

## Motivation

`upgrade <id> [<id> ...]` sweeps client-side, one atomic call per tree, and gets to be
casually best-effort because an upgrade is **idempotent**: its header comment says so, and
its tally reports "already on N" without counting it against the exit code, so *a partial
sweep is repaired by running it again*.

pause/resume/retry have the opposite property. All three **deliberately refuse a no-op** —
`db_lifecycle.go:255` says it outright: *"Report it rather than silently succeeding."*
So the naive group inherits a trap: `pause a b c` where `b` fails leaves `a` and `c`
paused, and re-running the same line now fails on `a` and `c` and succeeds on `b`. The
repair story that makes a best-effort group forgivable does not exist.

Two things follow. The group needs a failure taxonomy finer than succeeded/failed, and it
has to be one that makes re-running the same line converge.

## The model

**Exit 0 is a promise about every id named.** That promise is stated per verb, and it is
the thing that generates everything below:

| verb | exit 0 promises |
|---|---|
| `pause` | every tree you named has been asked to stop and will start no new work (a task already in flight still finishes — see §202) |
| `resume` | every tree you named is advancing |
| `retry` | every tree you named got a fresh attempt |

Each id lands in one of three outcomes. Only the third fails the command:

- **done** — the call succeeded and the state changed (`applied`, or `accepted` where a
  task in flight defers it; the `already` line says which).
- **already** — the verb's promise already held for this id. Reported, never fatal.
- **refused** — the promise does not hold and the operator has a decision to make.

`already` is what restores convergence: re-running a partially applied line exits 0,
because the ids that landed the first time now report `already`.

## The decisions

1. **The group is the primitive; the single command is `N=1`.** One code path, so the two
   cannot drift. The per-id line is printed for every N and the summary only for `N>1`, so
   at `N=1` output is byte-identical to today's success case. The one deliberate change at
   `N=1`: an `already` outcome now exits 0 where today it exits 1.

2. **`already` means the verb's promise holds — not "close enough".** For `pause` a
   `completed` tree counts, because it is not advancing and never will; for `resume` it
   does not, because it is not advancing either. The asymmetry is real and forcing symmetry
   is the error. The alternative — every conflict is a refusal — was rejected because it
   re-creates the non-convergence above and trains the operator to ignore exit 1, which is
   strictly worse than occasionally under-reporting.

3. **Classify on the outcome, never on prose.** `done` versus `already` is `Outcome` —
   readable as the HTTP status by clients that have one, and as a body field by the TCP and
   UDS transports that do not; a refusal is classified by the error `code`, which the wire
   already carries (`server.go:218`) and which genctl currently decodes and discards,
   keeping only `error`. Nothing may key on a message: a reworded server string must not be
   able to reclassify an outcome, and a rule reading `(status: paused)` out of a sentence
   would do exactly that.

4. **`already` is not an error, so the API must stop returning one.** See §The API change.
   The classification the CLI would otherwise reconstruct is a fact only the server holds,
   and it holds it under the lock that decided the outcome; `changed` on a 200 is that fact.
   An earlier draft had the CLI classify `resume`'s 409 by re-reading the row — rejected,
   because it makes the client rebuild a judgement the server already made and re-read state
   that has since moved on.

5. **`not_found`, not-a-root (`invalid`), transport and internal errors are `refused` for
   all three verbs.** These are mistakes in the command, not answers from the state: a
   typo'd id, a child id where the root was meant, a server that is not there.

6. **The group is N transactions and never claims otherwise.** Ids are acted on in the
   order named; a refusal does not stop the ones after it; nothing is rolled back. This is
   why no batch endpoint is proposed: pausing five unrelated trees is five logical changes,
   not one, and each already takes `FOR UPDATE` over a whole subtree in id order — holding
   N of those open is unbounded lock-holding plus a deadlock surface, the same reason
   `upgrade`'s sweep is client-side. A batch endpoint would be this loop moved server-side,
   buying one round trip on a kept-alive connection and costing a partial-success wire
   shape that has to stay true forever. Contrast `applyBatch`, which earns its endpoint
   because an apply **is** one logical change (`internal/api/CLAUDE.md`).

7. **Duplicates are not deduplicated and `@last` may appear among the ids.** Each
   positional resolves on its own; `pause a a` is `done` then `already`, which is honest.

8. **The single-read commands refuse a second positional.** `get` and `logs` take exactly
   one id and must reject `get a b` rather than acting on `a` — an id that is silently
   dropped reads as if it had been shown, and an id list is now a thing operators type.
   `signal` stays single (it addresses one instance's `--task`), and `compat` already
   documents why it takes one.

9. **`--force` applies to every id in a `retry` group**, like `rm -f a b c`. There is no
   per-id form.

## Surface

```
stdout  paused: <id>                                    # done — unchanged from today
stdout  already: <id>  <the server's message>           # e.g. "... (status: paused)"
stderr  genctl: <id>: <reason>                          # refused
stderr  5 named: 3 paused, 1 already, 1 refused         # summary, N>1 only
```

Refusals go to stderr so `genctl pause $ids >/dev/null` still shows what went wrong and
`2>/dev/null` yields only the ids that are fine; it also keeps the `N=1` failure path on
stderr, where it is today. Exit is **0 iff no id was refused**, never a count.

`already` changes nothing server-side, so it writes no audit entry — do not look for one.

## The API change

The whole model above rests on pause and resume being **assertions** — "make this tree
paused", "make this tree advance". An assertion that already holds is a success that
changed nothing. Returning 409 for it is the modelling error that forced every workaround
in the drafts of this spec, so the improvement is not a finer error code; it is noticing
that `already` was never an error.

`retry` is untouched: it is not an assertion (nothing you can already be is "given a fresh
attempt"), so every one of its refusals stays a 409. That the wire shapes now differ is the
same asymmetry [pause-resume.md](pause-resume.md) §1 records as the reason the verbs must
never re-merge — it just becomes visible from outside.

### The outcome belongs on `Reply`, and the status is derived from it

The obvious move — "return a different HTTP status" — is the wrong shape here, and the
codebase already says why. `Reply.Code` carries the failure classification *in the body*,
and its comment gives the reason: **"it is on Reply, not on the HTTP response alone,
because TCP and UDS clients encode Reply directly and have no status line to read"**
(`handlers.go:51-58`). The HTTP status is then derived from it by one table (`statusOf`,
`errors.go:58`). A success outcome expressed only as a status code would be invisible to
two of the three transports.

So the success half mirrors the failure half already there:

```go
type Outcome string   // on Reply, beside Code
    OutcomeApplied   = "applied"     // the assertion holds, and this call made it hold
    OutcomeAccepted  = "accepted"    // accepted, not yet in effect
    OutcomeUnchanged = "unchanged"   // it already held; nothing was written
```

| outcome | HTTP | when |
|---|---|---|
| `applied` | **200** | `pause` settled the whole tree; `resume` flipped it; `retry` revived it |
| `accepted` | **202** | `pause` left rows `pausing` — a worker holds a task and the tree has not stopped yet |
| `unchanged` | **204** | the assertion already held |
| *(refused)* | **409** | via the existing `Code` path — `resume` on a settled tree, every `retry` refusal |

A client that only reads the status line gets the answer; a TCP or UDS client reads
`Outcome` and gets the same one. Today there is no path for this at all: `writeReply`
writes the implicit 200 on every success (`server.go:226-234`), so `Reply` gains the field
and the success branch gains a `WriteHeader`.

### 202 is not decoration — it fixes an error in this spec

An earlier draft of §The model said `pause`'s promise is "no tree you named is advancing".
**That promise cannot be kept synchronously and never could.** A leased row goes to
`pausing`, not `paused`, and lands only when the worker's own write releases the lease —
the endpoint summary says so outright ("takes effect at the next task boundary, so a task
already executing runs to completion", `actions.go:489`). So a `pause` that returns 200
today may leave a task running, and a second `pause` on a draining tree selects nothing
(`status = 'running'` only, `db_lifecycle.go:230`) and would have reported `unchanged` —
claiming the tree had stopped while a worker was still inside a task.

`pause`'s promise is therefore **"every tree you named has been asked to stop and will
start no new work"**, and `accepted`/202 is what distinguishes asked-and-stopped from
asked-and-draining. `resume` never returns it (a resume is atomic — pause-resume.md §7),
and neither does `retry`; the asymmetry is real, not an accident of implementation.

### The honest wrinkle

**HTTP has no status code meaning "already in the desired state".** 204's actual meaning is
about the *body* — no content — not about change; using it for `unchanged` is a widespread
convention, not a standard, and it costs the response body, so an HTTP client learns
`unchanged` but not *which* already-state (already paused, or settled). `Outcome` still
carries it over TCP and UDS, and `GET /instances/{id}` answers it for anyone who cares.
That detail is the Known gap below, not a new loss. The fallback, if that detail matters
more than switching on the status line, is `200 {"outcome": ...}` for both — which is what
the status-code requirement exists to avoid.

### The rest of the shape

The constant `{"paused": true}` is dropped: it was `true` on every success and carried
nothing. What replaces it, on the two outcomes that have a body:

```
200 / 202  {"status": "pausing", "instances": 4}
204        (no body)
```

`status` is the root's status after the call and `instances` the number of rows this call
wrote — both already computed and already logged (`logTreeAction`, `db_lifecycle.go:301`),
so neither costs a query. `outcome` appears in the JSON too, for the transports with no
status line.

### Which 409s survive

| verb | today | proposed |
|---|---|---|
| `pause`, nothing running in the tree | 409 | **204** — the promise holds |
| `pause`, some rows left draining | 200 | **202** — asked, not yet stopped |
| `resume`, tree is live (`running`, `failing`) | 409 | **204** — the promise holds |
| `resume`, tree is settled (`Status.Terminal()`) | 409 | **409** — it cannot hold; retry it or start a new instance |
| `retry`, any non-`failed` status, or `only_once` without `force` | 409 | **409** |
| any verb: unknown id, non-root id | 404 / 400 | unchanged |

The `resume` split is on `Status.Terminal()` (`model/instance.go:46`), evaluated inside
`ResumeProcess` under the lock that already read the tree — decided from the same snapshot
as the outcome itself. That is the part a client cannot reproduce at any price: a CLI
re-reading after a 409 is reading a tree that may have moved.

**"Report it rather than silently succeeding"** (`db_lifecycle.go:255`) is the comment this
appears to contradict, and does not. Its concern is silence — the original sin was a plain
success that hid the no-op. A distinct outcome and status code report it louder than the
409 did and, unlike prose, a program can switch on it. The comment has to be rewritten when
this lands, or it will read as an invariant this broke.

### Cost

`PauseProcess` and `ResumeProcess` return `(outcome, err)` rather than `error`; the API is
their only caller (`handlers_instances.go:262-293`). `Reply` gains `Outcome` and
`writeReply` a success-side `WriteHeader`. The registry's `Resp` is **one shape per action**
(`actions.go:29`), so documenting three success codes is the one genuinely new capability
here — everything else follows an existing pattern. Then `openapi.json` and
`tests/generated/api.ts` regen. Test churn is mostly inversion:
`TestPauseProcess_NothingRunning` asserts `unchanged` instead of an error;
`api_errors_test.ts`'s resume case resumes a *completed* process, so it stays 409 untouched.

**What it buys beyond this CLI.** Pause and resume become idempotent for every client: a
retried POST after a network blip stops turning into a spurious conflict, and any
automation asserting "this tree is paused" can stop special-casing 409 — while 202 finally
makes the drain visible, which `{"paused": true}` never could.

## Prior art

Nothing above is invented; the taxonomy is the mainstream split, and it is worth naming
which tool each half comes from because that is what makes an operator's intuition
transfer.

**Best-effort iteration with an aggregate exit code** — decisions 1, 5, 6 and the surface.
`rm a missing b` removes `a` and `b`, prints one error on stderr and exits 1; `git branch
-d b1 nope b2` deletes both real branches, reports `nope`, exits 1. `kubectl delete pod a b
c` is the same shape with a per-item line on stdout. None of them abort at the first
failure, none is atomic, and all report per item and fail in aggregate.

**Where "already in the desired state" is success, and where it is not** — decision 2. The
established tools split this exactly where this spec does, along the same line:

| the case | convention | here |
|---|---|---|
| the object does not exist | an error by default; leniency only behind a flag (`rm -f`, `kubectl delete --ignore-not-found`) | `not_found` is always `refused` (decision 5); no flag is proposed |
| the object exists and already holds the target state | success, by default, no flag: `systemctl stop` on a stopped unit and `docker stop` on a stopped container both exit 0 | `already` (decision 2) |

`systemctl start`/`stop` is the closest analogue there is — units are asserted into a
state, already-there is success, and an unknown unit still fails. That is `resume`/`pause`.
`retry` has no counterpart in that family precisely because it is not an assertion, which
is the same reason it must never re-merge with `resume` (see
[pause-resume.md](pause-resume.md) §1).

**Three outcomes rather than two** — Ansible reports `ok / changed / failed` and Terraform
plans in the same triple; `already / done / refused` is that shape, and it is where the
summary line comes from. Most single-purpose CLIs print no summary at all, which is why
this one appears only for `N>1`.

**Nothing here diverges, once §The API change lands.** It is worth recording why the
divergence existed: an early draft had the CLI re-read the row to classify `resume`'s 409,
which no tool above does — their APIs return errors granular enough to switch on. That the
client needed a workaround at all was the signal the API was wrong, not the client. The
same is true of the tools cited: `systemctl stop` on a stopped unit exits 0 because systemd
models the call as an assertion, not because `systemctl` post-processes a failure.

## Known gaps

- **No `--json`.** The per-id lines and the exit code are the contract. The trigger to add
  one is the first script that needs to branch per id rather than on the aggregate; the
  shape would be `{id, outcome, reason}` per the `--json`-is-the-one-machine-format rule.
- **`pause` treats a settled tree and an already-paused one as the same outcome** — both
  are `already`, because the promise is "not advancing" and both keep it. They are still
  distinguishable when it matters: the response's `status` says which, and the `already`
  line prints it. What is deliberately *not* offered is a way to make settled fatal.
- **No selector sweep.** `pause --status running --process foo` is a different feature with
  `upgrade`'s sweep shape (list, then loop) and is out of scope here.
- The summary line is more explicit than `upgrade`'s existing tally ("moved 2 tree(s), 1
  already there"). `upgrade` may adopt this form later; nothing here requires it.

## Coverage

What the tests must pin, none of which today's suite would catch:

- **Convergence.** A partially applied group, re-run verbatim, exits 0 — the property the
  whole taxonomy exists for.
- **A refusal named between two workable ids** fails neither of them and still carries the
  exit code (the shape `upgrade_test.ts` already uses).
- **Every row of §The API change's table**, at both layers: that the endpoint answers 200
  `changed:false` versus 409 where the table says so, and that the CLI turns those into
  `already` (exit 0) versus `refused` (exit 1). `resume` on a live tree against `resume` on
  a settled one is the pair that carries the whole split.
- **`unchanged` is returned exactly when nothing was written** — it must leave no audit
  entry and no `updated_at` bump, or the outcome is decorative.
- **A second `pause` on a draining tree is `accepted`, not `unchanged`** — the case §202
  exists for, and the one a `status = 'running'` selector gets wrong.
- **`N=1` identity**: the success line and the stderr failure path unchanged from today.
- **`get a b` and `logs a b` are refused**, not silently truncated to `a`.

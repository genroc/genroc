# Child → parent error handling

Status: **Built.** Fault payloads are designed in [error-extensions.md](error-extensions.md) §X2-c.

## 0. Governing principle

> **An error is a branch slot, not a value.**

A raise says: *an anticipated condition prevents me from finishing, and my parent may react.*
It carries a code (to branch on), a message (to read), and — where the caller declared that
code's shape — a payload (I6). A *successful* child's value belongs in `output`.

- **Anything unanticipated panics.** No catchable form, no wildcard: making a failure reactable
  means converting it to a raise *inside* the child, at the task that understands it (§5.4).
- **Only a declared condition can be retried around.** A child task's `on_error` carries
  `retry` like any task's (R4), but a rule names only codes in `raises(D)` (R5), and a defect is
  never catchable (D5).

Three terminal clauses cover the outcome space once each: `goto: end` → `completed`;
`raise: {code, message, data?}` → `raised`, which the caller reacts to by naming the code;
`panic: {code, message, data?}` → `failed`, which nothing can react to. A raised code is read
inside the tree by a branching parent, a panic code outside it by dashboards. "8 of 10 shipped
and why" is a result, so it goes in `output` (`{ok: false, reason}`), not in control flow.

## 1. Vocabulary

**batch** — children of one `(parent_id, spawn_task_id, parent_task_epoch)`; **slot** — a
stable batch position, surfaced as `child_key` (`child_map`) or `child_index` (`child_list`);
**raise/panic** — termination via the clause → `raised`/`failed`; **defect** — any `failed`,
authored or engine, never catchable; **raise set** — `raises(D)` (§2.3); **resolution** — the
parent's decision over a settled batch (§5.2).

## 2. Surface syntax

### 2.1 `raise` — a clause, not a goto

A field, so code, message and data travel together. One `Fault` type serves raise and panic —
they differ in what they do, not what they carry — and the distinction lives at the use site
(`Raise *Fault` / `Panic *Fault`). Valid on `SwitchCase` and `ErrorCase`, exclusive with `goto`
(R3). `message` is a required template; `data` an optional expression or object of expressions;
both are evaluated in the clause's own scope **before** it concludes, so a fault reached through
`on_error` can read the `error` it is handling. The code is a literal (R2).

They fail differently on purpose: a message that will not render **degrades to its raw
template** (with a warning), while `data` that will not evaluate **fails the instance** with
`engine.expression` — the payload is a contract, and dropping it would surface the loss at the
caller's conform instead of at its origin.

### 2.2 `panic` — authoring a defect

Terminates as `failed`: *something I did not anticipate; nothing downstream should work around
it.* The code is for **classification** — `error_code` is filterable, so
`submit_contract_violation` can be alerted on. The canonical case is one HTTP cannot see: a
`200` with an error body passes `accepted_status`, and only the author knows whether that body
is anticipated (`raise`) or a broken contract (`panic`). Named "panic" so it reads as alarming:
it is uncatchable and takes the tree down. Its `data` reaches only an operator.

The clause also decides **who may re-run the work**: a raise invites the parent to retry, so a
raised child is re-spawned whole (§5.5, §12); a panic invites nothing, so a failed child is
revived *in place*, its completed tasks — `only_once` included — never redone, and a panic
standing at an `only_once` task refuses retry without `force`.

### 2.3 The raise set is inferred

No `errors:` block. `raises(D)` is a syntactic scan of `raise` clauses — exact, imprecise only
in the safe direction (an unreachable raise inflates it). **Panic codes are excluded**: no rule
can ever match a panic, since a panicking child poisons its ancestors and the parent never
resolves. `raises(D)` is published on the definition listing.

### 2.4 Parent side — no prefix, no new syntax

On a child task, `on_error` codes need no `child.` namespace because there is little else they
can see: input validation, definition lookup and spawn go straight to `failInstance` (E6), and
collect corruption stays `engine.collect`. The catchable set is `raises(D) ∪ {result.invalid}` —
the one engine code being a caller's narrowing bet that lost (error-extensions.md §X2-c). R1
forbids `.` in raised codes and every engine code has one, so a raised code is always an
authored name. **Propagation is explicit**: a parent re-raises with a `raise` in an `on_error`
rule, which puts the new code into *its* raise set.

## 3. Static semantics (registration)

- **R1 — fault shape.** `Code` matches `^[a-z][a-z0-9_]*$`; `Message` non-empty; `Data`
  optional. `.` is reserved for engine codes, so re-raising a system code is refused by
  construction; `%` is the match wildcard, so no code needs escaping in a pattern.
- **R2 — the code is static.** An expression would make `raises(D)` uncomputable and
  `error_code` unqueryable. R1's regex admits no expression, so R1's shape *is* R2. The message
  is a template, type-checked to a **non-null string**.
- **R3 — one terminal clause.** A `SwitchCase` carries exactly one of `goto`/`raise`/`panic`;
  an `ErrorCase` **at most** one (a verb-less rule exhausts retries, then fails with the
  engine's code). Checked in the validator, not the decoder, so the rejection names task and
  case index.
- **R4 — `retry` is allowed on a child task; `not_reached` is not.** Retrying re-spawns the
  raised slots (§5.5). `not_reached` is refused because every code a child task catches means
  the child ran — so `retry` on an `only_once` child task is refused at registration too,
  rather than left for `isRetryAllowed` to drop at runtime (D7). `only_once` *inside* the child
  is a different position, deliberately unguarded (§12). Codes are patterns, safe because R5
  bounds them to a finite raise set.
- **R5 — rule reachability.** Every pattern must match some code in `⋃ raises(D)` over the
  task's resolved children (plus `result.invalid`); catch-alls exempt. Its own pass, because
  reachability is a property of the rule set against the union, not of one entry.
- **R6 — a code is a raise code or a panic code, never both.** Otherwise `error_code` means two
  things on one process, for exactly the observers it serves.
- **R7 — a declared payload must be one the code can carry.** `raises[code]` is checked against
  the payload type the child's raise clauses produce with `NarrowsTo`, sound because collect
  conforms it. Two clauses on one code union; a clause attaching nothing types as `null`.
  specs/error-extensions.md §X2-c.

### 3.1 What R5 does and does not catch

One direction only, rule → raise set: typos are caught, a rule orphaned by a removed code is
caught on re-registration, but **a code added with no rule surfaces at runtime** — the
deliberate cost (an unhandled raise fails the parent, §5.2). Version pinning bounds the blast
radius: a new code reaches a parent only after a deliberate dependency bump. Requiring coverage
is rejected (D3).

## 4. Matching

**M1.** A rule matches iff one of its `code` patterns matches (empty list = catch-all), through
the same `errcode.MatchCode` as action tasks. **`%` is the only wildcard**; `_` and `.` are
literal — SQL-`LIKE`'s `_` would be a silent single-character wildcard in codes full of
underscores.

**M2 — `case`, a predicate on the matched error.** A rule may carry a `case` beside its `code`;
it matches only when both hold, and a false case falls through to the next rule — `on_error` is
a switch over errors, and this is `SwitchCase.Case` on that channel.

**Why: it removes a handler task.** Classifying in a handler leaves the instance standing on
the *handler*, past the batch, where retry cannot reach the child (§11.1). Deciding at the rule
leaves it on the task that failed, where `retry` acts. The alternative — letting a retry budget
separate transient from permanent by observation — spends attempts on failures already known
to be permanent.

**The prefilter types it.** Inside a rule naming `code: ["x"]`, `error.data` is exactly
`raises.x` — the same union-across-patterns inference the routed task gets, applied one step
earlier. Reading `error.data` for a code the call never declared is a registration error.

- **Fall-through, or the feature is inert.** Code-matched-but-case-false tries the next rule.
- **A guarded rule is never a catch-all.** `{code: [], case: X}` is not total, so it neither
  satisfies the catch-all-last requirement nor is forced last.
- **Naming a code no longer guarantees it is handled** — with a case it is only *considered*.
  Nothing static can warn; an unmatched raise still degrades to a defect (§5.2).
- **The error is bound, not written.** The case sees the routed task's scope plus `error` — the
  failure this rule caught — and no `self`. The payload conform runs first (it can replace the
  code, which decides the rule), but nothing is persisted, so a non-matching rule leaves no trace.
- **A failed evaluation fails the instance** (`engine.expression`): it was type-checked at
  registration, so a runtime failure is a broken guarantee, not a non-match.
- **It is an expression, not a template** (`Expr: true`): as a template it renders to a string
  and silently takes every guarded rule out of play. A list under `case` is refused by shape,
  hinting at `code`.

R5 is untouched: reachability stays a question about codes. Per-slot admission (§5.5) evaluates
the case per slot, against that slot's own error.

## 5. Operational semantics

### 5.1 Child: raising

`raise` and `panic` write identical fields (`error_code`, `error_message`, `error_data`,
`wake_at := nil`) and differ only in status, which decides whether ancestors are poisoned. Panic
is `failInstance` with authored words (→ `FailInstanceAndAncestors`); raise writes
`StatusRaised` and falls through to `FinishChild`, being a normal outcome. `raised` is directly
terminal — a raise happens at a task boundary, after the child's own children collected — and
is in `Status.Terminal()` (§11.4). **Neither computes process `output`**; registration agrees
for free, since the output-boundary analysis keys on `goto: end`, which a raise case never has.

### 5.2 Parent: resolution

Precondition: `running ∧ collecting` (`failing` → `settleFailing`, `pausing` → `settlePausing`;
`paused` is unclaimable and resolves on resume). So **a resolving parent's batch holds only
`completed` and `raised` children** — a failure poisoned it first (§5.4), a paused child holds
it in `children` (`CountActiveSiblings`).

```
E := raised children in slot order
E = ∅        → collect outputs, continue          (happy path)
otherwise    → admit retries (§5.5)
               any slot re-spawned → park on 'children'; no error, no route
               else f := E[0]; write `last_error` from f (§5.3); match f's rule:
                    nil or verb-less → fail P   ·  goto:end → complete P
                    raise/panic      → as §5.1  ·  goto:$id → P.task := id
```

**`last_error` is written only when the batch is done retrying**: a parent on a backoff carries
none, as on the action path. What reaches an operator is the raise that *ended* the batch. The
payload conform still runs ahead of the rules, since it can replace the code.

Deterministic — no clock, no completion order (I3). Resolution sits ahead of the collect, so
`buildChildOutput` keeps its strict every-child-completed guard. The unhandled branch names
code, child and slot, and the parent's `error_code` becomes the child's raised code, not
`engine.collect`: **an unhandled raise degrades to a defect**, and propagation is never
implicit. `goto: end` completes through `completeViaErrorHandler`, shared with the action path
so the two cannot drift.

### 5.3 What the routed task sees

`{task, code, message, data?, child_key | child_index}`, with `data` present only where the
call declared that code's shape (I6). The slot is two single-typed fields rather than one
`string|integer`, so a handler never type-switches; exactly one is present, both optional in
the schema since an action task's `on_error` leaves them absent. **One raise is reported**, the
first in slot order (D2). The routed task keeps its context, minus `outputs.<T.id>`, which the
failed batch never produced.

### 5.4 Defects fail fast, always

A failed child — authored panic or engine fault — is uncatchable under any configuration:
`FailInstanceAndAncestors`, batch abandoned, parent settles without resolving. The only route to
catchability is a raise inside the child (`on_error: [http.503] → retry: 3 → raise:
psp_unavailable`). A defect in one slot dominates a sibling's raise: a fault must not be masked
by a business error beside it.

### 5.5 Retrying a child task

A rule matched on a raised slot may carry `retry`. **Each slot is a call with its own budget**:
at resolution every raised slot matches its own code (§4), and one whose rule carries `retry`
with its attempt count under the limit is **re-spawned** — superseded and replaced in the same
batch (§12). If any slot is, the parent returns to `children` and resolves again when the batch
settles; if none is, `raised[0]`'s rule routes as in §5.2. Completed siblings stand, so I1 holds.

The count is `_spawn_attempt`, in the child's `_spawn_*` bookkeeping — not on the parent, not
in a column. It starts at 0 and does not distinguish codes, so `attempt < limit` admits exactly
`limit` retries against whichever rule matched this round. The replacement's `wake_at` is
`updated_at + retryDelay(attempt, policy)`, measured from the **raised child's** conclusion: the
time it spent waiting on siblings already served what a backoff is for.

**A replacement's input is re-evaluated**, not copied: version re-resolved, input rebuilt and
re-validated against the parent as it now stands. "A retried call re-sends the same arguments"
is the plausible objection, and it is wrong here: a `$import`ed script is an input, and
publishing a new version is how a caller changes one — copying makes a fix undeliverable. The
batch is rebuilt once per round and indexed by slot. A slot the parent no longer declares (an
upgrade removed it, or `over` came back shorter) fails the instance rather than inventing an
input.

**The batch is the unit**: a slot is re-spawned when the batch settles, not when it raises.
That keeps §5.4 true — an eager retry would spend an attempt, and its side effects, on a batch a
sibling is about to poison. The cost: a slot cannot retry before its siblings settle, and a
raise no rule would retry does not route until they do.

Things that break silently:

- **The parent must not bump `task_epoch`.** The epoch is the batch identity; bumping it orphans
  the kept siblings (§12). (The action retry branch bumps it, for its external token.)
- **The parent's `retry_count` is not the budget** — entering a spawn task zeroes it. A count
  the parent never rewrites is what terminates the loop.
- **Every raised slot is conformed each round** (`admitRetries` → `slotError`): a payload
  failing its declaration replaces the code, and the code picks the rule.
- **Dispatch is `outcomeRespawn` → `RespawnSlotsAndWait`, not `SpawnChildrenAndWait`**, which
  refuses a parent whose phase is not `''`. Supersede, inserts and park are one transaction, or a
  crash leaves a slot with no occupant.
- **A superseded attempt keeps its subtree** — rows and object claims accumulate per attempt.

Budgets multiply: a child's own `retry: 3` under a parent's `retry: 3` is sixteen attempts at
the underlying call. A re-spawned child that panics ends the loop at once (§5.4). A round
audits one warn line per slot (slot, code, `attempt n/N`), written after the commit so it never
names a child that does not exist. Tests assert the **collected output**, not status: a batch
that collects silently empty still reads completed.

## 6. Invariants

- **I1 — all-or-nothing.** `outputs[T.id]` exists only when every child completed.
- **I2 — single observation.** A settled batch is resolved exactly once; with §5.5 a task
  resolves once per generation, each over its own rows.
- **I3 — determinism.** Resolution is a pure function of (T, slot-ordered children). Attempt
  counts ride those children, so the tuple stands.
- **I4 — crash safety.** From I3: a reclaimed parent re-resolving the same rows decides the same.
- **I5 — caller independence.** A child's terminal status and ancestor effects do not depend on
  who spawned it.
- **I6 — data crosses only where the caller declared it.** The parent reads a fault's `data` as
  `error.data` only where the call declares that code under `raises` — the error channel's
  `result_schema`, declared by the **caller** so a generic child stays generic. A payload that
  does not fit reports `result.invalid` in place of the raised code; R7 makes that reachable only
  by a payload registration cannot type. specs/error-extensions.md §X2-c.

## 7. Data model

`error_code TEXT NOT NULL DEFAULT ''` — one spelling of "no code"; filtered, never sorted, so no
index. The instance's error columns split **by direction** — the error it CAUGHT
(`error_internal`) and the error it REPORTS (`error_code`, `error_message`, `error_data`):
error-extensions.md §X2-c.

### 7.1 `error_code` discriminates every non-success outcome

`completed` → `''`; `raised` → the raised code; `failed` → the panic code or the engine code.
Two engine families: call codes (`http.500`, `pre.timeout`, `result.invalid`…) via
`handleCallError`, and the terminal `engine.*` set (`definition`, `expression`, `config`,
`input`, `output`, `spawn`, `collect`, `panic`) via `failInstance`, plus the one catchable
`only_once.interrupted` ([only-once-interrupted.md](only-once-interrupted.md)). Authored codes
never contain a dot; engine codes always do.

**Exactly one status predicate changes for `raised`**: `CountActiveSiblings` lists it as
settled, or the parent never wakes. `ClaimInstances` (whitelists live statuses), `FailAncestors`
(a settled `raised` row must not reopen into `failing`) and `WakeParent` need nothing — terminal
for "is the batch done", but not a failure: it neither poisons nor is poisoned.

### 7.2 The wire format is hand-written

`Fault`, `switch` cases and `ErrorCase` decode by hand, refusing unknown keys, and
`Action.JSONSchemaBytes` is a hand-written union whose variants set `additionalProperties:
false` — so a field added to a Go struct but not to its variant is refused at the edge by the
editor schema and the OpenAPI blob, far from the change. R3 lives in the validator so its
rejection is named rather than a decode error.

## 8. Edge cases

| # | case | resolution |
|---|---|---|
| E1 | child raises while tree paused | parent armed for `collecting` but unclaimable; decision deferred to resume |
| E1b | pause lands mid-resolution | routing write settles the pause via the `UpdateInstance` CASE; parent pauses already pointed at the goto target |
| E3 | raise + defect in one batch | defect wins; the raise is never routed (§5.4) |
| E6 | spawn-time failure | `failInstance`, never `on_error` — what makes §2.4 true |
| E8 | root raises | no parent; API reports `raised` + code |
| E9 | grandchild raises, child unhandled | child fails; never crosses two levels implicitly |
| E13 | handler routes into main flow | legal; `outputs[T.id]` absent — reads are a registration error |

## 9. Locked decisions

- **D1 — no `child.` prefix** (§2.4).
- **D2 — no `siblings`; `error` reports one raise.** The engine never routes on an aggregate,
  "6 of 10 failed and why" is a result (§0), and I1 makes a partial batch's successes
  uncollectable regardless.
- **D3 — reachability only, no exhaustiveness.** Coverage requirements make shared children
  painful, and the direction matters: adding exhaustiveness later breaks definitions, removing
  it breaks nothing.
- **D4 — `raised` is a distinct status.** Not `completed` (dashboards key on status), not
  `failed` (that means defect, and poisons). It also names the unit of retry (§12).
- **D5 — defects are never catchable** (§5.4). **D6 — `panic` carries a code for
  classification, not branching**: parents branch, API consumers classify (R6).
- **D7 — a child task retries** (§5.5). Re-spawning a raised child re-runs its upstream tasks,
  so it can decide differently, and the attempt count rides the child's `_spawn_*` bookkeeping,
  so the sibling queries gain nothing. A retry that can never fire is refused at registration
  rather than dropped at runtime (R4).

## 10. Re-running a batch

### 10.1 Re-running a batch without retry

A `goto` back to the spawning task re-spawns a fresh batch (the error route cleared `phase`):
every slot re-runs — wrong for `only_once` children, wasteful for fan-outs, and unbounded unless
the definition counts, since entering a spawn task zeroes `retry_count`. It remains the only way
to re-run *completed* slots; raised ones are §5.5's.

## 11. The `retry` command

`RetryProcess` is failed-only ([pause-resume.md](pause-resume.md)). One unit of retry, a
process: name the root, and `revive` walks down to what was interrupted, keeping settled work.

### 11.1 Retry re-runs the task the instance sits on — no rewind

For a **fault** that fits: the cause is at the task. For a **raise** it does not — the deciding
state is upstream and persisted, so re-running the task re-raises identically (and an action
that already succeeded would repeat a side effect). The line is fault vs outcome, which status
already encodes (D4). **Panics stay retryable**: fix outside, re-enter.

An error routed to a handler leaves the instance on the handler, so the failure that mattered
is behind it; M2 is the way out.

**Rejected: gating retry on "the task has an action".** `config` is live, so a switch-only panic
on `config.psp_enabled` retries meaningfully after an env flip, and a correct gate needs static
analysis of what an expression reads, on an operator-facing API.

### 11.2 `raised` is settled: never revived

In `revive`, `raised` joins `completed` ("settled, keep"), not the defensive live-status arm
("not ours to touch") — the two `return nil`s mean different things. A raised slot under a
failed parent is re-spawned instead (§12).

### 11.3 Clear the reported error slots on revive

`error_code`, `error_message` and `error_data` clear: a kept `error_code` corrupts the column it
exists for (a revived-then-completed instance still reports its old death). The caught
`error_internal` stays — a revived node can stand on a task reached through `on_error`, whose
context requires `last_error`.

### 11.4 `Status.Terminal()` must include `raised` — a live bug if missed

Revival reconstructs `children` vs `collecting` from "is anything still active?". If
`Terminal()` does not know the status, the parent parks in `children` forever, unrecoverable
and unlogged. Its SQL mirror is `CountActiveSiblings` (§7.1); the two edits travel together,
and either alone hangs a parent.

## 12. Re-spawn on the `retry` command

The operator's counterpart to §5.5 — same mechanism, different trigger. `retry` stays one verb
with no flag because the child's status is the intent: a `failed` child is retried from inside,
a `raised` child is re-spawned whole, since only re-running the upstream tasks can produce a
different decision.

**The operator bypasses the budget rather than resetting it**, by MARKING the parent: `revive`
leaves its raised slots alone and sets a one-shot `_retry_override`; the next collect admits
every raised slot whatever `_spawn_attempt` says, consults no rules (so **no backoff** — someone
asking wants it now), and clears it. The count still advances, so the fresh child runs once: if it raises
again, §5.5's admission declines and the batch routes or fails. The marker exists because the
re-spawn cannot happen in the db layer: a replacement's input is evaluated against the parent's
current definition (§5.5), and `internal/db` cannot evaluate an expression.

Shared with §5.5:

- **The parent keeps its `task_epoch`** — batch identity is `(parent_id, spawn_task_id,
  parent_task_epoch)`. Bumping it orphans the batch: a `child_map` merges `{}` and reports
  **completed**, a single `child` fails `engine.collect`, a raised sibling is never resolved. A
  node revived with no batch still bumps (it re-runs from the top, and an external token derives
  from the epoch), and the walk binds `parent_task_epoch` in its lookup, or a spawn task
  re-entered by a loop hands it two generations.
- **The replacement is rebuilt as phase 1 would build the slot**, placed under the old epoch,
  and written through the ordinary persist path — an input carrying object references needs its
  own claims declared.
- **The old row is superseded, not deleted** — `superseded_at`, filtered out of
  `GetChildrenForTask` and `revive`'s lookup, or `buildChildOutput` sees two rows for one slot
  and the stale raise routes again. `CountActiveSiblings` needs no predicate: a superseded row
  is `raised`, hence never active.

**`only_once` does not gate a re-spawn.** It bounds attempts within an instance, and a re-spawn
makes a new one — indistinguishable from starting the process again, or from §10.1's `goto`.
Protection is idempotency at the child's boundary; on the parent's own child task it still
refuses (R4).

**A raised root stays refused**: a raise is retried from its parent, and for a root "start a
new instance" *is* the re-spawn. `upgrade`-then-retry recovers a missing rule: the re-spawned
child raises again and the parent resolves against the rules it now holds.

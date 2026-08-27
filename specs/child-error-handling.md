# Child → parent error handling

Status: **shipped 2026-07-20** (raise/panic + `error_code`, single-child catch, batch
resolution); **fault payloads shipped 2026-08-22**, replacing I6's original "no data
crosses" — designed in [error-extensions.md](error-extensions.md) X2, whose X1 and X3 stay
declined. **§5.5 and §12 — per-slot child retry and the operator's re-spawn — shipped
2026-08-27**, reversing D7, as did **M2** (`case` on an `on_error` rule).

## 0. Governing principle

> **An error is a branch slot, not a value.**

A raise says: *an anticipated condition prevents me from finishing, and my parent may react
to it.* It carries a code (to branch on), a message (to read), and — where the caller
declared that code's shape — a payload (to act on). Still not a second result channel: a
*successful* child's return value belongs in `output`, and a payload is readable only where
the caller asked for it by name (I6). Two corollaries:

- **Anything unanticipated panics.** No catchable form, no wildcard. Making a failure
  reactable means converting it to a raise *inside* the child, at the task that understands
  it (§5.4).
- **Only a declared condition can be retried around.** A child task's `on_error` carries
  `retry` like any other task's (R4, reversed 2026-08-26) — a child is a call, and a call is
  retryable. "Catch everything and retry" stays impossible: a rule can only name codes in
  `raises(D)` (R5), and a defect is never catchable (D5).

Three terminal clauses cover the outcome space once each: `goto: end` → `completed`,
`raise: {code, message, data?}` → `raised` (the caller reacts by naming the code), `panic:
{code, message, data?}` → `failed` (nothing can react, ever). The two failure codes have
different readers — a raised code is consumed inside the tree by a branching parent, a panic
code outside it by dashboards. The `{ok: false, reason}` output convention coexists: "8 of 10
shipped and why" is a result, not control flow.

## 1. Vocabulary

**batch** — children of one `(parent_id, spawn_task_id, parent_task_epoch)`; **slot** — a
stable batch position, surfaced as `child_key` (`child_map`) or `child_index`
(`child_list`); **raise/panic** — termination via the clause → `raised`/`failed`;
**defect** — any `failed`, authored or engine, never catchable; **raise set** — `raises(D)`
(§2.3); **resolution** — the parent's decision over a settled batch (§5.2).

## 2. Surface syntax

### 2.1 `raise` — a clause, not a goto

A field (`raise: {code, message, data?}`) so the three travel together. One shared `Fault`
type serves raise and panic — they differ in what they do, not what they carry, and the
distinction lives at the use site (`Raise *Fault` / `Panic *Fault`). Valid on `SwitchCase`
and `ErrorCase`, mutually exclusive with `goto` (R3). `message` is required (R1) and is a
template; `data` is optional, an expression or object of expressions; both are evaluated in
the clause's own scope when it fires. The code is a literal (R2), or the raise set stops
being computable.

The two fail differently, deliberately: a message that will not render **degrades to its raw
template**, while `data` that will not evaluate **fails the instance** with
`engine.expression` — the payload is a contract, so dropping it silently would surface the
loss at the caller's conform rather than at its origin. Both are evaluated *before* the
clause concludes, so a fault reached through `on_error` can read the `error` it is handling
to recompose its own payload.

### 2.2 `panic` — authoring a defect

Terminates as `failed`: *I detected something I did not anticipate; nothing downstream
should work around it.* The code is for **classification**, not catching — `error_code` is
filterable, so `submit_contract_violation` can be alerted on. The canonical case is one HTTP
cannot see: a `200` with an error body satisfies `accepted_status`, so only the switch knows
the call failed, and only the author knows whether that body is anticipated (`raise`) or a
broken contract (`panic`). Named "panic" not "fail" on purpose: it is alarming, uncatchable,
and takes the tree down — a milder word would invite it. A panic may carry `data` as well —
the slot is on `Fault`, so both clauses have it — but it reaches no parent, only an operator
reading the instance detail and its logs.

Since §5.5 the clause also decides **who may re-run the work**: a raise invites the parent to
retry, so a raised child is re-spawned whole; a panic invites nothing, so a failed child is
revived *in place*, its completed tasks — `only_once` included — never redone, and a panic
standing at an `only_once` task refuses retry without `force`.

### 2.3 The raise set is inferred

No `errors:` block. `raises(D)` is a purely syntactic scan of `raise` clauses — statically
exact, imprecise only in the safe direction (an unreachable raise inflates it). **Panic
codes are excluded**: the raise set is what R5 validates rules against, and no rule can ever
match a panic (a panicking child poisons its ancestors, so the parent never resolves).
Discoverability is closed by publishing `raises(D)` on the definition listing.

### 2.4 Parent side — no prefix, no new syntax

On a child task, `on_error` codes need no `child.` namespace because there is little else
they can see: input validation, definition lookup, spawn and collect all go straight to
`failInstance` (E6, as amended). Raised codes are lexically distinct too — R1 forbids `.`,
every engine code has one — so a raised code is always an authored name (`psp_unavailable`,
never a re-raised `http.503`). **Propagation is explicit**: a parent re-raises via a `raise`
in an `on_error` rule, which puts the new code into *its* raise set.

## 3. Static semantics (registration)

- **R1 — fault shape.** `Code` matches `^[a-z][a-z0-9_]*$`; `Message` non-empty; `Data`
  optional. `.` is
  reserved for engine codes, so re-raising a system code is refused by construction; `%` is
  the match wildcard, so no code ever needs escaping in a pattern.
- **R2 — the code is static.** An expression in `Code` would make `raises(D)` uncomputable
  and `error_code` unqueryable. Nothing enforces it separately: no expression can be spelled
  in R1's regex, so R1's shape *is* R2. The rule once covered `Message` too — **an
  oversight**, whose check matched `${` by substring, missing a leaf-leading `$:` and
  catching the `$${` escape. A message is a template, type-checked to a **non-null string**.
- **R3 — one terminal clause.** A `SwitchCase` carries exactly one of `goto`/`raise`/`panic`;
  an `ErrorCase` carries **at most** one, because a verb-less rule predates this design
  (exhaust retries, then fail with the engine's code). Checked in the validator, not the
  decoder, so the rejection names task and case index.
- **R4 — `retry` is allowed on a child task; `not_reached` is not.** *Reversed 2026-08-26
  (built 2026-08-27):* a child task's `on_error` carries `retry` like any other task's,
  and retrying re-spawns the raised slots (§5.5). The `not_reached` rejection stays, and is
  now load-bearing: it keeps an `only_once` child task — the parent's own spawning task,
  whose re-attempt is within *this* instance — un-retryable, since every code such a task can
  catch (`raises(D) ∪ {output.invalid}`) implies the child ran, and spawn-time failures never
  reach `on_error` (E6). Registration must refuse that combination rather than leave
  `isRetryAllowed` to drop it at runtime: a retry that can never fire is what D7 meant by
  "rejecting beats silently ignoring". (`only_once` *inside* the child is a different
  position, deliberately unguarded — §12.) Codes are
  patterns, like an action task's — safe because R5 validates them against a finite raise
  set, and the SQL-`LIKE` `_` footgun was removed at the source (M1).
- **R5 — rule reachability.** Every pattern must match some code in `⋃ raises(D)` over the
  task's resolved children; catch-alls exempt. Its own pass, because reachability is a
  property of the rule set against the union, not of one entry. It subsumes the old
  `child.failed` rejection: an empty raise set makes any pattern unreachable.
- **R6 — a code is a raise code or a panic code, never both.** Otherwise `error_code` means
  two things on one process, for exactly the observers it serves.
- **R7 — a declared payload must be one the code can carry.** *Added 2026-08-27.* Beside R5's
  question about the code, `raises[code]` is checked against the payload type the child's raise
  clauses actually produce: `NarrowsTo`, the same relation and the same soundness condition as
  `result_schema` (collect conforms it, so an unknown may be narrowed). Two clauses on one code
  union; a clause attaching nothing types as `null`, since the slot is cleared.
  specs/error-extensions.md §X2-c.

### 3.1 What R5 does and does not catch

One direction only, rule → raise set: typos caught, a rule orphaned by a removed code caught
on re-registration, but **a code added with no rule surfaces at runtime** — the deliberate
cost (an unhandled raise fails the parent, §5.2). Version pinning bounds the blast radius: a
new code reaches a parent only after a deliberate dependency bump. Requiring coverage was
rejected as painful for shared children (D3).

## 4. Matching

**M1.** A rule matches iff one of its `code` patterns matches the raised code (empty list =
catch-all) — the same `matchOnError` as action tasks. **`%` is the only wildcard**; `_` and
`.` are literal. Two draft positions were reversed here: literal-only matching, and the
SQL-`LIKE` semantics that would have made `_` a silent single-character wildcard.

**M2 — `case`, a predicate on the matched error. Shipped 2026-08-27.** A rule may
carry a `case` beside its `code`; it matches only when both hold, and a false case falls
through to the next rule. It is `SwitchCase.Case` on the error channel — `on_error` was always
a switch over errors, and the predicate is the half it was missing.

**What it removes is a task, and that is the point.** Classifying an error in a handler leaves
the instance standing on the *handler*, with the batch behind it and nothing able to re-run
the child — retry re-runs the task the instance sits on (§11.1), so a routed failure is out of
its reach. Deciding at the rule leaves the instance on the task that failed, which is where
`retry` can act. The alternative without it is to let a retry budget separate transient from
permanent by observation, which works but spends attempts on failures already known to be
permanent.

**The prefilter is what makes the predicate worth writing.** At a routed task `error` is the
union over every rule that routes there, so `error.data` is whatever any of them could carry.
Inside a rule naming `code: ["x"]` it is exactly `raises.x` — one declared shape. The case is
therefore *better* typed than the handler it replaces, and needs no new inference: the union
across a rule's patterns is already computed for the routed task, applied one step earlier.
A rule naming a code the call never declared under `raises` has no readable `error.data`, and
reading it stays a registration error.

Five things the design has to say:

- **Fall-through, or the feature is inert.** Code-matched-but-case-false must try the next
  rule, not fail the match outright.
- **A guarded rule is never a catch-all.** An empty `code` list means catch-all and must be
  last; `{code: [], case: X}` is not total, so it must neither satisfy that requirement nor be
  forced last. Missing this rejects good definitions and, worse, keeps promising that some
  rule always fires.
- **Naming a code stops guaranteeing the error is handled.** Today a code in a rule means it
  is caught; with a case it means it is *considered*. Nothing static can warn — the predicate
  is dynamic — so this is the hazard the feature buys, and an unmatched raise still degrades
  to a defect (§5.2).
- **The error is bound, not written.** The case evaluates in the scope the routed task would
  see — the process context plus `error`, no `self`, since the task produced no result. The
  payload conform runs first (it can replace the code, which decides the rule), but nothing is
  persisted: a rule that does not match leaves no trace, and since §5.5 a retrying parent
  writes no `error` at all.
- **A failed evaluation fails the instance.** It is type-checked at registration, so a runtime
  failure means that guarantee did not hold — `engine.expression`, not a silent non-match.
  Same reading as fault `data`: a contract, not prose.

R5 is untouched: reachability stays a question about codes, and the case narrows at runtime on
top of it. Per-slot admission (§5.5) evaluates the case per slot, against that slot's own
error, which is the same rule applied where the batch has many.

Two things the build added. The case is an **expression, not a template** (`Expr: true`) — as
a template it renders to a string and compares as nothing, silently taking every guarded rule
out of play. And `case` was previously a REJECTED key on `on_error`, with a hint pointing at
`code`; the mistake it caught — a code list written under `case` — is still a mistake, so it
is now refused by shape instead, keeping the same hint.

## 5. Operational semantics

### 5.1 Child: raising

`raise` and `panic` write identical fields (`error_code`, `error_message`, `error_data`,
`wake_at := nil`) and
differ only in status — the whole difference, since status decides whether ancestors are
poisoned. Panic is `failInstance` with authored words (→ `FailInstanceAndAncestors`); raise
writes `StatusRaised`, which falls through to `FinishChild` because a raise is a normal
outcome. `StatusRaised` is directly terminal — a raise happens at a task boundary, so the
child's own children already collected — and joins `Status.Terminal()` (§11.4). **Neither
computes process `output`**, and registration agrees for free: the output-boundary analysis
keys on `Goto == end`, which a raise case never has, so an early-exit raise is never asked to
read outputs that do not exist yet.

### 5.2 Parent: resolution

Precondition: `running ∧ collecting` (failing → `settleFailing`; pausing →
`settlePausing`; paused is unclaimable and resolves on resume). Consequence: **a resolving
parent's batch holds only `completed` and `raised` children** — failed poisoned it first
(§5.4), paused holds it in `waiting` (`CountActiveSiblings`), running means unsettled.

```
E := raised children in slot order
E = ∅        → collect outputs, continue          (happy path)
otherwise    → admit retries (§5.5)
               any slot re-spawned → park on 'waiting'; no error, no route
               else f := E[0]; write `error` from f (§5.3); match f's rule:
                    nil or verb-less → fail P   ·  goto:end → complete P
                    raise/panic      → as §5.1  ·  goto:$id → P.task := id
```

**`error` is written only when the batch is done retrying** — a parent on a backoff carries
none, matching the action path, where `handleCallError` returns from its retry branch before
writing. What reaches an operator is the raise that *ended* the batch: a slot that raised and
recovered contributes nothing, and the reported slot may differ between rounds. Only the write
moves — the payload conform still runs ahead of the rules, since it can replace the code.

Deterministic — no clock, no completion order (I3). It sits ahead of the collect, so
`buildChildOutput` keeps its strict every-child-completed guard (a raised child reaching it
is a bug, not a case). The `rule = nil` branch is the **normal** unhandled-raise path: its
message names code, child and slot, and the parent's `error_code` becomes the child's raised
code, not `engine.collect`. **An unhandled raise degrades to a defect** — propagation is
never implicit. `goto: end` computes the output through `completeViaErrorHandler`, shared
with the action-task path so the two cannot drift.

### 5.3 What the routed task sees

`error` = `{task, code, message, data?, child_key | child_index}`, with `data` present only
where the call declared that code's shape (I6). The slot is two single-typed fields rather
than one `string|integer`, so a handler never type-switches; exactly one is present, and both
are optional in the schema because a plain action-task `on_error` leaves them absent. **One
raise is reported**, the first in slot order, never the set (D2). The routed task keeps its
normal context; the only absence is `outputs.<T.id>`, which the failed batch never produced.

### 5.4 Defects fail fast, always

A failed child — authored panic or engine fault alike — is uncatchable under any
configuration: `FailInstanceAndAncestors`, batch abandoned, parent settles without resolving.
No `child.failed` code, no opt-in. The only route to catchability is a raise inside the child
at the task that understands the failure (`on_error: [http.503] → retry: 3 → raise:
psp_unavailable`). A defect in one slot dominates a sibling's raise: a fault must not be
masked by a business error beside it.

### 5.5 Retrying a child task

A rule matched on a raised slot may carry `retry`. **Each slot is a call with its own
budget**: at resolution every raised slot matches its own code (§4), and one whose rule
carries `retry` and whose attempt count is under that rule's limit is **re-spawned** —
superseded and replaced in the same batch (§12). If any slot is re-spawned the parent returns
to `waiting` and resolves again when the batch settles; if none is, `raised[0]`'s rule routes
as in §5.2. Completed siblings stand, so I1 is unchanged: a slot may take several attempts.

The count is `_spawn_attempt`, carried in the child's `_spawn_*` bookkeeping beside its slot
identity — not on the parent, not in a column. It starts at 0 and does not distinguish codes,
so `attempt < limit` admits exactly `limit` retries against whichever rule matched this
round. The replacement's `wake_at` is `updated_at + retryDelay(attempt, policy)`, measured
from the **raised child's** conclusion rather than from dispatch: the wall-clock it spent
waiting on siblings already served what a backoff is for, so a delay shorter than that wait
lands in the past and the replacement is claimable at once.

A replacement's input is **re-evaluated** against the parent as it now stands — version
re-resolved, input rebuilt and re-validated — not copied from the attempt it replaces.
*Reversed 2026-08-27, having shipped as a copy.* The copy was argued on "retrying a call
re-sends the same arguments", with a live `config` the only thing it could miss. That misses
the case the whole reversal exists for: a `$import`ed script is an **input**, and publishing a
new version is how a caller changes one — so copying makes a fix undeliverable. Fix the
script, apply, upgrade, retry, and the replacement is handed the old string. The batch phase 1
would spawn now is rebuilt **once per round** and indexed by slot, so a hundred-slot fan-out
retrying forty still evaluates each input once. A slot the parent no longer declares — an
upgrade removed the entry, or `over` came back shorter — fails the instance rather than
inventing an input for it.

**The batch is the unit.** A slot is re-spawned when the batch settles, not when it raises.
That is what keeps §5.4 true: retrying eagerly would spend an attempt, and its side effects,
on a batch a sibling is about to poison, whereas a defect puts the parent in `failing`,
`WakeParent` gives it `''` rather than `collecting`, and resolution never runs. Two costs — a
slot cannot retry before its retrying siblings settle, and a raise no rule would retry does
not route until they do.

Five things break silently:

- **The parent must not bump `task_epoch`.** The action retry branch does, because the epoch
  makes an external token unique per occurrence. A child task has no token and its epoch is
  the batch identity; bumping it orphans the kept siblings (§12).
- **The parent's `retry_count` is not the budget.** Entering a spawn task zeroes it
  (`child.go`) — which is what §10.1's "unbounded" means. A count the parent never rewrites is
  what terminates the loop.
- **Per-slot admission needs a per-slot conform.** A payload failing its declaration replaces
  the code, and the code picks the rule, so every raised slot is conformed each round — where
  before §5.5 only `raised[0]` was. Built: `admitRetries` loops the batch through `slotError`.
- **Dispatch is `outcomeSpawn` but not `SpawnChildrenAndWait`.** That primitive refuses a
  parent whose `wait_state` is not `''`, and a retrying parent's row reads `collecting`. The
  supersede, the inserts and the park must be one transaction, or a crash leaves a slot with
  no replacement and the next collect is missing it.
- **A superseded attempt keeps its subtree.** Rows and object claims accumulate per attempt —
  the price of the history §12 keeps.

Budgets multiply: a child's own `retry: 3` under a parent's `retry: 3` is nine attempts at
the underlying call. A re-spawned child that panics ends the loop at once (§5.4).

A round audits **one line per slot**, not one per round, at warn — naming the slot, the code
it raised and `attempt n/N`, the same granularity the action path reports. A round is not a
unit anyone debugs, and a per-round line can name neither the slot nor the attempt. Like the
spawn audit it is written after the commit, so it can never name a child that does not exist.

Tests must assert the **collected output**, not status — today's e2e retry test passes while
the batch it collects is silently empty. Also pin that completed siblings do not re-execute,
that a slot which always raises stops after `limit` rounds, that superseded rows never reach
the collect, and, on the shiftable clock, that the delay runs from the failure.

## 6. Invariants

- **I1 — all-or-nothing.** `outputs[T.id]` exists only when every child completed.
- **I2 — single observation.** A settled batch is resolved exactly once; with §5.5 a task may
  resolve once per generation, each over its own rows.
- **I3 — determinism.** Resolution is a pure function of (T, slot-ordered children) — no
  clock, no completion order. Attempt counts ride those children (§5.5), so the tuple stands.
- **I4 — crash safety.** From I3: re-resolving the same rows decides the same, so a
  reclaimed parent resumes identically.
- **I5 — caller independence.** A child's terminal status and its ancestor effects do not
  depend on who spawned it.
- **I6 — data crosses only where the caller declared it.** A fault carries `data` beside its
  code and message, and the parent reads it as `error.data` only where the call declared that
  code's shape in `raises` — the error channel's counterpart to `result_schema`, declared by
  the **caller**, so a generic child stays generic. `{}` exposes it opaquely for a rule to
  narrow, `null` declares a code carrying none, an omitted code leaves `error.data` absent
  (reading it is a registration error), and a payload that does not fit its declaration
  reports `output.invalid` in place of the raised code. The original "no data crosses" rule
  is out; what survives is the half that mattered — the caller asks by name.
  **Amended 2026-08-27:** a declaration is checked against the child's inferred payload at
  registration (R7), so `output.invalid` is reached only by a payload registration cannot
  type — the top type a generic wrapper forwards.
- **E6 amended 2026-08-22 (built).** §2.4's "nothing else they can see" no longer holds
  exactly: a child task's catchable set is `raises(D) ∪ {output.invalid}`, the
  `result_schema` conform having moved off `engine.collect` so a caller narrowing an
  **unknown** child output can react to a bet that lost.

## 7. Data model

Migration 023 added `error_code TEXT NOT NULL DEFAULT ''` — one spelling of "no code",
filterable but not sortable, so no index. Migration 034 split the columns **by direction**:
`error_internal` is the error the instance CAUGHT, part of its state at the task it stopped
on, so a concluding fault must not edit it; `error_code` / `error_message` / `error_data` are
the error it REPORTS. Three plain columns rather than one JSON object, because `error_code`
is filtered on and a code buried in a blob can be neither indexed nor matched in SQL. Only
the payload gets a value column — it is arbitrarily large, and takes the same object-store
cut every other value slot gets.

### 7.1 `error_code` discriminates every non-success outcome

`completed` → `''`; `raised` → the raised code; `failed` → the panic code or the engine code
— the last being where the operational value is, since engine codes existed only inside prose
before. Two families: call codes (`http.500`, `pre.timeout`, `output.invalid`…) via
`handleCallError`, and a closed `engine.*` set (`definition`, `expression`, `config`, `input`,
`spawn`, `collect`, `panic` — the last for a Go panic escaping an advance) via
`failInstance`. (`engine.only_once` later became the catchable `only_once.interrupted` —
[only-once-interrupted.md](only-once-interrupted.md).) R1's namespace split holds: authored
codes never contain a dot, engine codes always do.

**Exactly one status predicate changes** for `raised`: `CountActiveSiblings` adds it to the
settled list, or the parent never wakes. `ClaimInstances` (whitelists live statuses),
`FailAncestors` (a settled `raised` row must not reopen into `failing`) and `WakeParent` need
nothing. The asymmetry between the first and third is the whole status in one line: terminal
for "is the batch done", but not a failure — it neither poisons nor is poisoned.

### 7.2 The wire format is hand-written

`SwitchCase`/`ErrorCase` wire structs, their `UnmarshalJSON` and the hand-written
`JSONSchemaBytes` blobs change in lockstep — `required: ["goto"]` and
`additionalProperties: false` each block `raise` at the edge otherwise — and the OpenAPI blob
fails far from the change. R3 lives in the validator so the rejection is named, not a decode
error.

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

The cases that only restate a rule are not listed: a `failing` parent short-circuits, a
recursive child terminates under R5's syntactic scan, a crash mid-resolution is I4, a raise
on an unreachable task inflates `raises(D)` safely.

## 9. Locked decisions

- **D1 — no `child.` prefix** (§2.4).
- **D2 — no `siblings`; `error` reports one raise.** The engine never routes on an
  aggregate; "6 of 10 failed and why" is a result §0 sends to output; and it does not fit a
  batch anyway — I1 means a partial-raise batch produces no output, so the successes are
  uncollectable regardless.
- **D3 — reachability only, no exhaustiveness.** Coverage requirements make shared children
  painful, and direction matters: adding exhaustiveness later breaks definitions, removing it
  later breaks nothing.
- **D4 — `raised` is a distinct status.** Not `completed` (dashboards key on status), not
  `failed` (that means defect, and poisons). Named to pair with its clause as `paused` pairs
  with `pause`, and to survive a column scan. It also turns out to name the unit of retry
  (§12).
- **D5 — defects are never catchable** (§5.4). **D6 — `panic` carries a code for
  classification, not branching**: parents branch, API consumers classify, and the asymmetry
  is reachability rather than shape (R6).
- **D7 — no parent-side retry. REVERSED 2026-08-27 (built).** The original
  argued that a raise is settled so re-spawning reproduces it, and that per-slot retry would
  add an attempt dimension to the concurrency-sensitive sibling queries. Provisional —
  refused until a use case arrived — and one did. Both premises fell: §5.5 re-spawns the
  child *whole*, so its upstream tasks run again and can decide differently; and the attempt
  count rides the child's `_spawn_*` bookkeeping, so those queries gain nothing at all. What
  it blocked is the north star — a child is a call, and a call retries like any other task
  ([custom-tasks.md](custom-tasks.md)).

## 10. Phasing

Built in order: raise/panic + `error_code` + R1–R3/R6; single-child catch (M1, R4/R5,
resolution ahead of the collect); batch resolution with `child_key`/`child_index`.

### 10.1 Re-running a batch without retry

A `goto` back to the spawning task re-spawns a fresh batch (the error route cleared
`wait_state`). Deliberately coarse: every slot re-runs, wrong for `only_once` children,
wasteful for fan-outs, unbounded unless the definition carries its own counter. Exactly what
per-slot retry would fix — reaching for it repeatedly was named as the evidence to build it,
and in 2026-08 that fired (D7 reversed; §5.5, §12). The `goto` remains legal and is still the
only way to re-run *completed* slots; it is no longer the way to re-run raised ones.

## 11. The `retry` command

`RetryProcess` is failed-only (the pause/resume split — [pause-resume.md](pause-resume.md)).
One unit of retry: a process. One restart path: name the root, and `revive` walks down to
what was interrupted, keeping settled work.

### 11.1 Retry re-runs the task the instance sits on — no rewind

That fact decides everything. For a **fault** it fits: the cause is at the task. For a
**raise** it does not — the deciding state is upstream and persisted, so re-running the task
re-raises identically (provably for a switch-only task, and worse with an action, which
already *succeeded* and would repeat a side effect). So the line is fault vs outcome, and
status already encodes it (D4). **Panics stay retryable**: `panic` chose `failed` precisely
because it means fault, and fix-outside-and-re-enter is what retry is for.

A definition can be written into a corner by this: an error routed to a handler leaves the
instance on the handler, so the failure that mattered is behind it. M2 is the way out — decide
at the rule, and the instance stays where retry can reach.

**Rejected refinement:** gating retry on "the task has an action". The proxy is wrong —
`config` is live, so a switch-only panic on `config.psp_enabled` retries meaningfully after
an env flip — and a correct gate needs static analysis of what an expression reads, on an
operator-facing API. The proportionate lever is information, not refusal.

### 11.2 `raised` is settled: never revived

In `revive`, `raised` joins `completed` ("settled, keep"), not the defensive live-status arm
("not ours to touch") — the two `return nil`s mean different things. Shipped beside it was a
stated limitation: a parent that failed on an unmatched code re-resolves identically, because
the missing rule lives in a version-pinned definition retry does not re-read. §12 removes it
— never *revived*, now *re-spawned*.

### 11.3 Clear every error slot on revive

All four columns clear together — the three reported (`error_code`, `error_message`,
`error_data`) and the caught `error_internal`. A kept `error_code` corrupts the very column
it exists for (a revived-then-completed instance still reports its old death); a kept
`error_internal` survives into `error` reads. Clearing cannot break a valid definition:
mustErr/mayErr admits an `error` read only where an error is present on every path.

### 11.4 `Status.Terminal()` must include `raised` — a live bug if missed

Revival reconstructs `waiting` vs `collecting` from "after revival, is anything still
active?". If `Terminal()` does not know the status, the parent parks in `waiting` forever,
unrecoverable and unlogged. The fix belongs in `Terminal()` itself — it answers "is this
settled", and D4 says yes — and its SQL mirror is `CountActiveSiblings` (§7.1); the two
edits travel together, either alone hangs a parent.

## 12. Re-spawn on the `retry` command

**Shipped 2026-08-27.** The operator's counterpart to §5.5 — same mechanism,
different trigger. `retry` stays one verb with no flag, because the child's status is the
intent: a `failed` child is retried from inside (its pending task, everything before it
standing), a `raised` child is re-spawned whole, since only re-running the upstream tasks
that produced the decision can produce a different one. §11.1's fault-vs-outcome line one
level down, on the distinction D4 already carries.

**The operator bypasses the budget rather than resetting it**, and does it by MARKING the
parent rather than re-spawning. `RetryProcess` revives the parent to `collecting` with a
one-shot `_retry_override`; the engine's admission reads it on the next collect, admits every
raised slot whatever `_spawn_attempt` says, and clears it. The count still advances, so the
fresh child runs exactly once: if it raises again §5.5's admission declines and the batch
routes or fails. One extra attempt per slot — the shape retry already has on a failed action
task, which keeps `retry_count` so the revived task "runs once and surfaces its failure
instead of grinding backoffs". Under the override the rules are not consulted at all, which
also means **no backoff**: someone asking for a retry wants it now.

The marker exists because the re-spawn cannot happen in the db layer. A replacement's input is
re-evaluated against the parent's current definition (§5.5) — that is how a fix published as a
new version reaches the child — and nothing in `internal/db` can evaluate an expression. One
re-spawn implementation, one place inputs are built.

The mechanism, shared with §5.5:

- **The parent keeps its `task_epoch`.** Batch identity is `(parent_id, spawn_task_id,
  parent_task_epoch)` and the kept siblings live under it. Until 2026-08-27 the walk bumped
  the epoch on *every* revived node, which orphaned the batch: the collect found nothing, so a
  `child_map` merged `{}` and reported **completed**, a single `child` failed
  `engine.collect`, and a raised sibling was never resolved. It had been wrong since the
  epochs were introduced. Fixed by splitting the decision — a node revived with no batch still
  bumps, because it re-runs from the top and an external token derives from the epoch — and by
  binding `parent_task_epoch` in the walk's own lookup, without which a spawn task re-entered
  by a loop hands it two generations at once.
- **The replacement copies the slot's identity** (`spawn_task_id`, `_spawn_child_key` /
  `_spawn_index`, `call_stack`, input) with fresh runtime state, through the ordinary persist
  path — an input carrying object references needs its own claims declared, and a raw column
  copy leaves the row pointing at content nothing holds for it.
- **The old row is superseded, not deleted** — a nullable `superseded_at` filtered out of
  `GetChildrenForTask` and of `revive`'s lookup, or `buildChildOutput` sees two rows for one
  slot and the stale raise routes again. `CountActiveSiblings` needs no predicate: a
  superseded row is `raised`, hence never active.

The parent needs no new handling: the fresh child is non-terminal, so `revive`'s existing "is
anything still active" test parks it at `waiting` (§11.4).

**`only_once` does not gate a re-spawn.** It bounds attempts within an instance, and a
re-spawn makes a new one — indistinguishable from starting the process again, or from §10.1's
`goto`. A guard would claim a cross-instance guarantee nothing else offers, and the field
concedes the point itself: `only_once.interrupted` exists so an author can check the system of
record. Protection is idempotency at the child's boundary, with the clause (§2.2) as the
engine-level barrier. Two positions, two answers — inside the child it is out of scope; on the
parent's own child task it still refuses (R4).

**A raised root stays refused**: a raise is retried by re-spawning it *from its parent*, and a
root has no parent, so "start a new instance" *is* the re-spawn.

`upgrade`-then-retry still recovers a missing rule — the re-spawned child raises again and the
parent resolves against the rules it now holds, one execution later. X1-b in
[error-extensions.md](error-extensions.md) is §5.5, accepted with it.

# Child → parent error handling

Status: **fully implemented 2026-07-20** — all three phases (§10): raise/panic +
`error_code`, single-child catch, batch resolution. Where implementation refined the
draft, a **Delta** notes it inline. Declined extensions (batch-shape routing, raise
payloads, exhaustiveness) live in [error-extensions.md](error-extensions.md). Examples
predate the `{{ }}` → `${ }` retarget; read accordingly.

Decisions closed at shipping: D3 confirmed (reachability only — the one decision that is
expensive to reverse later); D7 confirmed (no parent-side retry; §10.1 is the workaround
and the trigger); §11.3 revive clears `$error` too; the status is `raised`, not
`errored` (D4). The pause/resume redesign strengthened §5.2's guarantee (a paused child
counts as active in `CountActiveSiblings`, so a resolving parent still sees only settled
children — now independent of where a stop originated) and made E1 a live, correct case.

## 0. Governing principle

> **An error is a branch slot, not a value.**

A raise says: *an anticipated condition prevents me from finishing, and my parent may
react to it.* It carries a code (to branch on) and a message (to read) — nothing else.
If the parent needs data back, it was an outcome and belongs in `output`. Two
corollaries:

- **Anything unanticipated panics.** No catchable form, no wildcard. Making a failure
  reactable means converting it to a raise *inside the child*, at the task that
  understands it (§5.4).
- **Errors are for branching, not retrying around.** Transient conditions are the
  child's to retry; a raise describes a *settled* condition (hence D7), and
  "catch everything and retry" is not expressible.

Three terminal clauses cover the outcome space once each: `goto: end` → `completed`
(produces output); `raise: {code, message}` → `raised` (caller reacts by naming the
code); `panic: {code, message}` → `failed` (nothing can react, ever). Both failure
clauses carry codes for different readers: a raised code is consumed inside the tree by
a branching parent; a panic code outside it, by dashboards and alerting. The
`{ok: false, reason}` output convention coexists — "8 of 10 shipped and why" is a
result, not control flow.

## 1. Vocabulary

**batch** — children of one `(parent_id, spawn_task_id)`; **slot** — a stable batch
position, surfaced as `child_key` (string, `child_map`) or `child_index` (integer,
`child_list`); **raise/panic** — termination via the clause → `raised`/`failed`;
**defect** — any `failed`, authored or engine, never catchable; **raise set** —
`raises(D)` (§2.3); **child task** — action `child_map`/`child_list`; **resolution** —
the parent's decision over a settled batch (§5.2).

## 2. Surface syntax

### 2.1 `raise` — a clause, not a goto

A field (`raise: {code, message}`) so code and message are structurally paired. One
shared `Fault` type serves raise and panic — they differ in what they do, not what they
carry; the distinction lives at the use site (`Raise *Fault` / `Panic *Fault`). Valid on
`SwitchCase` and `ErrorCase`, mutually exclusive with `goto` (R3). `message` is required
(R1); both fields are literals (R2) — an expression would re-open the data channel this
design closes.

### 2.2 `panic` — authoring a defect

Terminates as `failed`: *I detected something I did not anticipate; nothing downstream
should work around it.* The code is for **classification**, not catching — `error_code`
is a filterable column, so `submit_contract_violation` can be counted and alerted on.
The canonical case is one HTTP cannot see: a `200` with an error body satisfies
`accepted_status`, so only the switch can tell the call failed — and only the author
knows whether that body is anticipated (`raise`) or a broken contract (`panic`). In an
`on_error` rule, `panic` documents a fatal code ahead of a permissive later rule or
replaces the engine's generic text. Named "panic", not "fail", on purpose: it is
alarming, uncatchable, and takes the tree down — a milder word would invite it.

### 2.3 The raise set is inferred

No `errors:` block. `raises(D)` is a purely syntactic scan of `raise` clauses — no
dataflow, statically exact, imprecise only in the safe direction (an unreachable raise
inflates it). **Panic codes are excluded**: the raise set is what R5 validates rules
against, and no rule can ever match a panic (a panicking child poisons its ancestors and
the parent never resolves). Discoverability cost is closed by the server computing
`raises(D)` at registration and publishing it on the definition endpoint.

### 2.4 Parent side — no prefix, no new syntax

On a child task, `on_error` codes need no `child.` namespace because there is nothing
else they can see: every other failure path (input validation, definition lookup, spawn,
collect) goes straight to `failInstance` (E6). Raised codes are also lexically distinct
— R1 forbids `.`, every engine code has one — so a raised code is always an authored
semantic name (`psp_unavailable`, not a re-raised `http.503`). **Propagation is
explicit**: a parent re-raises via a `raise` in an `on_error` rule, which puts the new
code into *its* raise set.

## 3. Static semantics (registration)

- **R1 — fault shape.** `Code` matches `^[a-z][a-z0-9_]*$`; `Message` non-empty. `.` is
  reserved for engine codes (and re-raising a system code is refused by construction);
  `%` is the match wildcard, so no code ever needs escaping in a pattern.
- **R2 — faults are static.** No expressions in `Code` (would make `raises(D)`
  uncomputable and `error_code` unqueryable) or `Message` (would smuggle data).
- **R3 — one terminal clause.** A `SwitchCase` carries exactly one of
  `goto`/`raise`/`panic`. *Delta:* on an `ErrorCase` it is **at most** one — a verb-less
  rule predates this design (exhaust retries, then fail with the engine's code). Checked
  in the validator, not the decoder, so the rejection names task and case index.
- **R4 — child tasks do not retry.** `retries`/`not_reached` rejected (D7); codes are
  patterns matched like an action task's. *Delta:* the draft's literal-only child codes
  were reversed — patterns are safe because R5 validates them against a finite raise
  set, and the SQL-LIKE `_` footgun was removed at the source (M1).
- **R5 — rule reachability.** Every pattern must match some code in `⋃ raises(D)` over
  the task's resolved children; catch-alls exempt. *Delta:* generalized from literal
  membership to pattern match via the runtime matcher; this also subsumed the old
  `child.failed` rejection (an empty raise set makes any pattern unreachable). Lives in
  its own pass, `validateChildOnErrorReachability` — reachability is a property of the
  task's rule set against the union, not of one entry.
- **R6 — a code is a raise code or a panic code, never both.** Otherwise `error_code`
  would mean two things on the same process for exactly the observers it serves.

### 3.1 What R5 does and does not catch

One direction only, rule → raise set: typos caught; a rule orphaned by a removed code
caught on re-registration; **a code added with no rule surfaces at runtime** — the
deliberate cost (an unhandled raise fails the parent, §5.2). Version pinning bounds the
blast radius: a new code reaches a parent only after a deliberate dependency bump.
Requiring coverage was rejected — it makes shared children painful (D3).

## 4. Matching

**M1.** A rule matches iff one of its `code` patterns matches the raised code (empty
list = catch-all) — the same `matchOnError` as action tasks. **`%` is the only
wildcard**; `_` and `.` are literal. *Delta:* two reversals — draft literal matching,
then shared SQL LIKE (which made `order_%` match `order.placed` via `_`), then the fix
that stuck: drop `_` as a wildcard in the shared matcher, since both `_` and `.` are
ordinary characters in codes. Not full SQL LIKE, and the field description says so.

## 5. Operational semantics

### 5.1 Child: raising

`raise` and `panic` write identical fields (`error_code`, `error`, `wake_at := nil`) and
differ only in status — which is the whole difference, since status decides whether
ancestors are poisoned. No new plumbing in `saveAndNotify`: panic is `failInstance` with
authored words (→ `FailInstanceAndAncestors`); raise writes `StatusRaised`, which falls
through to `FinishChild` — correct, a raise is a normal outcome. `StatusRaised` is
directly terminal (a raise happens at a task boundary; the child's own children already
collected); it joins `Status.Terminal()` (§11.4). **Neither computes process `output`**,
and registration agrees for free: the output-boundary analysis keys on `Goto == end`,
which a raise/panic case never has — otherwise every early-exit raise would fail
registration for reading outputs not yet available. *Delta:* `failInstance` gained a
`code` parameter so no failure path leaves `error_code` empty (§7.1).

### 5.2 Parent: resolution

Precondition: `running ∧ collecting` (failing → `settleFailing`; pausing →
`settlePausing`; paused unclaimable — resolves on resume, never before). Consequence:
**a resolving parent's batch contains only `completed` and `raised` children** — failed
poisoned it first (§5.4), paused holds it in `waiting` (`CountActiveSiblings`), running
means unsettled.

```
E := raised children in slot order
E = ∅        → collect outputs, continue          (happy path)
otherwise    → f := E[0]; write $error from f (§5.3); match f's rule:
               nil or verb-less → fail P   ·  goto:end → complete P
               raise/panic      → as §5.1  ·  goto:$id → P.task := id
```

Deterministic — no clock, no completion order (I3). Lives at the head of
`runChildProcesses` phase 2, before the collect; `buildChildOutput` keeps its strict
every-child-completed guard (a raised child reaching it is a bug, not a case). The
`rule = nil` branch is the **normal** unhandled-raise path, and its message names code,
child and slot. **An unhandled raise degrades to a defect** — propagation is never
implicit. *Delta:* the failing parent mirrors the child (`error_code` = the child's
raised code, not `engine.collect`); a matched rule may also `raise`/`panic`, not only
`goto`; and `goto:end` computes the process output via `completeViaErrorHandler`, shared
with the action-task path so the two cannot drift (the action path previously skipped
`computeOutput` — a real bug this fixed).

### 5.3 What the routed task sees

`$error` = `{task, code, message, child_key | child_index}` — identity and code only, no
child data (I6). Two separate single-typed fields, not one `string|integer`, so a
handler never type-switches; exactly one is present. **One raise is reported** — the
first in slot order — never the set (D2): "which of the 10 failed and why" is a result
and belongs in `{ok: false}` outputs. The routed task keeps its normal context; the only
absence is `outputs.<T.id>` — the failed batch never produced one. Typing: `child_key`/
`child_index` are optional in the `$error` schema (a plain action-task `on_error` leaves
them absent); presence of `$error` itself comes from the existing mustErr/mayErr
dataflow. *Delta:* the draft's nested `slot: {key|index}` flattened to the two scalars;
`siblings` removed (D2).

### 5.4 Defects fail fast, always

A failed child — authored panic or engine fault alike — is uncatchable under any
configuration: `FailInstanceAndAncestors`, batch abandoned, parent settles without
resolving. No `child.failed` code, no opt-in. The only route to catchability is a raise
inside the child at the task that understands the failure (e.g. `on_error: [http.503] →
retry: 3 → raise: psp_unavailable` — retries belong on the fetch inside the child). A
defect in one slot dominates a sibling's raise: a fault must not be masked by a business
error beside it.

## 6. Invariants

- **I1 — all-or-nothing.** `outputs[T.id]` exists only when every child completed.
- **I2 — single observation.** A batch is resolved exactly once, when settled.
- **I3 — determinism.** Resolution is a pure function of (T, slot-ordered children).
- **I4 — crash safety.** From I3: re-running resolution over the same rows decides the
  same, so a reclaimed parent resumes identically.
- **I5 — caller independence.** A child's terminal status and its ancestor effects do
  not depend on who spawned it.
- **I6 — no data crosses.** After an error route the only child-derived values in the
  parent are code, message, and which child.

## 7. Data model

Migration 023: `error_code TEXT NOT NULL DEFAULT ''` (beside `error`, same convention —
one spelling of "no code"; filterable, not sortable, so no index). No data migration:
old rows honestly report `''`.

### 7.1 `error_code` discriminates every non-success outcome

`completed` → `''`; `raised` → the raised code; `failed` → the panic code or the engine
code. The last is where the operational value is — engine codes existed only inside
prose before. *Delta:* engine codes are two families — call codes (`http.500`,
`pre.timeout`, `output.invalid`…) via `handleCallError`, and a closed `engine.*` set
(`expression`, `config`, `definition`, `input`, `spawn`, `collect`) via `failInstance`'s
new `code` parameter. (`engine.only_once` later became the catchable
`only_once.interrupted` — see [only-once-interrupted.md](only-once-interrupted.md).)
R1's namespace split holds: authored codes never contain a dot, engine codes always do.

**Exactly one status predicate changes** for `raised`: `CountActiveSiblings` adds it to
the settled list (else the parent never wakes). `ClaimInstances` (whitelists live
statuses), `FailAncestors` (a settled `raised` row must not reopen into `failing`) and
`WakeParent` need nothing. The asymmetry between the first and third is the whole status
in one line: terminal for "is the batch done", but not a failure — neither poisons nor
is poisoned.

Go surface: `Fault`, `.Raise`/`.Panic` on both case types, `StatusRaised` (+
`Terminal()`, §11.4), `ErrorCode` on instance + summary + filters + CLI, `Raises()`,
the §5.2 split of `collectChildOutputs`, R5's pass, `$error` schema fields. *Delta:*
`raises` is published on the definition **list** (there is no per-definition JSON
endpoint); `error_code` went into the list summary despite the "listing stays light"
rule — it is short, and it is what a list is scanned for; the OpenAPI builder now
rewrites the shared `#/$defs/Model` prefix so any hand-written schema ref resolves.

### 7.2 The wire format is hand-written

`SwitchCase`/`ErrorCase` wire structs, their `UnmarshalJSON`, and the hand-written
`JSONSchemaBytes` blobs must change in lockstep (`required: ["goto"]` and
`additionalProperties: false` both block `raise` at the edge otherwise), and R3 must
live in the validator to produce named errors rather than decode errors. Not deep, but
four files, and the OpenAPI blob fails far from the change.

## 8. Edge cases

| # | case | resolution |
|---|---|---|
| E1 | child raises while tree paused | parent armed for `collecting` but unclaimable; decision deferred to resume |
| E1b | pause lands mid-resolution | routing write settles the pause via the `UpdateInstance` CASE; parent pauses already pointed at the goto target |
| E2 | parent already `failing` | short-circuits to `settleFailing`; no resolution |
| E3 | raise + defect in one batch | defect wins; the raise is never routed (§5.4) |
| E3b | child panics | any other defect; authored message is the only observable difference |
| E4 | `child_list` over `[]` | unchanged — continues inline |
| E5 | two children raise the same code | first in slot order routes (D2) |
| E6 | spawn-time failure | `failInstance`, never `on_error` — what makes §2.4 true |
| E7 | raise at the first task | fine; no output either way |
| E8 | root raises | no parent; API reports `raised` + code |
| E9 | grandchild raises, child unhandled | child fails; never crosses two levels implicitly |
| E10 | recursive child | R5 against `raises(D)` itself; syntactic scan, terminates |
| E11 | `child_map`, some raise | R5 takes the union over entries |
| E12 | crash mid-resolution | safe by I4 |
| E13 | handler routes into main flow | legal; `outputs[T.id]` absent — reads are a registration error |
| E14 | raise on unreachable task | inflates `raises(D)`; conservative in the safe direction |
| E15 | child gains a raise site post-registration | §3.1 row 3; bounded by version pinning |

## 9. Locked decisions

- **D1 — no `child.` prefix.** §2.4: a child task's `on_error` can only see raised
  codes, and they are lexically distinct from engine codes.
- **D2 — no `siblings`; `$error` reports one raise.** The draft's aggregate was cut:
  the engine never routes on it; "6 of 10 failed and why" is a result §0 sends to
  output; and it does not fit a batch anyway — I1 means a partial-raise batch produces
  no output, so the successes are uncollectable regardless.
- **D3 — reachability only, no exhaustiveness.** Coverage requirements make shared
  children painful. Direction matters: adding exhaustiveness later breaks definitions;
  removing it later breaks nothing.
- **D4 — `raised` is a distinct, non-retryable status.** Not `completed` (dashboards
  key on status), not `failed` (that means defect and poisons). Named `raised` to pair
  with its clause as `paused` pairs with `pause`, and to survive the column scan
  (`failed` vs `errored` is indistinguishable without docs).
- **D5 — defects are never catchable.** Authoring a panic grants nothing; conversion to
  a raise inside the child is the only route (§5.4).
- **D6 — `panic` carries a code, for classification not branching.** Parents branch;
  API consumers classify. The asymmetry is reachability, not shape: panic codes stay
  out of `raises(D)`, R6 keeps a code from being both.
- **D7 — no parent-side retry.** A raise is settled; re-spawning mostly reproduces it.
  Rejecting `retries` (R4) beats silently ignoring it. This removed the only part of
  the design touching the concurrency-sensitive sibling queries (a per-slot attempt
  dimension inside the lock-ordering discipline). Purely additive to add later; §10.1
  is the workaround, and using it repeatedly is the trigger.

## 10. Phasing

Built in order: (1) raise/panic + `error_code` everywhere + migration 023 + R1–R3, R6
(a root could already report typed failures; panic complete from day one); (2) catch
for a single child — M1, R4/R5, resolution ahead of collect; (3) batch resolution with
`child_key`/`child_index` (landed with 2 — a single child is a one-element batch).

### 10.1 Re-running a batch without retry

A `goto` back to the spawning task re-spawns a fresh batch (the error route cleared
`wait_state`). Deliberately coarse: every slot re-runs, wrong for `only_once` children,
wasteful for fan-outs, and unbounded unless the definition carries its own counter.
Exactly what per-slot retry (D7) would fix — reaching for this repeatedly is the
evidence to build it.

## 11. The `retry` command

`RetryProcess` is failed-only (pause/resume split); it re-runs the parent, never the
child — falls out of D7 plus §11.2, no rule of its own needed. One unit of retry: a
process; one restart path: name the root, `revive` walks to what was interrupted.

### 11.1 Retry re-runs the task the instance sits on — no rewind

That fact decides everything. For a **fault** it fits: the cause is at the task. For a
**raise** it usually does not: the deciding state is upstream and persisted, so the
re-run re-raises identically — provably so for a switch-only task, and worse with an
action (which already *succeeded* and would re-execute a side effect). So the line is
fault vs outcome, and status already encodes it: `failed` (authored or engine) retryable,
`raised` not — the root gate keeps its failed-only shape, with a dedicated message for
`raised` ("a declared outcome, not a fault — start a new instance or publish a new
version") because both special cases exist to prevent bug reports, and pause has a verb
to point at while raise does not.

**Panics stay retryable** — `panic` chose `failed` precisely because it means fault;
fix-outside-and-re-enter is what retry is for. The residual (a panic reading stale
upstream state) is retry-has-no-rewind biting, not something panic introduced.

**Rejected refinement:** gating retry on "the task has an action". The proxy is wrong —
`config` is live (re-resolved every tick), so a switch-only panic on `config.psp_enabled`
retries meaningfully after an env flip; a correct gate needs static analysis of what the
expression reads, on an operator-facing API. And futility is not a gating criterion
anywhere else (`output.invalid` retries identically today). The proportionate lever is
information, not refusal.

### 11.2 `raised` is settled work — keep it, never revive it

In `revive`, `raised` joins `completed` ("settled, keep"), not the defensive
live-status arm ("not ours to touch") — the two `return nil`s mean different things.
`raised` means concluded at every depth. Known limitation, stated rather than hidden: a
parent that failed at resolution on an unmatched code re-resolves identically on retry —
the missing rule lives in a version-pinned definition retry does not re-read; the real
fix is a new version. Scoping note worth a comment in code: `revive` reads only the
batch of the node's *current* task, so a batch already routed past is behind the node.

### 11.3 Clear `error_code` and `$error` on revive

All three error slots clear together. A kept `error_code` quietly corrupts the very
column it exists for (a revived-then-completed instance still reports its old death);
a kept `$error` becomes reachable via the batch error route. Clearing cannot break a
valid definition: mustErr/mayErr admits a `$error` read only where an error is present
on every path.

### 11.4 `Status.Terminal()` must include `raised` — a live bug if missed

Revival reconstructs `waiting` vs `collecting` from "after revival, is anything still
active?". Raised children are not revived (§11.2), so if `Terminal()` does not know the
status, the parent parks in `waiting` forever, unrecoverable and unlogged. The fix
belongs in `Terminal()` itself — it answers "is this settled", D4 says yes — and its SQL
mirror is `CountActiveSiblings` (§7.1); the two edits travel together, either alone
hangs a parent.

### 11.5 Naming — resolved

ROADMAP's "retry only for errored processes" meant *failed*; D4 took `raised`, nothing
competes for the word, and the roadmap line now reads as it always meant.

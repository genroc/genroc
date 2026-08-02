# Child → parent error handling

Status: **fully implemented 2026-07-20.** Addresses `ROADMAP.md` → "think about
error handling child -> parent". Revised against the shipped pause/resume design
([pause-resume.md](pause-resume.md)), which was implemented after the first draft
and removed the `cancelled`/`cancelling` statuses two of the arguments below
leaned on.

> **Syntax note.** Examples below predate the `{{ }}` → `${ }` retarget ([typed-values.md](typed-values.md)): read `{{ x }}` as `${ x }` inside a string, or `"$: x"` where the whole leaf is one expression.

All three phases (§10) are built:

1. **Raise and panic** — `raise`/`panic`, the `raised` status, `error_code` on
   every terminal outcome, and all of §11.
2. **Catch (single child)** — M1 LIKE matching, R4/R5, and the resolution step
   in `runChildProcesses`.
3. **Batch resolution** — §5.2 over a multi-child batch, with `$error.child_key`
   / `.child_index` naming which child raised.

A parent now catches a child's raised error through `on_error` rules on the child
task: a matching rule routes it (goto / raise / panic), and no matching rule
degrades the raise to a defect that fails the parent — **carrying the child's own
code and message forward**, so the failure reads as the raise that caused it
rather than a generic collect error. Where the implementation refined a spec
detail, a **Delta** note records it inline. Code:
`internal/model/{instance,wire,definition,validate,logs}.go`,
`internal/engine/{error,advance,collect,child}.go`,
`internal/validation/{validate_children,infer}.go`,
`internal/db/{queries.sql,db_lifecycle.go,db_instances.go,db_claim.go}`,
migration `023_error_code.up.sql`. Tests: `internal/model/fault_test.go`,
`internal/validation/validationtest/child_reachability_test.go`,
`internal/db/dbtest/raise_test.go`, `tests/integration/raise_panic_test.ts`,
`tests/integration/child_catch_test.ts`.

Every decision is now closed:

- **D3** — reachability-only checking, no exhaustiveness. **Confirmed.** The one
  decision that is expensive to reverse: adding exhaustiveness later breaks
  existing definitions, while removing it later breaks nothing.
- **D7** — no parent-side retry. **Confirmed.** Cheap to reverse; §10.1 describes
  the coarse workaround and the signal that would justify building the real thing.
- **§11.3** — revive **does** clear the `$error` context slot, alongside the
  `error` and `error_code` columns.
- **§11.5** — the status is **`raised`**, not `errored`. It pairs with the
  `raise:` clause exactly as `paused` pairs with `pause`, and cannot be misread
  as a synonym for `failed` in the status column that every dashboard keys on.

Three **extensions** to this design were considered and declined, with the
arguments recorded rather than lost: routing on batch shape, a diagnostic payload
on `raise`, and opt-in exhaustiveness over a child's raise set. See
[error-extensions.md](error-extensions.md) — none is accepted, all are additive,
and each carries the trigger that should reopen it.

### What the pause/resume design changed here

Nothing in §0–§6 was invalidated, but three arguments now rest on different
mechanics, and one fix moved:

- §5.2's guarantee that a resolving parent sees only settled children used to be
  argued from `cancelled`. It is now argued from `paused` **not** being terminal:
  `CountActiveSiblings` counts a paused child as active, so a parent holding one
  is never woken to `collecting` at all. The guarantee is stronger, not weaker.
- E1 (child raises under an operator stop) changed from a dead end to a live,
  correct case: the parent resolves on resume (§8).
- §11.4's `anyActive` bug is now fixed by `Status.Terminal()` rather than by
  hand-editing a status list — see there for why that is the better place.

## 0. Governing principle

> **An error is a branch slot, not a value.**

A raised error is a child saying one specific thing:

> *There is a condition I anticipated, it prevents me from finishing, and I want
> my parent to be able to react to it.*

That is the whole contract. It carries a code so the parent can branch on it and
a message so a human can read it — nothing else. If the parent needs data back,
it was not an error; it was an outcome, and it belongs in `output`.

Two corollaries drive every rule below.

**Anything unanticipated panics.** An uncaught action failure, a bad config, a
missing definition, a bug — the process fails immediately and takes its tree with
it. There is no catchable form of it, no flag, no wildcard. The only way to make
a failure reactable is to convert it into a raise *inside the child*, at the task
that actually understands what went wrong (§5.4).

A definition can say this deliberately too. Three clauses terminate a process,
and they cover the outcome space exactly once each:

| clause | status | meaning | caller can react |
|---|---|---|---|
| `goto: end` | `completed` | finished; produces `output` | — |
| `raise: {code, message}` | `raised` | an anticipated condition stopped me | **yes**, by naming the code |
| `panic: {code, message}` | `failed` | something is wrong that I did not anticipate | **no**, ever |

Both failure clauses carry a code, but for different readers. A raised code is
consumed *inside* the tree, by a parent that branches on it. A panic code is
consumed *outside* it — by the API, dashboards and alerting, which need a stable
discriminator to count and filter on. Reactability and classification are
separate concerns, and only the first is denied to a panic.

**Errors are for branching, not for retrying around.** Transient conditions
belong to the child, which retries them internally and raises only once it has
given up. An error that crosses a process boundary describes a *settled*
condition, so there is no parent-side retry at all (D7) — and "catch everything
and retry" is not expressible here.

This does **not** replace the `{ok: false, reason: …}` output convention; the two
coexist for different jobs. "8 of 10 shipped, and here is why 2 didn't" is a
*result* and stays in child output. Errors are for control flow only.

## 1. Vocabulary

| term | meaning |
|---|---|
| **batch** | all children spawned by one `(parent_id, spawn_task_id)` pair |
| **slot** | the abstract term for a stable position in a batch; surfaced in `$error` as `child_key` (a string, `child_map`) or `child_index` (an integer, `child_list`) |
| **raise** | a child terminating via a `raise` clause → status `raised` |
| **panic** | a child terminating via a `panic` clause → status `failed`; an *authored* defect |
| **defect** | any termination as `failed`, authored or engine-produced — never catchable |
| **raise set** | `raises(D)`, the codes a definition `D` can raise (§2.3) |
| **child task** | a task whose action is `child_map` or `child_list` |
| **resolution** | the parent's decision procedure over a settled batch (§5.2) |

## 2. Surface syntax

### 2.1 `raise` — an explicit clause, not a goto

A `raise` clause terminates the process with an error. It is a **field, not a
goto target**, so the code and its message are structurally paired and neither
can drift from the other:

```yaml
name: charge-card
tasks:
  - id: charge
    action: { type: fetch, url: "{{ config.psp_url }}/charge", result_schema: { … } }
    on_error:
      - code: ["http.402"]
        raise:
          code: card_declined
          message: "the issuer declined the charge"
    switch:
      - case: "self.result.status == 'no_funds'"
        raise:
          code: insufficient_funds
          message: "the account has insufficient funds"
      - goto: end
```

```go
// Fault is a terminal error: a machine-readable code and a human-readable
// message, both static. Used by both `raise` and `panic` — they differ in what
// they do, not in what they carry.
type Fault struct {
    Code    string `json:"code"    validate:"required" description:"Error code, lower_snake_case, no dots. A literal — never an expression."`
    Message string `json:"message" validate:"required" description:"Human-readable message. A literal string — never an expression."`
}
```

One shared type rather than two identical ones: they carry the same thing for
the same reasons, and two structs that must stay in lockstep is a drift hazard
for no gain. The distinction lives in the field name at the use site
(`Raise *Fault` / `Panic *Fault`), which is where a reader looks anyway.

`raise` is valid on a `SwitchCase` and on an `ErrorCase`, and is **mutually
exclusive with `goto`** on both (R3): a case either routes or raises.

`message` is **required** (R1). One line of prose per failure mode is cheap, and
it is what an operator reads off the instance detail before anything else.

Both fields are literals. An expression in either would re-open the data channel
this design exists to close (R2).

### 2.2 `panic` — authoring a defect

`panic` terminates the process as `failed`. It is the definition-level form of
everything in §0's first corollary: *I have detected something I did not
anticipate, and nothing downstream should try to work around it.*

```yaml
  - id: submit
    action:
      type: fetch
      url: "{{ config.api }}/submit"
      result_schema: { … }
    switch:
      - case: "self.result.status == 'rejected'"
        raise:
          code: submission_rejected      # anticipated — the caller may react
          message: "the service rejected the submission"
      - case: "self.result.error != null"
        panic:                                    # not anticipated
          code: submit_contract_violation
          message: "the service returned 200 with an error body"
      - goto: end
```

`panic` carries the same `Fault` as `raise`. Its code is not there to be caught —
nothing can catch a panic — but to be **classified** by everything outside the
process tree: `error_code` is a filterable column, so an operator can count
`submit_contract_violation` across instances, alert on it, or spot a spike after
a deploy. Burying that in free text would make the API strictly less useful for
the failures people most need to see.

This is the case the HTTP layer cannot see for you: a `200` with an error body
satisfies `accepted_status`, so no `http.NNN` code is ever produced and
`on_error` never fires. Only the switch, reading the parsed body, can tell that
the call actually failed — and only the author knows whether that particular body
is an anticipated condition (`raise`) or a broken contract (`panic`).

`panic` is valid wherever `raise` is — a `SwitchCase` or an `ErrorCase` — and is
mutually exclusive with both `goto` and `raise` (R3). In an `on_error` rule it is
mostly a way to *document* a fatal code ahead of a more permissive later rule, or
to replace the engine's generic failure text with something a human can act on:

```yaml
    on_error:
      - code: ["http.401"]
        panic:
          code: bad_credentials
          message: "credentials rejected — check GENROC_<proc>_API_TOKEN"
      - code: ["http.5%"]
        retries: 3
        goto: $fallback
```

**Why "panic" and not "fail".** The status it produces is `failed`, so the names
do not match — deliberately. `panic` is alarming, and it should be: it is
uncatchable and it takes the whole tree down. A milder spelling would invite
reaching for it instead of thinking about whether the condition deserves a
`raise`.

### 2.3 The raise set is inferred

There is no `errors:` declaration block. A process's raise set is derived by a
purely syntactic scan:

```
raises(D) := { f.Code : f is the Fault of a *raise* clause on any
                        SwitchCase or ErrorCase of any task of D }
```

No dataflow, no fixpoint — `Fault.Code` is a plain field. The set is *statically
exact*, and where imprecise it is imprecise in the safe direction: a `raise` on
an unreachable task inflates the set, never the reverse.

**Panic codes are excluded, even though panics have codes.** `raises(D)` is the
set a parent may write rules against (R5), and no rule can ever match a panic —
a panicking child is `failed`, so it poisons its ancestors and the parent never
reaches resolution at all. Including panic codes would let R5 bless rules that
can never fire. A panic code is for observers; the raise set is for callers.

**Cost: discoverability.** "What can this process raise?" is no longer answerable
by reading the top of the file. The server closes this by computing `raises(D)`
at registration and exposing it on the definition endpoint and in the generated
schema — a first-class property of a published version that nobody authors
directly.

### 2.4 Parent side — no prefix, no new syntax

```yaml
- id: pay
  action:
    type: child_map
    children:
      charge: { name: charge-card, input: { … }, result_schema: { … } }
  on_error:
    - code: ["card_declined"]
      goto: $offer_alternative
    - code: ["insufficient_funds"]
      goto: $notify_customer
  switch: next
```

Raised codes need no `child.` namespace, because **on a child task there is
nothing else `on_error` can ever see.** The other paths a `child_map` /
`child_list` task can fail on — input validation, definition lookup, spawn,
collect-time output validation — all go straight to `failInstance`
([child.go:170-183](internal/engine/child.go#L170-L183),
[child.go:28](internal/engine/child.go#L28)) and are not routed through
`on_error` at all. So a child task's `on_error` list is, by construction, a list
of raised codes.

They are also lexically distinct from engine codes at a glance: R1 forbids `.` in a
raised code, while every engine code contains one (`http.500`, `pre.timeout`,
`external.timeout`, `output.invalid`). So a raised code is always an authored,
semantic name — a child that catches an engine failure re-raises it under a *new*
code that says what it means (`psp_unavailable`, not `http.503`), which is the
point of raising rather than letting the failure propagate.

**Propagation is explicit.** A parent re-raising a child's error is just a `raise`
in an `on_error` rule — and that is what puts the new code into *this* process's
raise set:

```yaml
  on_error:
    - code: ["card_declined"]
      raise:
        code: payment_failed
        message: "payment could not be completed"
```

## 3. Static semantics (registration)

Each rule states its rejection message verbatim.

**R1 — fault shape.** On every `Fault` — under `raise` or `panic` alike —
`Code` matches `^[a-z][a-z0-9_]*$` (lower_snake_case) and `Message` is non-empty.
The two excluded characters each carry meaning that keeps them out of a code:
`.` is how engine-produced codes are spelled — forbidding it keeps authored codes
lexically distinct *and* stops a raise from just mirroring a system code (an
authored error should carry its own semantic name, `card_declined`, not re-raise
`http.503`); `%` is the `on_error` match wildcard (§4), so keeping it out means a
code never contains a character that has meaning in a pattern, and no escaping is
ever needed.
> `task "charge": raise: "Card-Declined" is not a valid error code (lower_snake_case, no dots)`
> `task "charge": raise: "http.503" must not contain '.' — dots are reserved for engine-produced codes; give the error a semantic lower_snake_case name of its own rather than re-raising a system code`
> `task "charge": panic: "rate%exceeded" must not contain '%' — it is the on_error match wildcard, so a code containing it could never be caught`
> `task "charge": raise "card_declined": message is required`

**R2 — faults are static.** Neither `Code` nor `Message` contains `{{ }}`. A
computed code would make `raises(D)` uncomputable and `error_code` unqueryable;
a computed message would smuggle data across the boundary.
> `task "charge": raise: code must be a literal, not an expression`
> `task "submit": panic: message must be a literal, not an expression`

**R3 — one terminal clause.** A `SwitchCase` carries exactly one of `goto`,
`raise`, `panic`.
> `task "charge": switch case 0: set exactly one of "goto", "raise", "panic"`

> **Delta (implemented).** On an **`ErrorCase`** the rule is *at most* one, not
> exactly one. A rule with none of the three is meaningful and predates this
> design: it exhausts its `retries` and then fails the instance with the engine's
> own code. Only "two answers to what this rule does" is rejected.
> `task "pay": on_error[0]: set at most one of "goto", "raise", "panic"`
>
> Both checks live in the validator, not in `UnmarshalJSON` — the decoder only
> checks a `goto`'s *shape* — so the rejection can name the task and the case
> index instead of surfacing as an opaque decode error.

**R4 — child tasks do not retry.** On a task whose action is `child_map` or
`child_list`, `on_error` codes are patterns matched against the child's raised
codes — the same syntax as an action task's on_error (`%` the only wildcard, §4).
`retries` and `not_reached` are rejected: there is no parent-side retry (D7), and
accepting a field that is silently ignored would be worse than refusing it.
> `task "pay": on_error[0]: retries is not supported on a child task; retry inside the child, then raise`
> `task "pay": on_error[0]: not_reached has no meaning on a child task`

> **Delta (implemented).** The draft made child on_error codes *literal* (no
> patterns, no catch-all), matched by a dedicated `matchOnErrorLiteral`. That was
> **reversed**: child on_error now uses the same patterns and the same
> `matchOnError` as an action task, so `order_%` catches `order_placed`. The safety
> the literal rule was reaching for is provided instead by R5 below: a pattern is
> validated at registration against the child's *known, finite* raise set, so a
> pattern that can never match is still caught — but a pattern that *can* match is
> allowed. (The original literal-only rule existed partly to dodge SQL LIKE's `_`
> single-char wildcard clashing with underscores in codes — that clash was removed
> at the source instead, by dropping `_` as a wildcard: see §4, M1.)

**R5 — rule reachability.** On a child task, every code *pattern* an `on_error`
rule names must match at least one code in `⋃ raises(D)` over the task's resolved
children (the union across entries, for `child_map`). A catch-all (empty list)
matches everything and is exempt.
> `task "pay": on_error[1]: no child of this task can raise a code matching "card_%"`

> **Delta (implemented).** Generalized from *literal membership* (each code is in
> the raise set) to *pattern match* (each pattern matches some code in the raise
> set), using the runtime `transport.MatchCode`. This is what lets R4 safely
> allow patterns: the child raise set is exact and finite, so "does `fourth_%`
> match anything this child can raise" is a decidable, precise check. It also
> subsumes the old special-cased `child.failed` rejection — a child that raises
> nothing has an empty raise set, so *any* pattern against it is unreachable and
> rejected, which is the same "you cannot catch a failure, only a raise" outcome
> reached by a general rule instead of a named one.

**R6 — a code is a raise code or a panic code, not both.** Within one definition,
no code appears on both a `raise` and a `panic`. Otherwise `error_code` would be
ambiguous for exactly the observers it exists to serve: the same value would
appear on `raised` and `failed` instances of the same process, meaning two
different things.
> `task "submit": panic "timeout": this code is already raised by task "poll"; a code cannot be both raised and panicked`

R5 lands next to `validateChildEntry`
([validate_children.go](internal/validation/validate_children.go)), which already
resolves the child definitions to subset-check their inputs, so their task lists —
and hence `Raises()` — are in hand.

> **Delta (implemented).** R5 is its own pass, `validateChildOnErrorReachability`,
> run per task inside `ValidateChildProcessRefs` rather than folded into the
> per-entry input check. The reason is scope: reachability is a property of the
> *task's whole rule set against the union of its children's raise sets*, not of a
> single `child_map` entry, so it reads more clearly as one check over the task
> than as something threaded through the per-entry loop. It reuses `resolveChild`
> and `ProcessDefinition.Raises()`, so it resolves the same versions the input
> check does.

### 3.1 What R5 does and does not catch

R5 is a **sanity check, not a coverage guarantee.** It runs in one direction
only — from rule to raise set:

| change | caught at registration? |
|---|---|
| rule names a code with a typo (`card_decline`) | **yes** |
| a code is *removed* from a child, orphaning a rule | **yes**, when the parent re-registers against the new version |
| a code is *added* to a child, with no rule for it | **no** — surfaces at runtime |

The third row is the deliberate cost. An unhandled raised code reaches step 2 of
resolution with `rule = nil` and **fails the parent** (§5.2). Version pinning
(`GetDependencyVersion`, [child.go:116](internal/engine/child.go#L116)) bounds the
blast radius: a running parent keeps its pinned child version, so a new code
reaches it only after someone deliberately bumps the dependency and
re-registers — a runtime failure on a new pairing, not a retroactive break of
what is already deployed.

Requiring every raisable code to be handled was considered and rejected: it makes
a shared child process painful to depend on, since any new raise site anywhere in
it would break re-registration for every parent. See D3.

## 4. Matching

**M1 — pattern matching on child tasks.**

> On a child task, a rule matches **iff one of its `code` patterns matches the
> raised code** (or the rule is an empty-list catch-all) — the same `matchOnError`
> an action task uses. The only wildcard is `%` (any run of characters); every
> other character, `_` and `.` included, is literal.

R5 rejects at registration every pattern that could never match a raise, so a rule
reaching this point can always match a code the child actually raises. Non-child
tasks use the identical matcher for `http.*` / `pre.*` / `external.timeout` /
`script.*` / `output.*`.

> **Delta (implemented).** Two reversals, one after the other. The draft matched
> child codes *literally* via a separate `matchOnErrorLiteral`, to avoid a raised
> code's underscores being read as SQL LIKE `_` wildcards (`card_declined`
> spuriously matching `cardxdeclined`). That was first replaced with shared SQL
> LIKE — which reintroduced exactly that footgun: `order_%` matched `order.placed`,
> because `_` matched the `.`. The fix that stuck removes the wildcard at its
> source: the matcher (`transport.MatchCode`) makes **`%` the only wildcard** and
> treats `_` and `.` as literals, since both are ordinary characters in an error
> code. So `order_%` matches `order_placed` but not `order.placed`, and child and
> action tasks share one intuitive matcher. This is not full SQL LIKE, and the
> `ErrorCase.Code` description says so.

## 5. Operational semantics

### 5.1 Child: raising

A `raise` clause is a sibling of the `GotoEnd` branch in `advance()`
([advance.go:261-270](internal/engine/advance.go#L261-L270)), and of the
`GotoEnd` branch in `handleCallError` for the `on_error` form
([error.go:71-76](internal/engine/error.go#L71-L76)):

```
raise:                            panic:
  status     := raised              status     := failed
  error_code := fault.Code          error_code := fault.Code
  error      := fault.Message       error      := fault.Message
  wake_at    := nil                 wake_at    := nil
```

The two write identical fields and differ only in status — which is the whole
difference, since status is what decides whether ancestors are poisoned.

**Neither needs new plumbing in `saveAndNotify`**
([advance.go:416-424](internal/engine/advance.go#L416-L424)), which already
branches exactly where this design needs it to:

- `panic` is `failInstance` ([error.go:92](internal/engine/error.go#L92)) with an
  authored reason and code, so it takes the existing `Status == StatusFailed`
  branch to `FailInstanceAndAncestors`.
- `raise` writes `StatusRaised`, which is not `StatusFailed`, so it falls through
  to **`FinishChild`** — the correct destination, because a raise is a normal
  outcome and must not mark ancestors `failing`. Nothing there changes.

> **Delta (implemented).** `raise` and `panic` are `raiseInstance` /
> `panicInstance` in `error.go`, called from both the switch path (`advance.go`)
> and the `on_error` path (`handleCallError`). `panicInstance` is literally
> `failInstance(inst, f.Code, f.Message)` — a panic is a defect like any other, so
> it *is* the failure path with the author's words substituted, not a parallel to
> it. To make that substitution total, `failInstance` gained a `code` parameter:
> every one of its ~30 call sites now passes an `engine.*` code (see §7.1's
> Delta), so no failure path can leave `error_code` empty by omission.

`StatusRaised` is directly terminal; no draining state is needed, because a
raise happens at a task boundary where this instance's own children have already
collected. It must be added to `Status.Terminal()`
([instance.go:30](internal/model/instance.go#L30)) — see §11.4, where that one
line also closes a live bug in `RetryProcess`.

**Neither computes the process `output`.** This matters at registration as well
as at runtime: `addTerminal` ([context.go:63-88](internal/validation/context.go#L63-L88))
must be reached only for `goto: end` cases, never for `raise` or `panic`. A raise
site is *not* a terminal for the purpose of validating the process `output:`
expression — otherwise raising from a task where the outputs `output:` reads are
not yet available would fail registration, which is precisely the situation
every early-exit raise is in.

> **Delta (implemented).** No change was needed here. `addTerminal` and its
> predecessor graph `buildPreds` already branch on `c.Goto == GotoEnd` /
> `ec.Goto`, and a `raise`/`panic` case has an empty `Goto`. So a terminal clause
> is invisible to the output-boundary analysis for free — the spec's requirement
> was already satisfied by keying on `Goto` rather than on "does this case end the
> process".

### 5.2 Parent: resolution

**Precondition:** `P.status = running ∧ P.wait_state = collecting`. Both other
live statuses short-circuit earlier in `advance()`
([advance.go:139-152](internal/engine/advance.go#L139-L152)): a `failing` parent
goes to `settleFailing`, a `pausing` one to `settlePausing`. A `paused` parent is
not claimable at all. So a suspended parent parked on `collecting` resolves when
it is resumed, never before.

That precondition has a strong consequence — **a parent that reaches resolution
has a batch of only `completed` and `raised` children**, so the algorithm never
has to reason about defects or suspensions at all. The three excluded statuses
are each blocked by a different mechanism:

| child status | why the parent cannot be resolving | mechanism |
|---|---|---|
| `failed` | it poisoned its ancestors first, so `P` is `failing` | §5.4 |
| `paused` / `pausing` | a paused child still counts as active, so `P` was never woken | `CountActiveSiblings` ([queries.sql:201](internal/db/queries.sql#L201)) |
| `running` | the batch is not settled | `CountActiveSiblings` |

The middle row is what the removal of `cancelled` changed. An earlier draft
argued it from cancellation coming down from the root; the pause design gives a
stronger guarantee, because it does not depend on where the stop originated.
A paused child holds its parent in `waiting` no matter who paused it.

```
resolve(parent P, task T):
  B := children of batch(P, T.id), ordered by slot
       -- child_list: by _spawn_index;  child_map: by sorted _spawn_child_key
  assert ∀c ∈ B : c.status ∈ {completed, raised}

  E := [ c ∈ B : c.status = raised ]                    -- keeps slot order

  ── 0. happy path ────────────────────────────────────────────────────────
  if E = ∅:
      collect outputs as today → self.result;  continue advance()

  ── 1. match ─────────────────────────────────────────────────────────────
  for c ∈ E:
      c.rule := matchOnError(T, c.error_code)            -- literal; nil if none

  ── 2. route ─────────────────────────────────────────────────────────────
  f := E[0]                                              -- first in slot order
  write $error from f                                    -- §5.3
  match f.rule:
      nil, or no goto/raise → fail P
      goto = "end"          → complete P
      raise                 → raise from P               -- §5.1
      goto = "$id"          → P.task := id;  continue
```

Two steps, no retry, no loop. Ordering is by slot, already established by
`buildListChildOutput` / `buildMapChildOutput`
([collect.go:40-72](internal/engine/collect.go#L40-L72)). Nothing reads a clock
or observes completion order, so resolution is deterministic (**I3**).

**Where it lives.** Resolution is a new step at the head of `runChildProcesses`
phase 2 ([child.go](internal/engine/child.go)), *before* the collect — not inside
it. `runChildProcesses` does the one `ChildrenForTask` read and hands the siblings
to `resolveRaisedBatch` (if any raised) or to `buildChildOutput` (the old
`collectChildOutputs` body, minus the read). `buildChildOutput` keeps its strict
every-child-completed guard: by the assertion above, a `raised` child reaching it
is a bug in resolution, not a case to accommodate.

Step 2's `rule = nil` branch is the **normal** path for an unhandled raised code,
not an edge case — R5 does not require coverage (§3.1). Its message must name the
code, the child and the slot, so the omission is readable straight off the
instance detail:
> `task "pay": child "charge-card" (child_key "charge") raised "insufficient_funds": the account has insufficient funds`

Note what this means for composition: **an unhandled raise degrades to a defect.**
The parent fails, which fails fast up its own tree. An error never propagates a
level implicitly — propagation is a `raise` in an `on_error` rule and nothing
else.

> **Delta (implemented).** Two refinements to step 2, both about the failed
> parent *mirroring* the child that caused it:
>
> - **`error_code` is the child's raised code**, not `engine.collect`. The
>   pseudocode's `fail P` is `failInstance(inst, first.ErrorCode, msg)`, so a
>   parent that dies on an unhandled `insufficient_funds` reports `error_code =
>   insufficient_funds` and a message that quotes the child's own message. Before
>   resolution existed, this same case surfaced as the generic collect-guard
>   failure (`engine.collect`, "outputs can only be collected when all children
>   completed"), which read like an internal bug rather than "a child raised and
>   nobody caught it". Mirroring the child is the whole point of the fix.
> - **`on_error` may `raise` or `panic`, not only `goto`.** The pseudocode lists
>   `nil / goto:end / raise / goto:$id`; the implementation also honours a `panic`
>   clause on the matched rule (fail with the authored panic code), for parity with
>   the switch/`handleCallError` paths where a rule can carry any terminal clause.
> - **`goto:end` computes the process output**, like a normal completion. Both
>   error-completion paths — this one and the action-task `on_error → end` in
>   `handleCallError` — share `completeViaErrorHandler`
>   ([error.go](internal/engine/error.go)), so the output is computed identically
>   and the two cannot drift. (`handleCallError`'s `end` branch previously skipped
>   `computeOutput`, so a process completing via an action-task `on_error → end`
>   silently produced no `output`; unifying on the helper fixed that.)

### 5.3 What the routed task sees

`$error` (the existing `error_data` context slot). A `child_list` fan-out:

```
{ task:        "fanout",
  code:        "out_of_stock",
  message:     "no stock remaining for this sku",
  child_index: 3 }                    -- a child_map child would carry child_key: "charge"
```

Identifying the child costs nothing — the engine already knows which one raised —
and it is what makes a payload-free design workable for fan-outs: the child never
says *which one*, because its identity is structural. It is identity and code
only; no child data crosses (**I6**).

A `child_map` child is named by a string **`child_key`**, a `child_list` child by
an integer **`child_index`** — two separate single-typed fields, not one
`string|integer` value, so a handler reads exactly the one its batch kind produces
without a type-switch. Exactly one is present.

`$error` reports **one** raise — the first in child-key order — not the whole set
that raised. When several children in a batch raise, the first drives the parent
and the rest are not enumerated (see D2 for why an aggregate `siblings` list was
cut). A fan-out that needs "which of the 10 failed, and why" is describing a
*result*, not control flow, and belongs in child `output` via the
`{ok: false, reason}` convention (§0), where every child completes and the parent
collects all ten.

**The routed task keeps its normal context.** `input`, `config`, and every
`outputs.<id>` from previously completed tasks are readable as usual. The single
thing absent is `outputs.<T.id>` — the failed batch never produced one. That is
the whole of "no partial data": there is no half-populated output, not that the
handler is blindfolded.

Typing follows in `contextSchema` ([infer.go](internal/validation/infer.go)):
`child_key` and `child_index` are both optional there, since a plain
action-task `on_error` leaves them absent and the schema can't tell which
`on_error` produced a given `$error`. *Presence* of `$error` itself is already
computed by the existing mustErr/mayErr dataflow
([context.go:337-400](internal/validation/context.go#L337-L400)) and needs no
change.

> **Delta (implemented).** Two changes to the `$error` shape from the draft:
>
> - **`slot` → `child_key` / `child_index`.** The draft carried a single `slot`
>   field shaped `{key}` / `{index}`. Renamed to a flat scalar and split by kind:
>   **`child_key`** (string, `child_map`), **`child_index`** (integer,
>   `child_list`). "slot" read as internal jargon, and a nested one-field object is
>   awkward to write an expression against (`error.slot.index`);
>   `error.child_index` is direct, and two single-typed fields keep each one
>   monomorphic — a handler never asks "string or int". "slot" survives in this doc
>   only as the *abstract* term for a batch position (slot order, per-slot).
> - **`siblings` removed.** The draft's `siblings` array (every raised child in the
>   batch) was cut — see the revised D2. `$error` now reports only the first raise.

### 5.4 Defects fail fast, always

A child that *fails* rather than raises is **not catchable, under any
configuration** — and that holds identically whether the failure was authored
with `panic` or produced by the engine. It calls `FailInstanceAndAncestors`
([db_lifecycle.go:83](internal/db/db_lifecycle.go#L83)) exactly as today:
ancestors go `failing`, the batch is abandoned, the parent settles without ever
resolving.

There is no `child.failed` code, no opt-in flag, no per-spawn stamp. This is the
first corollary of §0 made structural rather than merely discouraged: the only
way to make a failure catchable is to convert it into a raise *inside the child*,
at the specific task where you understand what went wrong —

```yaml
    on_error:
      - code: ["http.503"]
        retries: 3
        raise:
          code: psp_unavailable
          message: "the payment provider is unavailable"
```

— which is exactly the reasoning a blanket catch-and-retry lets you skip. Note
that `retries` here is on a *fetch* task inside the child, where it has always
been legal and where transient failures actually belong.

An unhandled defect in one slot therefore dominates a sibling's tidy raise in
another: the parent goes `failing` and the raise is never routed. Correct — a
fault must not be masked by a business error that happened to occur beside it.

## 6. Invariants

- **I1 — all-or-nothing.** `outputs[T.id]` is written only when every child of
  the batch is `completed`. There is no partially populated batch output.
- **I2 — single observation.** A parent resolves a batch exactly once, when
  every child is terminal — and every one of them is then `completed` or
  `raised` (§5.2).
- **I3 — determinism.** Resolution is a pure function of `(T, slot-ordered
  children)`. No clock, no completion-order dependence, no race.
- **I4 — crash safety.** From I3, re-running resolution over the same rows
  yields the same decision, so a crash between decision and persist is safe and
  a reclaimed parent resumes identically.
- **I5 — caller independence.** A child's terminal status, *and its effect on its
  ancestors*, are fully independent of who spawned it. Nothing about a child's
  termination is parameterised by its parent.
- **I6 — no data crosses.** After an error route, the only child-derived values
  in the parent's context are code, message and which child (`child_key` / `child_index`).

## 7. Data model

The runtime footprint is deliberately small — one column, one status, one
predicate. The authoring footprint is larger than it looks, because the switch
and on_error wire formats are hand-written (§7.2).

**Migration `023_error_code.up.sql`:**
```sql
ALTER TABLE process_instances ADD COLUMN error_code TEXT NOT NULL DEFAULT '';
```
Filterable, not sortable — so no index is required (only sorts must be
index-backed; see `paginate.go`). No data migration: every existing row takes the
default, which is exactly what §7.1's table says a pre-existing `completed` row
should carry, and what an old `failed` row honestly reports — its code was never
recorded anywhere but the prose in `error`.

> **Delta (implemented).** `NOT NULL DEFAULT ''`, not a nullable `TEXT`. The
> column sits beside `error`, which is already `NOT NULL DEFAULT ''`, so "no code"
> is one value (`''`) rather than two (`NULL` and `''`) for every filter, scan and
> Go zero-value to handle. `ProcessInstance.ErrorCode` is a plain `string`
> throughout; the §7.1 table's "null" rows read as `''`.

### 7.1 `error_code` is the discriminator for every non-success outcome

Once panics carry a code, `error_code` should be populated on *every* terminal
failure, not just authored ones:

| terminal state | `error_code` | source |
|---|---|---|
| `completed` | `''` | — |
| `raised` | the raised code | `raise` |
| `failed`, authored | the panic code | `panic` |
| `failed`, engine | the engine code — `http.500`, `pre.timeout`, `output.invalid`, `engine.*` … | `handleCallError` / `failInstance` |

The last row is a small extension beyond `raise`/`panic`, and it is where most of
the operational value is: an engine failure's code used to exist only inside the
`error` text, formatted into `task %q: %s: %s`
([error.go:87](internal/engine/error.go#L87)), so it could not be grouped or
alerted on without parsing prose. Populating the column makes *all* failures
uniformly queryable.

> **Delta (implemented).** "The engine code" is two families, not one. Failures
> that flow through a *call* carry the call's own code (`http.500`, `pre.timeout`,
> `output.invalid`, `script.1` …), which `handleCallError` already had in hand.
> But the engine also fails an instance for reasons that never touched a call —
> an unevaluable expression, a bad config, a missing definition, a spawn or
> collect error, an interrupted `only_once` task. Those went through
> `failInstance` with a message but no code. So `failInstance` now takes a `code`,
> and there is a small closed set of `engine.*` codes for exactly these cases:
> `engine.expression`, `engine.config`, `engine.definition`, `engine.input`,
> `engine.spawn`, `engine.collect`, `engine.only_once`
>
> (`engine.only_once` was later replaced by `only_once.interrupted`, which is
> **catchable** — the one engine-produced code `on_error` may route. See
> [only-once-interrupted.md](only-once-interrupted.md).)
> ([error.go](internal/engine/error.go)). They carry a dot like every other engine
> code, so R1's namespace split still holds.

The two namespaces stay legible side by side because R1 forbids dots in authored
codes and every engine code has one: `card_declined` is something a definition
decided, `http.401` is something the engine observed. A filter never has to
disambiguate them, and a raised code is always an authored, semantic name rather
than a re-raised system one.

**Exactly one status predicate changes.** The engine keeps four separate copies
of "which statuses are live", and it is worth stating that only one of them has
an opinion about `raised`, because the other three are easy to change by reflex
and wrong to:

| predicate | change |
|---|---|
| `CountActiveSiblings` ([queries.sql:201](internal/db/queries.sql#L201)) | **`NOT IN ('completed', 'failed', 'raised')`.** Without it the parent never wakes and the batch hangs. |
| `ClaimInstances` ([db_claim.go:73](internal/db/db_claim.go#L73)) | none — it whitelists `running`/`failing`/`pausing`, so a `raised` row is already unclaimable. |
| `FailAncestors` ([queries.sql:255](internal/db/queries.sql#L255)) | none — it whitelists `running`/`pausing`/`paused`. A settled `raised` row must not be reopened into `failing`. |
| `WakeParent` ([queries.sql:212](internal/db/queries.sql#L212)) | none — it tests the *parent's* status, which is never `raised` while a child is live. |

Note the asymmetry between rows 1 and 3: `raised` is terminal for the purpose of
"is this batch settled" but is not a failure, so it neither poisons upward nor is
poisoned from above. That is the whole status in one line.

Other query changes: `GetChildrenForTask`, `instanceColumns`,
`instanceSummaryColumns` and `scanInstance` gain `error_code` in the projection.

**New context keys:** none.

**Go:**
- `model.Fault`; `SwitchCase.Raise` / `.Panic`; `ErrorCase.Raise` / `.Panic`;
  `model.StatusRaised`.
- `Status.Terminal()` ([instance.go:30](internal/model/instance.go#L30)) gains
  `StatusRaised` — see §11.4.
- `ProcessInstance.ErrorCode`; `InstanceSummary.ErrorCode`.
- `model.ProcessDefinition.Raises()` — the §2.3 scan, used by R5 (phase 2) and the
  definition endpoint. Panics are not included.
- `addTerminal` ([context.go:63-88](internal/validation/context.go#L63-L88)):
  reached only for `goto: end`, not for `raise` / `panic` (§5.1) — no change
  needed, it already keys on `Goto`.
- `RetryProcess` ([db_lifecycle.go:424](internal/db/db_lifecycle.go#L424)): four
  changes, see §11.
- `collectChildOutputs` → split into `buildChildOutput(task, siblings)` +
  `resolveRaisedBatch` ([collect.go](internal/engine/collect.go)); the strict
  every-child-completed guard stays (§5.2).
- child tasks share `matchOnError` ([error.go](internal/engine/error.go)), M1; `Raises()`
  reachability pass `validateChildOnErrorReachability`
  ([validate_children.go](internal/validation/validate_children.go)), R5; the
  `$error` schema gains `child_key` + `child_index`
  ([infer.go](internal/validation/infer.go)), §5.3.
- API and CLI: `raised` in the status enums
  ([actions.go:169](internal/api/actions.go#L169),
  [handlers_types.go:119](internal/api/handlers_types.go#L119),
  [commands.go:436](cmd/genctl/commands.go#L436)); `error_code` added to
  `instancePaginator.filterCols` ([db_instances.go:29](internal/db/db_instances.go#L29))
  and to the instance detail + summary responses; the raise set on the definition
  list.

> **Delta (implemented).** "The definition endpoint" is the definition **list**
> (`GET /definitions` → `DefinitionSummary.raises`), not a separate detail
> endpoint — there is no single-definition JSON endpoint; the only per-definition
> route is the OpenAPI spec (`ProcessSpec`). `error_code` is on **both** the
> instance detail and the list summary (`InstanceSummaryResp`), against the
> "listing stays light" rule, because a code is exactly what a list is scanned for
> when something failed, and it is a short bounded string. The instance list gains
> an `error_code` filter param alongside the `status` one; `genctl instances` gets
> a `CODE` column and an `--error-code` flag, and `genctl get` prints `Code:`.
>
> One OpenAPI-plumbing change came with `Fault`: the spec builder rewrote only the
> `#/$defs/ModelShape` ref prefix, so the new `#/$defs/ModelFault` ref would not
> have resolved. It now rewrites the shared `#/$defs/Model` prefix, so any future
> hand-written schema that refs a model type resolves with no further edit
> ([openapi.go](internal/api/openapi.go)).

**Untouched:** `internal/schema`, `internal/expression`, `internal/template`, and
every sibling query beyond `CountActiveSiblings`.

### 7.2 The wire format is hand-written, and that is where the work is

`SwitchCase` and `ErrorCase` do not use struct tags for their wire form. Each has
a hand-written companion struct plus a hand-written `JSONSchemaBytes` blob that
OpenAPI reflects, and the three must be changed in lockstep:

- `switchWireCase` ([wire.go:31-34](internal/model/wire.go#L31-L34)) declares
  `Goto` as `json:"goto"`, and `SwitchMap.UnmarshalJSON`
  ([wire.go:68-73](internal/model/wire.go#L68-L73)) **rejects an entry without
  one**. R3 (exactly one of `goto`/`raise`/`panic`) therefore has to move out of
  the unmarshaler and into the validator, where it can produce the message §3
  specifies instead of a decode error.
- `SwitchMap.JSONSchemaBytes` ([wire.go:81-104](internal/model/wire.go#L81-L104))
  hardcodes `"required": ["goto"]` and `"additionalProperties": false`. Both must
  change, or every definition using `raise` fails schema validation at the edge
  before the validator ever sees it.
- `errorCaseWire` ([wire.go:211-216](internal/model/wire.go#L211-L216)) needs the
  same two fields.
- `ErrorCase.Code`'s description ([wire.go:203](internal/model/wire.go#L203))
  currently ends *"child.failed cannot be caught here — handle errors inside the
  child process and communicate them via return data."* Still true (D5), but it
  now has a name and a mechanism: it should point at `raise`.

None of this is deep, but it is four files' worth of edits that the "one column,
one status" framing hides, and the OpenAPI blob in particular fails in a place
far from the change.

## 8. Edge cases

| # | case | resolution |
|---|---|---|
| E1 | child raises while the tree is paused | child goes `raised`; `FinishChild` still arms the paused parent for `collecting` ([queries.sql:212](internal/db/queries.sql#L212) includes `paused` deliberately), but the parent is unclaimable, so the routing decision is simply **deferred to the resume**. Nothing is lost and nothing is decided early |
| E1b | pause arrives while the parent is mid-resolution | the worker holds the row, so `PauseProcess` can only mark it `pausing`; the routing write lands the pause via the `CASE` in `UpdateInstance` (pause-resume §3). The parent settles into `paused` already pointed at the goto target, and continues there on resume |
| E2 | parent already `failing` | short-circuits to `settleFailing`; resolution never runs |
| E3 | batch mixes a raise and a defect | defect wins: it poisons ancestors first, so the parent never resolves and the raise is never routed (§5.4) |
| E3b | child `panic`s | identical to any other defect — `failed`, ancestors poisoned, uncatchable. The authored message replaces the engine's generic reason, which is the only observable difference |
| E4 | `child_list` over `[]` | unchanged — no children, continues inline ([child.go:64-72](internal/engine/child.go#L64-L72)) |
| E5 | two children raise the same code | first in child-key order routes; the rest are not enumerated (D2) |
| E6 | spawn-time failure (bad input, missing definition) | unchanged — `failInstance`, not routed through `on_error`. This is what makes a child task's `on_error` list purely raised codes (§2.4) |
| E7 | child raises at its first task | fine; no output is computed either way |
| E8 | root process raises | no parent; `UpdateInstance`; API reports `raised` + `error_code` |
| E9 | grandchild raises, child does not handle it | child fails (§5.2 step 2), which fails fast up its own tree. An error never crosses two levels implicitly |
| E10 | self-referencing (recursive) child | R5 checks `D`'s rules against `raises(D)` itself. No fixpoint: `raises` is a syntactic scan, so it terminates |
| E11 | `child_map` where only some children raise | R5 takes the union of `raises` over all entries of the task |
| E12 | parent crashes mid-resolution | safe by I4 |
| E13 | handler routes back into the main flow | legal, but `outputs[T.id]` is absent — reading it is a registration error via the existing reachability analysis |
| E14 | a `raise` on an unreachable task | inflates `raises(D)`; R5 permits rules for a code that cannot occur. Conservative in the safe direction |
| E15 | child gains a new raise site after the parent registered | not caught (§3.1); parent fails at runtime when it occurs. Bounded by version pinning |

## 9. Locked decisions

- **D1 — no `child.` prefix.** A child task's `on_error` can only ever see raised
  codes, because every other failure path on those actions goes straight to
  `failInstance` (E6). Raised codes are also lexically distinct from engine codes
  (R1 forbids dots; every engine code has one), so a raised code is always an
  authored, semantic name — never a re-raised system code.
- **D2 — no `siblings`; `$error` reports one raise.** An earlier draft put an
  aggregate `siblings` list (every raised child in the batch) in `$error`, argued
  as information not data: "6 of 10 failed" is a branching input. **Reversed and
  removed.** Three reasons: (1) the engine never routes on it — resolution always
  routes on the first child-key-ordered raise (§5.2), so `siblings` was pure
  reporting with no control-flow role; (2) it is exactly the "signal, not value"
  line §0 draws — "6 of 10 shipped, and here is why" is a *result*, which §0 says
  belongs in child `output`, not the error channel; (3) it does not even fit a
  fan-out cleanly, because a batch is all-or-nothing (I1) — if 3 of 10 raise, the
  batch produces no output, so you cannot collect the 7 successes either. A
  fan-out that needs per-item outcomes should have its children *complete* with a
  `{ok: false, reason}` output and let the parent collect all ten; raise-in-a-batch
  is for "branch on the first occurrence", which the first-raise `$error` serves.
  This was the one place the design leaked aggregate reporting into errors, and
  cutting it makes the boundary clean.
- **D3 — reachability check only; no exhaustiveness.** R5 verifies that every
  rule *can* fire; nothing verifies that every raisable code *has* a rule.
  Requiring the latter makes a shared child painful to depend on. Accepted cost:
  §3.1's third row. **Note the direction:** adding exhaustiveness later would
  break existing definitions, so this is the harder of the two to reverse.
- **D4 — `raised` is a distinct status.** Not `completed` (every filter,
  dashboard and alert keys on `Status`; folding a third outcome in would
  mislabel all of them) and not `failed` (which means defect and poisons
  ancestors). It is **not retryable**, and is treated as settled work at every
  depth (§11.1, §11.2): a raise is a declared outcome, and retry — which can only
  re-run the *deciding* task, never the upstream state that drove the decision —
  has nothing to offer it. `failed` stays retryable whether the fault was
  detected by the engine or authored with `panic`.
  **The name is `raised`, not `errored`.** It pairs with the clause that produces
  it exactly as `paused` pairs with `pause` — the convention the lifecycle verbs
  already follow — and it survives the one place a status name has to work
  hardest: an operator scanning a column of `failed` and `raised` can tell them
  apart, where `failed` and `errored` are indistinguishable without the docs.
  That also retires the collision §11.5 was written about.
- **D5 — defects are never catchable.** No `child.failed`, no opt-in. Making a
  failure catchable requires converting it to a raise inside the child, at the
  task that understands it (§5.4). An authored `panic` is a defect like any
  other — authoring it grants no special status.
- **D6 — `panic` carries a code, for classification not branching.** An earlier
  draft gave `panic` a message only, reasoning that a code exists to be branched
  on and nothing can branch on a panic. That conflated two audiences. Parents
  branch; the API's consumers *classify* — alerting, dashboards, "how many
  instances died of this, and did it start after Tuesday's deploy". `error_code`
  is a filterable column and free text is useless for that, so panics carry the
  same `Fault` as raises.
  What remains asymmetric is reachability, not shape: panic codes are excluded
  from `raises(D)` (§2.3), because no `on_error` rule can ever match one and R5
  would otherwise bless rules that can never fire. R6 keeps a code from being
  both, so `error_code` means one thing per process.
- **D7 — no parent-side retry.** By §0 a raised error describes a *settled*
  condition: the child already exhausted whatever transient retries made sense
  before giving up, so re-spawning it mostly reproduces the same error. `retries`
  is therefore rejected on child tasks (R4) rather than silently ignored.
  This removes the only part of the design that would have touched the
  concurrency-sensitive sibling queries — a per-slot attempt dimension
  (`_spawn_attempt`, live-attempt selection, `CountActiveSiblings`), which sits
  inside the lock-ordering discipline that prevents deadlocks
  ([db_lifecycle.go:27-30](internal/db/db_lifecycle.go#L27-L30)).
  **Adding it later is purely additive** — no definition written under R4 can
  change meaning when it lands — so the decision is cheap to revisit if a real
  use case appears. §10.1 records what the coarse workaround looks like in the
  meantime, and doing it repeatedly is the signal to build the real thing.

## 10. Phasing

All three phases are implemented (see the status header). They were built and
tested in this order:

1. **Raise and panic.** ✅ `Fault` type, `SwitchCase` / `ErrorCase` gaining
   `.Raise` and `.Panic` (including the hand-written wire format and OpenAPI
   blobs, §7.2), `Raises()`, `StatusRaised` + `Status.Terminal()`, R1–R3 and R6,
   the `addTerminal` fix, migration 023, `error_code` populated on every terminal
   failure (§7.1), `CountActiveSiblings`. A raising child still failed its parent
   at this stage, but a *root* process could already report a typed failure, and
   `panic` was complete on its own from day one. Includes all of §11.
2. **Catch, single child.** ✅ M1 (child tasks use `matchOnError`), R4/R5, and
   `resolveRaisedBatch` ahead of the collect (§5.2). `saveAndNotify` needed no
   change (§5.1). Complete for a one-entry `child_map`.
3. **Batch resolution.** ✅ §5.2 over a multi-child batch, with `$error.child_key`
   / `.child_index` naming which child raised. Complete for fan-outs. Phases 2 and 3 landed together, since the
   single-child case is just a one-element batch.

### 10.1 Re-running a batch without retry

A parent that genuinely needs to re-run a batch can do it with a `goto` back to
the spawning task: the error route clears `wait_state`, so re-entering the task
spawns a fresh batch.

```yaml
- id: pay
  action: { type: child_map, children: { … } }
  on_error:
    - code: ["psp_unavailable"]
      goto: $pay          # re-spawn the whole batch
```

This is deliberately coarse — it re-spawns **every** slot, discarding the
successful ones — so it is wrong for `only_once` children and wasteful for
fan-outs. Nothing bounds the loop either, so the definition must carry its own
counter (a task output incremented per pass, as in the polling example).

Those limitations are exactly what per-slot retry (D7) would fix. If definitions
start reaching for this pattern, that is the evidence to build it.

## 11. The `retry` command

`RetryProcess` ([db_lifecycle.go:424](internal/db/db_lifecycle.go#L424)) revives a
settled tree: it locks root + descendants, walks top-down, and restarts the
interrupted path while keeping finished work. Since the pause/resume split it is
**failed-only** — resuming a suspended tree is `ResumeProcess`, a plain status
flip that grants nothing (pause-resume §1).

The net change is small — the root gate keeps its shape — but two of the four
points below are correctness fixes, not adjustments.

**The unit of retry, settled.** A question this design could have left open is
whether retrying a parent whose child raised should re-run the *child* or
re-run the *parent*. It re-runs the parent, always, and never the child — and
that falls out of two decisions already made rather than needing a rule of its
own: per-slot child retry was removed (D7), and a raised child is settled work
that `revive` keeps (§11.2).

The general form is the same fault/outcome split as everywhere else:

- a child that **raised** is never restarted — it concluded;
- a child that **faulted** is restarted, as part of reviving the interrupted
  path, because the parent never got an answer from it.

So there is exactly one unit of retry — a process — and exactly one way to
restart work: name the root and let `revive` walk down to whatever was actually
interrupted.

### 11.1 What retry actually does, and what that implies

Retry re-runs **the task the instance is sitting on**. It has no rewind: it
cannot restart an earlier task, and it cannot alter persisted context. That
single fact decides everything below.

For a **fault**, retry's model fits: the task's action failed, the cause is *at*
that task, and re-running re-attempts exactly the thing that broke.

For a **raise**, it usually does not. `inst.Task` is the task whose switch made
the decision, but the state that *drove* the decision is often upstream:

```yaml
- id: check                       # raise reads a PRIOR task's output
  switch:
    - case: "outputs.validate.ok == false"
      raise: { code: invalid_order, message: "the order failed validation" }
    - goto: end
```

Re-running `check` re-evaluates the same expression against the same stored
`outputs.validate`, which retry cannot have changed. It re-raises, always. For a
switch-only task this is *provable*: there is no action, so nothing but persisted
context feeds the decision.

And when the raising task *does* have an action, retry is worse than futile — the
action already **succeeded** (the switch ran on its result), so re-running
re-executes a side effect that worked, and then re-raises on the same upstream
state.

So the line is not authored-versus-engine. It is **fault versus outcome** — and
the status already encodes it exactly:

| status | meaning | retryable |
|---|---|---|
| `completed` | finished | no — nothing to recover |
| `raised` | an anticipated condition concluded the process | **no** |
| `failed` (panic) | the author detected a fault the engine could not | yes |
| `failed` (engine) | the engine detected a fault | yes |
| `paused` / `pausing` | suspended by an operator; not an outcome at all | no — `resume` it |

**So the root gate keeps its shape** — `raised` falls through the failed-only
check exactly as `paused` already does, and needs no new condition, only its own
explanation. It slots in beside the existing paused branch
([db_lifecycle.go:432-451](internal/db/db_lifecycle.go#L432-L451)):

```go
if status := model.Status(rootRow.Status); status != model.StatusFailed {
    if status == model.StatusPaused || status == model.StatusPausing {
        return fmt.Errorf("process is paused, not failed (status: %s); resume it instead", status)
    }
    if status == model.StatusRaised {
        return fmt.Errorf("process concluded with error %q (status: raised); a raised error is "+
            "a declared outcome, not a fault — start a new instance, or publish a new version "+
            "if the outcome should be handled differently", rootRow.ErrorCode)
    }
    return fmt.Errorf("process is not retryable (status: %s)", status)
}
```

Both special cases exist for the same reason: "not retryable" alone invites a bug
report, and in both cases there is a *different verb* that is the right answer.
The pause branch names it (`resume`); the raise branch has to say that there
isn't one, which is why its message is longer.

**Panics stay retryable**, and that is not an inconsistency. `panic` chose the
`failed` status precisely because it means *this is a fault* — the author is
reporting something broken that the engine could not see, like a `200` with an
error body. Faults are what retry is for: fix the cause outside the process,
re-enter. An author who wanted "this is a settled outcome, do not re-run" has a
clause for that, and it is `raise`.

The residual case — a panic whose condition also reads stale upstream output — is
real, and retry is futile there too. But that is retry-has-no-rewind biting, a
property it already has for any failure whose cause is upstream. It is not
something `panic` introduces, and it does not justify a discriminator column to
separate authored from engine failures.

#### Why not gate on "the panicking task has an action"

A tempting refinement is to allow retry only when the current task has an
action, on the grounds that a switch-only task re-evaluates persisted state and
must decide identically. Rejected, for two reasons.

**The proxy is wrong: `config` is live.** It is re-resolved from the OS
environment every tick, never persisted
([advance.go:90-96](internal/engine/advance.go#L90-L96)), and `buildEnv` always
puts it in scope ([evaluator.go:44](internal/engine/evaluator.go#L44)). So a
switch-only task *can* decide differently on a retry:

```yaml
- id: guard
  switch:
    - case: "config.psp_enabled == false"
      panic: { code: psp_disabled, message: "the PSP integration is turned off" }
    - goto: next
```

Flip the env var, retry, and it proceeds — precisely the fix-outside-and-re-enter
case retry exists for. A correct version of the rule would have to ask whether
the deciding expression reads `config`, which means extending
`expression.Roots` and gating an operator-facing API on a static analysis that
must never be subtly wrong.

**Futility is not a gating criterion anywhere else.** A task whose response fails
its `result_schema` produces `output.invalid`; re-running fetches the same
response and fails identically, because the fix is a new definition version. That
retry is exactly as futile and is allowed today. Refusing only panics would be an
arbitrary carve-out for the one case we happened to examine.

The cost of permitting a futile retry is one claim and one advance, after which
the operator sees the identical failure and knows more than before. The cost of
forbidding it is an API whose behaviour depends on a definition detail the
operator may not have in front of them. If the concern is worth addressing, the
proportionate lever is *information* — have the retry response name the task that
will re-run and whether it has an action — not a refusal.

### 11.2 `raised` is settled work — keep it, do not revive it

`revive` switches on status ([db_lifecycle.go:526-534](internal/db/db_lifecycle.go#L526-L534)):
`completed` returns (finished work is kept); `running`/`failing`/`pausing`/`paused`
return (defensive — a live or suspended node belongs to the engine or to
`ResumeProcess`); **everything else is revived**. So `raised` would fall through
to revival by omission — and by §11.1 that is wrong. It belongs with `completed`:

```go
case model.StatusCompleted, model.StatusRaised:
    return nil // settled work is kept — see §11.1
```

The comment below the switch, *"node is failed"*, stays accurate.

Note the two `return nil` arms mean different things and must not be merged: the
first is *settled, keep it*, the second is *not ours to touch*. `raised` joins the
first. This keeps the status meaning one thing at every depth: **`raised` is
concluded, whether it is the root or a leaf.** A raised child was not
interrupted; it finished, by design, with a declared outcome.

**A limitation worth stating rather than hiding.** The only case where this
matters is a parent that failed *at resolution* because no rule matched a child's
raised code. Reviving that parent puts it back on the spawn task with a settled
batch, so it re-resolves, finds the same unmatched code, and fails identically.
Retry cannot help there — and could not have helped by reviving the child either,
since the missing `on_error` rule lives in a **version-pinned definition** that a
retry does not re-read. The real fix is a new parent version and a new instance.
Better to say so than to offer a retry that quietly changes nothing.

The scoping question this raises — *"what about a raised child whose error the
parent already handled and routed past?"* — is answered by the existing
structure either way. `revive` only looks at `children[node.ID][node.Task]`, the
batch of the node's **current** task; once resolution routes on an error,
`inst.Task` becomes the goto target and that batch is behind the node. Worth a
comment, because it is not obvious and a refactor could break it silently.

### 11.3 Clear `error_code` and `$error` on revive

`revive` clears `Error`, and the write loop passes `Error: ""` explicitly
([db_lifecycle.go:601-614](internal/db/db_lifecycle.go#L601-L614)). `error_code`
must be cleared in the same params struct.

This is easy to miss and its failure mode is quiet: a revived instance that later
completes would keep a stale `error_code`, so §7.1's filter would report a
successful process as having died of `card_declined`. It corrupts precisely the
column the code exists to serve.

**The `$error` context slot is cleared too** (`ErrorData`, currently passed
through verbatim as `raw.ErrorData` in the same struct). The inconsistency —
`error` cleared, `$error` kept — predates this design and was mostly harmless,
because the mustErr/mayErr dataflow means a stale `$error` is only *readable*
where the analysis says one may be present. But this design makes it reachable
one more way: a batch error route writes `$error`, and reviving that parent would
keep it. Clearing is the consistent choice and cannot break a valid definition —
if a task can read `$error`, the analysis has already proven an error is present
on every path reaching it, so the value it reads is the fresh one.

### 11.4 `Status.Terminal()` must include `raised` — a real bug if missed

```go
for _, k := range kids {
    revive(k)
    if !k.Status.Terminal() {
        anyActive = true
    }
}
```
([db_lifecycle.go:545-552](internal/db/db_lifecycle.go#L545-L552))

`revive(k)` runs *first*, so this asks "after revival, is anything running?" and
reconstructs `waiting` vs `collecting` from the answer.

**With §11.2 this is a live bug, not a latent one.** Raised children are *not*
revived, so they stay `raised` — and if `Terminal()` does not know that status,
the condition is true, `anyActive` is set, and the parent is parked in `waiting`
forever, waiting on a child that has already settled. The instance is
unrecoverable and nothing logs why.

**The fix belongs in `Status.Terminal()`
([instance.go:30](internal/model/instance.go#L30)), not here.** An earlier draft
patched the status list at this call site, back when it was spelled out inline;
the pause work replaced it with `Terminal()`, which is the better place on the
merits and not just by accident. `Terminal()` answers "is this a settled
outcome", D4 says `raised` is exactly that, and the call site is asking that
question and no other. Patching here would leave `Terminal()` lying.

The scope is safe to widen: `Terminal()` has exactly one call site today, this
one. What it does **not** cover is the SQL copies of the same idea — §7 lists all
four and shows that only `CountActiveSiblings` needs the matching edit. Those two
changes travel together; either alone hangs a parent.

### 11.5 Naming — resolved

`ROADMAP.md` says *"Retry only for errored processes."* Written before either
design, "errored" there meant *faulted* — what this spec calls `failed`. The
`cancel` → `pause` rename it was to be settled alongside has since shipped, and
D4 settles the rest: the new status is **`raised`**, so nothing now competes for
the word.

The roadmap line should be reworded to *"Retry only for failed processes"*, which
is what it always meant and what `RetryProcess` now enforces. It is also
already accurate in a way it was not when written: retry no longer accepts
`cancelled`, because there is no such status.

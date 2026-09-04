# Task scopes

**Status: implemented**, the error axis since 2026-09-04. Which names a task's expressions may
use, and what each one holds.

A task's slots are not evaluated at one moment. The action's slots are built first, the action
answers, the output map runs, the switch routes. `self` names the task itself, and its members
come into existence at different points along that line — so a slot may only read the ones that
already exist where it sits.

## The three members

| member | exists from | holds |
|---|---|---|
| `self.previous` | the task is entered | the output of the last run of **this** task |
| `self.result` | the action answers | the raw action result, typed by `result_schema` / `responses` |
| `self.output` | the output map has run | the projection that just became `outputs.<id>` |

`self.status` / `self.headers` are siblings of `self.result` and exist on a fetch only.

## The slots

| slot | `previous` | `result` | `output` |
|---|---|---|---|
| `input`, `body`, `url`, `method`, `headers`, `query`, `accepted_status`, `over` | ✓ | | |
| `delay.for` / `delay.until`, `timeout` | ✓ | | |
| `on_error` `case`, `retry.*`, `raise` / `panic` | ✓ | | |
| `output` | ✓ | ✓ | |
| `switch` `case`, `raise` / `panic` | ✓ | ✓ | ✓ |

`on_error` sits with the action slots rather than with the output: the action answered, but with
a failure, so there is no result and the output map never ran. What it *can* see is the output
of the last run that succeeded — untouched, because a failing task writes none.

A process-level `output` is not a task slot and has no `self` at all.

## The error axis: `error` and `last_error`

**BUILT 2026-09-04.** `error` used to name two different failures depending on which line it
sat on. Inside an `on_error` rule it is the error that rule **caught**; anywhere else in a task
it is the error that **routed control here** from some other task's `on_error`. Both are useful
and both are needed; sharing one name is what makes them impossible to tell apart.

| name | is | in scope |
|---|---|---|
| `error` | the failure the rule at hand caught | inside an `on_error` rule — `case`, `retry`, `raise`, `panic` |
| `last_error` | the failure that routed control into this task | every slot of a task an error edge enters |

The name is `last_error` and not `prev_error` because `previous` is already spoken for by
`self.previous` — *this* task's prior run — so `prev_error` reads as "my last attempt's error",
which is the wrong referent and type-checks. `last_error` misreads in the other direction, as
"the last error anywhere in this instance", and that reading is refused at registration.

**`last_error` is not durable, and the name is the only thing that suggests it might be.** It is
deleted on every ordinary transition (`advance.go`): a handler that wants the failure to travel
projects it into its own output, which is how every other value moves, and inference types it on
exactly the tasks an error edge enters. Making it survive would put an optional, union-typed
failure on every task downstream of any handler, trade an exact type for a weak one, and
duplicate what the audit trail already records.

### What it dissolves

`retry.*` used to read the routed-in error while `case` / `raise` / `panic` two lines above it
read the caught one — the same word, two failures, and no way to see which. Now all four read
`error`, the failure the rule caught: **a policy is measured against the failure it is
retrying**, which is what a `case` two lines above already does.

[retry-policy.md](retry-policy.md) recorded the opposite — that a policy answers "how long do we
wait for this dependency", a property of the deployment rather than of the individual failure.
That sentence and the placement that produced it landed in one commit (`70dca11`), and nothing
else argued for it: the engine resolved the policy before the failure was written, so the
restriction was what the code did, described. A body that says when to come back is the obvious
case against it.

The shape that falls out: **the persisted slot is only ever written for the next task; what a
rule reads is bound for the rule and nothing else.**

That makes the write's ORDER load-bearing, which it never was while one name covered both.
`last_error` must not be overwritten until the rule has been evaluated — otherwise a rule
naming both reads the same failure twice. So the engine binds `error` for the clause and defers
the write past it (`error.go`, `collect.go`); a granted retry still returns before either,
leaving nothing behind.

### What it costs

Handler tasks change `error.*` to `last_error.*`; `on_error` clauses do not move. Nothing goes
silently wrong — `error` is simply not in a handler task's context afterwards, so an unconverted
definition is refused at registration.

No migration: the failure has had its own column since migration 019 (`error_data`), so the name
lives only in `inst.State`'s key and in the inferred context. `state.error` on the instance
detail becomes `state.last_error`.

## `outputs.<own id>` is `self.previous`

Inside task `t`, `outputs.t` is the **previous** output, in every slot. It is the same value
`self.previous` holds, under the name any other task would use for it.

This is not free in the switch. The engine writes `outputs.<id>` before the switch runs, so the
stored value there is the one this run just produced — `engine.buildEnv` shadows the slot with
the prior output to keep the rule true. The alternative, letting the name mean the new output in
one slot and the old one everywhere else, is a difference no error can report: both readings
type-check, and the definition is simply wrong on one of them.

**`self.output` is the name for what this run produced.** A loop counting its own iterations
routes on `self.output.n`, not on `outputs.t.n`.

## No loop, no previous

`previous` exists only where control can return to the task. Where nothing does, both
`self.previous` and `outputs.<own id>` are refused at registration — the task runs at most once,
so the value they name never exists.

The analysis is `computeContextSets`, and an **error** edge back to the task does not count: a
task that failed produced no output, so the edge clears its own bit. A task reachable only
through its own `on_error` goto has no previous output, and reading one is refused.

## Where the two halves live

The rule is enforced twice and the halves must agree, because disagreement is silent in both
directions — a slot typed with a member the runtime does not populate reads `null` where the
schema promised a value, and one populated but untyped is simply unreadable.

- `internal/validation/scope.go` — `preOutputSlots` enumerates every slot evaluated before the
  output exists; `checkSelfScope` refuses a member that does not exist there.
  `TestPreOutputSlotsCoversEveryActionSlot` fails when a slot is added to `model.Action` and not
  to that list.
- `internal/engine/advance.go` — `selfBeforeOutput` builds the runtime counterpart, and
  `taskSelf` the full scope for the output map and the switch.

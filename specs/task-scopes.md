# Task scopes

**Status:** Built.

This spec covers which names a task's expressions may use, and what each holds. A task's slots are
not evaluated at one moment. The action's slots are built, the action answers, the output map runs,
and the switch routes. `self` names the task itself, and its members come into existence at
different points along that line, so a slot may read only the members that already exist where it
sits.

## The three members

| member | exists from | holds |
|---|---|---|
| `self.previous` | the task is entered | the output of the last run of **this** task |
| `self.result` | the action answers | the raw action result, typed by `result_schema` / `responses` |
| `self.output` | the output map has run | the projection that just became `outputs.<id>` |

`self.status` and `self.headers` are siblings of `self.result`, and exist on a fetch only.

**A result nothing declares does not exist.** It is absent rather than typed `null`, so naming it is
refused, with a message saying which declaration is missing. Typed `null`, a switch reading a delay's
result got "comparison requires non-nullable operands", which sent the author to `?? 0` rather than
to the reference that cannot work.

## The slots

| slot | `previous` | `result` | `output` |
|---|---|---|---|
| `input`, `body`, `url`, `method`, `headers`, `query`, `accepted_status`, `over` | ✓ | | |
| `delay.for` / `delay.until`, `timeout` | ✓ | | |
| `on_error` `case`, `retry.*`, `raise` / `panic` | ✓ | | |
| `output` | ✓ | ✓ | |
| `switch` `case`, `raise` / `panic` | ✓ | ✓ | ✓ |

`on_error` sits with the action slots: the action answered with a failure, so there is no result and
the output map never ran. What it can see is the output of the last successful run, untouched,
because a failing task writes none. A process-level `output` is not a task slot and has no `self` at
all.

## The error axis: `error` and `last_error`

| name | is | in scope |
|---|---|---|
| `error` | the failure the rule at hand caught | inside an `on_error` rule: `case`, `retry`, `raise`, `panic` |
| `last_error` | the failure that routed control into this task | every slot of a task an error edge enters |

All four slots of a rule, `retry.*` included, read `error`. A policy is measured against the failure
it is retrying, as the `case` beside it already is.

The name is not `prev_error`. `previous` already means *this* task's prior run (`self.previous`), so
`prev_error` reads as "my last attempt's error", which is the wrong referent and still type-checks.
`last_error` misreads the other way, as "the last error anywhere in this instance", and that reading
is refused at registration.

**`last_error` is not durable.** It is deleted on every ordinary transition (`advance.go`), and
inference types it only on the tasks an error edge enters. A handler that wants the failure to
travel projects it into its own output, as every other value moves. Making it survive would put an
optional, union-typed failure on every task downstream of any handler.

**The persisted slot is written only for the next task; what a rule reads is bound for that rule
alone.** That makes the write's ORDER load-bearing. `last_error` must not be overwritten until the
rule has been evaluated, or a rule naming both reads the same failure twice. So the engine binds
`error` for the clause (`bindCaught`) and defers the `last_error` write past it (`error.go`,
`collect.go`). A granted retry returns before either, leaving nothing behind.

## `outputs.<own id>` is `self.previous`

Inside task `t`, `outputs.t` is the **previous** output in every slot: the same value as
`self.previous`, under the name any other task would use.

In the switch this costs something. The engine writes `outputs.<id>` before the switch runs, so
`engine.buildEnv` shadows the slot with the prior output to keep the rule true. If the name meant
the new output in one slot and the old one elsewhere, both readings would type-check, and no error
could report the definition that is wrong on one of them. **`self.output` is the name for what this
run produced**: a loop counting its own iterations routes on `self.output.n`.

## No loop, no previous

`previous` exists only where control can return to the task. Where nothing returns, both
`self.previous` and `outputs.<own id>` are refused at registration, since the value they name never
exists. The analysis is `computeContextSets`. An **error** edge back to the task does not count,
because a failed task produced no output.

## Where the two halves live

The rule is enforced twice, and the halves must agree. Disagreement is silent in both directions: a
slot typed with a member the runtime does not populate reads `null` where a value was promised, and
a populated but untyped member is simply unreadable.

- `internal/validation/scope.go`: `preOutputSlots` enumerates every slot evaluated before the output
  exists, and `checkSelfScope` refuses a member that does not exist there.
  `TestPreOutputSlotsCoversEveryActionSlot` fails when a slot is added to `model.Action` but not to
  that list.
- `internal/engine/advance.go`: `selfBeforeOutput` builds the runtime counterpart, and `taskSelf`
  builds the full scope for the output map and the switch.

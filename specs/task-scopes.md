# Task scopes

**Status: implemented.** Which names a task's expressions may use, and what each one holds.

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

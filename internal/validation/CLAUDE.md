# internal/validation

## Version compatibility: the comparison is the floor

[specs/version-compatibility.md](../../specs/version-compatibility.md). `Compare` /
`CompareSet` (`compat.go`) read two **documents** and never see an instance, so they must
assume every reachable state and report a structural difference wherever one exists —
without judging whether it would hurt. That judgement needs a position (which task an
instance sits on, which keys its row actually holds), and belongs to the upgrade gate, which
is **not built yet**.

Two imprecisions follow from that, and neither is a bug to fix here: a branch that only
sometimes runs makes its output merely optional, and a new main-line task carrying an
`output` becomes required at every later task even where nothing reads it. Both corrections
are monotone — they only turn "different" into "tolerable" — which is what makes the layering
safe. **Nothing added here may turn a tolerable verdict into a refusal**, or a later gate
built on top of it would be unsound while still returning a bool.

Three rules the comparison itself depends on:

1. **One context per task is enough for the whole remaining run.** A task output's type is
   position-independent (every `outputs.<id>` resolves through `$defs[<id>_output]`), and
   the must-analysis is monotone along a path, so what holds at a task covers every task
   reachable from it. Checking a *different* task is wrong, not merely less precise.
2. **`input` is hoisted out of the per-task loop.** It sits in every task's context, so
   comparing it there reports one break N times. `taskContexts` takes the input slot from
   its caller for exactly this: `TaskContexts` (the general helper) passes it; `Compare`
   passes the zero schema and compares input once, at definition level.
3. **`config` is stripped from every context.** It is re-resolved from the environment on
   every tick, so nothing persisted corresponds to it and there is nothing to compare.

## The two `$defs` pools must stay separate

`subsetWith` takes `subDefs` and `superDefs` separately, so each side dereferences its own —
which is what lets two independently-generated contexts be compared even though
`uniqueDefName` may name the same concept differently. **Merging the pools silently compares
a schema against itself.**

The trap is `WithDefs`, which **replaces** a root pool rather than merging into it. A
`result_schema` is self-contained after `Normalize` (its shared definitions are baked into
its own root `$defs`), so it is compared **bare** — attaching the other side's pool to it
strips its own, and every `$ref` inside then resolves to nothing or, worse, to a same-named
definition that means something else.

## Changed slots are a field comparison, not a schema question

`changedslots.go` is its own file so that "which slots differ" never acquires opinions about
which differences matter — the verdicts are blind to meaning (dollars → cents is `number`
either way), and this list is what lets a reader supply the judgement.

**Adding a field to `model.Action`, `model.Task` or `model.ProcessDefinition` means adding
it here too, and nothing fails if you forget** — the slot silently stops being reported.
`changedslots_test.go` enumerates all three structs against the slot lists to make that
loud; a field that genuinely cannot differ goes in `notASlot` **with its reason**, not into
silence.

## Diagnostics decompose above `isSubset`, deliberately

`isSubset` returns a `bool` and sits on every hot validation path, so threading an error
through it would be a large change to load-bearing code for one caller. `explainer`
(`compat.go`) instead re-runs the check slot by slot and one level into properties, which
yields `outputs.charge.amount: number → string` and `input.currency: newly required` from
unmodified calls. Its depth is bounded because a recursive schema has no bottom to reach.

`explainer.swap` renders the arrow old → new even when the check runs new ⊆ old (the output
contract): the reader is asking what they changed, not which direction the subset ran.

## Pointers

- `NarrowsTo` vs `IsSubset`, and why only a `result_schema` may narrow —
  [internal/schema/CLAUDE.md](../schema/CLAUDE.md).
- The collapse that makes `outputs.a.v ?? outputs.b.v` imprecise, and the per-terminal walk
  that recovers it — [specs/path-sensitive-output.md](../../specs/path-sensitive-output.md).

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

## Upgradable means the gap is closable, not that the shapes match

The continuation checks use `IsSubsetAbsentAsNull`, which tolerates the new version
requiring a property the old one did not **when that property's type admits null**. The
justification is NOT that reads happen to be forgiving — it is that
`Schema.FillAbsentAsNull` closes exactly that gap by writing the null in, and an upgrade
runs the stored state through it. The verdict says *we know how to move this data*, which is
a claim with something behind it.

The two are a pair and must accept the same gaps; `schematest/absent_test.go` is what holds
them together. **A relation that tolerates more than the fill can close is the dangerous
direction** — it promises an upgrade that then fails to conform.

Which checks get it is decided by whether anything conforms the value afterwards:

- **`readExplainer`** — the per-task contexts and the input. An instance's stored context is
  read, and its input was conformed once at creation and never again, so the fill is the
  only thing that ever has to touch them.
- **`contractExplainer`** — the output contract, deliberately STRICT. Its consumers include
  a waiting parent, which conforms the child's output against its `result_schema` at collect,
  and nothing migrates the value on that path. Relaxing there would promise what the runtime
  refuses.

Two things would break this if they land without revisiting it: conforming the input on
upgrade (specs §10) makes the input relaxation unsound, and the gate's external-result rule
(§4) belongs on the relaxed side, because a submitted result is read back as `self.result`
rather than re-conformed.

## Nothing may vanish from a report, and nothing unjudged may look judged

`CompareSet` iterates the **union** of both sides' names, not the target's. A process the
target side does not carry would otherwise disappear silently — and the caller is expected
to have carried it over, so a disappearance means the caller has a bug the report would
hide. It gets a row saying there was nothing to compare it against.

**One row per process, whatever became of it.** An unanalysable version is a row with that
status rather than a list of its own, so a reader never crosses two arrays to find out what
happened to a name — and the row already carries the versions, which is what says *which* of
the two failed.

**A version number says more than its document does.** Two stored versions with identical
documents are still different processes: a version also pins the child versions it runs, so
`nothing changed` is decided by the numbers there. A *submitted* document has no number, so
that one case falls back to comparing documents (`documentsDiffer`) — which is also why that
comparison includes task ORDER, since `switch: next` routes by position.

**A process's status comes from its versions alone, and only a pair actually being compared
is analysed.** That ordering is load-bearing, not an optimisation: a registry accumulates
definitions validated under the rules of their day, so analysing everything up front lets an
unchanged, unasked-about process fail to analyse and drag a report about two *other* versions
down with it.

Every row carries a `CompareStatus`, and `compared` is the only value under which the
verdicts mean anything. `nothing_to_compare` (both sides at one version) and `new` (no
previous version) set the verdict fields to true because there is nothing to fail — which is
exactly why the status has to be there, and why the renderer prints dashes rather than
"yes" for them. Neither contributes to the roll-up: a deployed channel always carries
processes a bundle does not, so counting them would report almost every real comparison
as incompatible.

## Pointers

- `NarrowsTo` vs `IsSubset`, and why only a `result_schema` may narrow —
  [internal/schema/CLAUDE.md](../schema/CLAUDE.md).
- The collapse that makes `outputs.a.v ?? outputs.b.v` imprecise, and the per-terminal walk
  that recovers it — [specs/path-sensitive-output.md](../../specs/path-sensitive-output.md).

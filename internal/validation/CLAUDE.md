# internal/validation

## Version compatibility: the comparison is the floor

[specs/version-compatibility.md](../../specs/version-compatibility.md). `Compare` /
`CompareSet` (`compat.go`) read two **documents** and never see an instance, so they must
assume every reachable state and report a structural difference wherever one exists —
without judging whether it would hurt. That judgement needs a position (which task an
instance sits on, which keys its row actually holds), and belongs to the upgrade gate, which
is **not built yet**.

Two imprecisions follow, and neither is a bug to fix here: a branch that only sometimes runs
makes its output merely optional, and a new main-line task carrying an `output` becomes
required at every later task even where nothing reads it. **Nothing added here may turn a
tolerable verdict into a refusal**, or a later gate built on top of it would be unsound while
still returning a bool.

The second imprecision looks like it wants pruning — require only what is read from here on
— and that was built and **reverted**. It is not monotone, it is unsound: pruning leaves the
unread values unreconciled, so the row stops conforming to the version it now runs, and the
NEXT comparison reasons from a premise this one falsified. Nothing fails at the hop that
caused it. specs/compat-command.md §2f has the worked example; the rule it leaves behind is
that **a migration reconciles the whole context or the chain is broken**.

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

## What a pairing cannot see is an addition — everything else about a child call is its schema

**A renamed child call needs no rule, and adding one was reverted.** Registration established
that the old call's output fits the old result schema (statically via `checkChildOutputType`,
or at collect's conform where the child declares no output), so `old ⊆ new` carries the child
in flight across whatever process it is now an instance of. An identity check would be a false
break in the member that **cannot be excused** — and the implementation that added one also
skipped the schema comparison, which was the check doing the work.

Two things have no old schema to carry that premise, so they are reported directly:
`resultContract.added` (a `result_schema` declared where none was — `{}` excepted, since it
can fail nothing) and `addedChildKeyIssues` (a `child_map` key, which is §2b's added task one
level down). A key REMOVED is silent: the orphan output lands under a key the new version does
not declare and the output conform strips it.

**The changed-slot side must address a key exactly as the issue side does.** `childKeyAddress`
is shared for that reason: §6b suppresses a slot row only where a break carries the SAME
address, so a per-key break under a whole-map slot row would print `(not judged)` next to the
break its own edit produced.

## Upgradable means the gap is closable, not that the shapes match

The upgrade checks use `IsSubsetAsStored`, which is one half of a pair —
`Validate(data, schema.ConformToSchemaExactly)` is the other. The relation decides a gap is
closable; the conform closes it, in both directions of the null-versus-missing distinction
(writes the null where a required nullable is absent, removes a key whose stored null the new
schema will not take). The verdict says *we know how to move this data*, which is a claim
with something behind it.

**They must accept exactly the same gaps**, and `schematest/` holds them together:
`absent_test.go` for the fill half, `conform_exact_test.go` for the removal half, and
`subset_stored_test.go` for the two rules `IsSubsetAsStored` adds. A relation tolerating more
than the conform can close is the dangerous direction — it promises an upgrade that then
fails. Removal is narrower than it looks: only an optional declared property whose target
dropped `null`, never one the target still holds.

Three explainer configurations, and **the `swap` flag is the trap**:

- **`storedExplainer`** (`{asStored: true}`) — the per-task contexts and the input. An
  instance's stored context is read and its input was conformed once at creation, so the
  conform is the only thing that ever touches them.
- **the bare `explainer{}`** — the input CONTRACT and every result schema. Strict, and
  deliberately no swap: these already run old ⊆ new, and the arrow reads old → new.
- **`contractExplainer`** (`{swap: true}`) — the process output alone, which runs new ⊆ old.
  Swap flips BOTH halves of the message, not just the arrow: a property super requires that
  sub lacks is not "newly required" there, it is one the old side guaranteed and the new side
  no longer does.

An explainer that disagrees with the relation it dispatches to reports a break with nothing
to say about it, or the reverse — the message comes out `object → object`. Anything added to
`IsSubset` must be added to `explain` in the same change.

Two things would break the pairing if they land without revisiting it: conforming the input
on upgrade makes the input relaxation unsound, and the gate's external-result rule belongs on
the relaxed side, because a submitted result is read back as `self.result` rather than
re-conformed.

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
exactly why the status has to be there, and why the renderer repeats the status in BOTH
columns rather than leaving one blank — an empty cell under a header reads as a question that
went unanswered. Neither contributes to the roll-up: a deployed channel always carries
processes a bundle does not, so counting them would report almost every real comparison
as incompatible.

## Pointers

- `NarrowsTo` vs `IsSubset`, and why only a `result_schema` may narrow —
  [internal/schema/CLAUDE.md](../schema/CLAUDE.md).
- The collapse that makes `outputs.a.v ?? outputs.b.v` imprecise, and the per-terminal walk
  that recovers it — [specs/path-sensitive-output.md](../../specs/path-sensitive-output.md).

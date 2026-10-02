# Instance upgrade

**Status: built, except §3b's pairing check and §8.**

The upgrade gate: moving an instance from one version to another. The check it answers to —
what is compared, in which direction, how it is reported — is
[compat-command.md](compat-command.md). The check reads two documents and must assume every
reachable state; the gate has the row in hand.

## 1. The gate conforms the row

`MigrateState` conforms the instance's stored state to the target's layer at its task
(`TaskContexts(to)[task]`, `ConformToSchemaExactly`), after materializing externalized values —
the conform cannot normalize inside an object it has not loaded. It judges what the row holds,
so branch-only outputs a joined context makes merely optional pass here when the row has them.

**The gate may accept what compat calls different, never the reverse.** Given the premise
registration establishes — a stored value conforms to the type the old version inferred — an
upgradable verdict (`IsSubsetAsStored`) is a gap the conform closes (compat-command.md §2d).
The held-instance checks (§2) are compat's own functions, so the two cannot disagree. Demand is
**not** refined: a migration that reconciles only what is read leaves the row not conforming to
the version it now runs, which falsifies the next hop's premise (compat-command.md §2f).

## 2. The boundary is entry to a task

The persisted entry context is the one observable state per task: `self` never survives an
advance, and every task end is a boundary, inline chains included. A **held** instance carries
more:

- **parked** (`external`, or `children`/`collecting`) — the new result schema and `raises` must
  accept what the old promised, strictly `old ⊆ new` (`InFlightResultBreaks`, compat's upgrade
  findings; §5.8);
- **any held instance** — parked, or on a delay's timer, which has no phase, only `wake_at` —
  refuses an action-type change (`TypeChangeBreak`): what the old action left (a result,
  children carrying its spawn keys, a timer computed under the old definition) has no
  counterpart in the new one.

Otherwise it is entry plus a counter: a retry re-runs from the start, and a `retries` lowered
below the stored `retry_count` fails rather than retries.

**Only `paused` and `failed` move** (`movableStatus`, and the write's predicate). `running` can
advance between plan and write (the CLI pauses it first); `failing`/`pausing` are draining;
terminal statuses hold no work, so the move would only re-describe frozen data.

The write is conditional on `process_version`, `task`, `status IN ('paused','failed')` and
`worker_id IS NULL`. **`task` is load-bearing**: a row resumed, advanced and re-paused between
plan and write matches on everything else. A row paused after its lease expired keeps its
`worker_id`, so it moves only after a resume lets a worker reclaim it. Never clear `worker_id`
to admit it: it is the `ReclaimedExpired`/`only_once` evidence.

## 3. A running child and a waiting parent

### 3a. No `_spawn_result_schema`: collect reads the parent's current task

Collect conforms a child's output (and a raised payload) against the parent's task as it stands
now, never a spawn-time copy — which is why a parked parent's `result_schema` is an upgrade
concern (compat-command.md §2c). Do not reintroduce a copy: the conform normalizes, so a stale
schema strips fields both sides agreed on ([internal/engine/CLAUDE.md](../internal/engine/CLAUDE.md)).

### 3b. The pairing check (not built)

One schema governs both steps: `outC.NarrowsTo(S_parent)` as the parent currently
stands. Both-move is already guaranteed (batch registration checks it); the two mixed
rows — child moves only (`outC_new ⊆ S_old`), parent moves only (`outC_old ⊆ S_new`) —
are the cross-document check only `CompareSet` can compute. The child's general output
contract (compat-command.md §3a) transitively implies row two, but the pairing is
**tighter**: a child dropping a field fails that general contract, yet a parent that never
named the field is unaffected, and only this says so. Per key for `child_map`, per element
for `child_list`; skipped without a `result_schema`.

### 3c. A running child may not move without its parent

A parent's definition names its child versions (explicit `version`, else self-reference, else
the baked dependency row), so a running child moved alone leaves its parent executing a version
its definition does not name. The unit of upgrade is therefore the **non-terminal tree
closure** (`NonTerminalSubtree`): a non-root is refused, and each child's target comes from
`ResolveChildVersion` against its parent's TARGET — the function spawn uses, because two copies
drift silently. A self-reference inherits the parent's target. Terminal descendants stay put;
their outputs are frozen. The tree is written in one transaction, so no mixed state is visible.

## 4. The write: version and migrated state together

One conditional `UPDATE` (`UpgradeInstanceVersion`) writes `process_version` with the migrated
state, re-cut through `persistState`: the version is the lens the row is read through, so the
two are never written apart. Idempotent — a member already on the target is skipped, so
repeating a run repairs a partial one. `EventInstanceUpgraded` is the only record of the version
an instance came from. Not lossless: a pruned output (§5.5) does not come back on a downgrade.

A property newly required with a default refuses a row that lacks it: creation fills defaults
and the migration deliberately does not, since a default filled into a half-run instance
disagrees with every stored value derived from its absence (compat-command.md §2d).

## 5. What this cannot catch

1. **Meaning** — dollars → cents; invisible to any static check.
2. **Routing** — new `switch` conditions, or a child gaining a raise code its parent has no
   rule for (coverage is not guaranteed — child-error-handling.md D3).
3. **Tasks already run** never re-execute.
4. **Side effects already performed.**
5. **Stale outputs are pruned, not carried.** The conform strips a dropped task's output (the
   layer is complete inside `outputs`, and nothing on the new version can read it); engine keys
   outside the layer pass through `MigrateState`.
6. **A renamed task** reads as removed + added, and is refused (§8).
7. **Redaction** is config-only and console-only, and config is never stored, so `secret: true`
   changing with the version exposes nothing; compat reports it as a `(not judged)`
   `config_schema` row.
8. **An in-flight result is judged by schema, which over-refuses on children — accepted.** The
   strict comparison is forced only for `external`, where the result is with a worker. A running
   child moves with its parent to a registration-checked version, and a completed child's actual
   output could be conformed precisely. Kept because refusing leaves a tree paused and an
   operator informed, while a wrong allow wedges the parent at collect with a result nothing
   accepts.
9. **`only_once` may flip — accepted.** The new definition is the stated policy. An interrupted
   row cannot carry the flip across: it moves only after a reclaim (§2), which resolves the
   interruption under the version it ran.

## 6. Surface

`POST /instances/{id}/upgrade` moves ONE tree, all or nothing, and refuses a non-root (§3c). A
refusal is an answer, not an error: the reply names the blocking instance and reason, including
a tree that cannot be planned (a live child in a `child_map` slot the target no longer declares).

**There is no `dry_run`**: on a running instance its answer describes a state the instance has
already left, and "would these versions be compatible at all" is `compat`, from documents.

`genctl upgrade` sweeps client-side, one call per tree, pausing a running instance and resuming
it after. Instance ids replace `--from`, which is the sweep's selector: each row's own version
goes out as that write's `from_version` assertion, and `--status` is refused. A tree already on
the target counts as already there, so re-running the same ids repairs a partial run.
`genctl compat <instance-id> --to …` asks the same pair as a question, scoped to the row's
process — one id, since a side carries one version per process.

## 7. Where it lives

`internal/validation` owns the migration and the checks, with no db/engine/api dependency, so
the whole gate is testable from two documents. The API handler therefore owns the composition:
plan the tree's versions (db), migrate each state (validation), write them together (db).
**`CompareSet` is a per-name loop today and
must stop being one when §3b lands**: it needs old-parent/new-child and new-parent/old-child in
one frame. The comparison's internals are in
[internal/validation/CLAUDE.md](../internal/validation/CLAUDE.md).

## 8. Deferred

- **Compat at apply time** — advisory block in `applyBatch`'s planning pass; must never
  refuse. After the general command, not instead.
- **Fan-in compat** ("which live versions can move to v5?") + live instance counts per
  task.
- **Conforming the input on upgrade** — opt-in, unlocks required-with-default, costs
  reversibility.
- **Task rename / `--at <taskID>`** — mechanically easy, excluded because nothing
  validates the operator's claim and a wrong remap is unrecoverable.
- **Auto-upgrade on channel move** — deliberately not built; if ever, an explicit flag.

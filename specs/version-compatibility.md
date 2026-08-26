# Instance upgrade

**Status: built.** The compatibility check this gates on is
[compat-command.md](compat-command.md)'s subject — what is compared, in which direction, and
how it is reported. This doc is the other half: **moving** an instance from one version to
another, once that check says it may.

The two halves answer different questions, and this one starts where the other stops: the
check reads two documents and never an instance, so it must assume every reachable state.
The gate has the row in hand.

How the two sides of a comparison are resolved — repeatable `--from`/`--to`, channel versus
pins, the dependency closure, what a missing counterpart means — is shipped behaviour and
lives in [internal/api/CLAUDE.md](../internal/api/CLAUDE.md), not here.

## 1. The gate refines the comparison with the row

Same comparison, with the old side's **presence** taken from the instance: stored output
keys required, `input`/`error` required iff non-empty, types still the old definition's
inferred ones. It loads no values — presence is a map key, and big values live out of line.

The assumption, stated plainly: a stored value conforms to the type the old version inferred.
Registration establishes it; the engine conforms deviations at runtime. **The gate may accept
what the report calls different, never the reverse.**

One of the comparison's imprecisions is refined here, and monotonically — refinement only
turns "different" into "tolerable": branch correlation, where a joined context makes
branch-only outputs merely optional. The other, demand, is **not refined at all**:
compat-command.md §2f records why pruning the required set to what is read is unsound, and
the argument applies here with more force. The gate performs the migration, and a migration
that reconciles only part of the row leaves it not conforming to the version it now runs —
which is the premise §1 above assumes.

## 2. The boundary is entry to a task

One observable state per task: the persisted entry context (`self` never survives an
advance; inline task chains are an optimization, not the model — every task end is a
boundary). Exactly two interrupted states carry an extra persisted value:

- `external` with a submitted result → require `oldResultSchema(T) ⊆ newResultSchema(T)`;
- `waiting`/`collecting` → the children's own rows (§3).

Both are why a parked task's `result_schema` is an *upgrade* concern and not only a contract
one (compat-command.md §2c).

Everything else is entry plus a timer/counter (a retry re-runs from the start; a
lowered `attempts` below a stored `retry_count` fails instead of retrying — the new
policy applied to an old counter, as asked). An **action-type change on the parked task
is refused** (a `child_map` → `child_list` leaves children carrying the wrong spawn
keys; no schema relation describes that). **A leased instance is refused**; the write is
conditional on `process_version`, `task`, and no live lease — the `task` predicate is
load-bearing (a worker can claim-advance-finish between read and write, leaving
`worker_id` NULL again; pinning task+version makes that a lost race a re-run picks up).
An **expired** lease is admitted deliberately (a crashed worker's instance is the one an
operator most wants to move; clearing `worker_id` instead would destroy the
`ReclaimedExpired`/`only_once` evidence). By status: `paused` ideal; `failed` opt-in
(prelude to retry); `failing`/`pausing` refused (draining); `completed`/`raised` refused
— no work moves, and the only effect would be re-lensing stored data (§5.7).

## 3. A running child and a waiting parent

### 3a. Prerequisite: remove `_spawn_result_schema` — **done, shipped**

The parent's `result_schema` used to be marshalled onto every child row at spawn.
Removed because: per-task data duplicated per child (1000 copies on a fan-out); children
and externals disagreed about a question with one answer (the external path always
resolved from the pinned definition); and a stale schema mid-path is the silent killer —
the conform *normalizes*, so a field added to both sides in one release arrived stripped
for in-flight children and the parent read null, uncatchably. Collect now conforms
against the parent's task as it currently stands. Backward-compatible, no migration.

That last sentence is what makes a parent's `result_schema` part of the upgrade question: a
parent already waiting will conform its child's output against the schema it runs *now*.

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

§3b answers "will the data fit"; this answers "does the system still describe itself".
A parent's definition names its child versions (explicit `action.version`, else
self-reference, else the baked dep row); upgrading a running child alone leaves the
parent executing a version its definition does not name — the instance-level twin of
registry drift, same remedy (re-apply the parent). The rule: a non-terminal instance
with a parent moves only in an operation that also moves the parent to a version naming
the child's target. It closes both ways, so the unit of upgrade is the **non-terminal
tree closure**. Terminal descendants stay put (their outputs are frozen; §3b covers
them). No ordering imposed: any mid-migration window is one of §3b's checked mixed
cases.

Built as described. The closure is `NonTerminalSubtree`, the non-root refusal is in the
handler, and which version each child moves to comes from `ResolveChildVersion` against the
parent's TARGET — one rule shared with the engine's spawn path, because two copies of it
drift silently and a parent running a child version its definition never mentions is exactly
the drift this section exists to prevent. A self-reference has no dependency row to read and
inherits the parent's target, which is only observable when the target is not the latest.

## 4. Upgrade writes one column

`process_version` — but that column is also the lens for redaction and display of data
the instance already holds, so the one-column write re-interprets the row (§5.7 accepts
this; refusing terminal instances is where it stopped being free). Mirrors the
resume/retry split: reversible (downgrade = swapped arguments), idempotent (already-on-
target skipped, so partial bulk runs are repaired by repetition), auditable (an
`EventInstanceUpgraded` entry is the whole story).

The case it costs: required-with-default input properties. Start-time fills defaults and the
upgrade does not — deliberately, because a default filled into a half-run instance disagrees
with every stored value that was derived from its absence (compat-command.md §2d). §8's
opt-in conform would fix it at the price of reversibility.

Bulk upgrade plans the whole closure first, then writes one row per transaction
(`applyBatch`'s shape; §3c makes partial runs data-safe, idempotency makes them recoverable).

## 5. What this cannot catch

1. **Meaning** — dollars → cents; the likeliest mistake, invisible to any static check.
2. **Routing** — new switch conditions; includes a child gaining a raise code its parent
   has no rule for (coverage was never guaranteed — D3).
3. **Tasks already run** never re-execute.
4. **Side effects already performed.**
5. **~~Stale keys~~ — fixed.** The output of a task the target no longer declares is now
   PRUNED: the migration conform strips what the layer does not name, and the layer is
   complete inside `outputs`. Nothing on the new version could read it (an expression naming
   it is refused at registration), so carrying it forward only grew the row and pinned
   whatever it referenced. The engine's own slots survive because the layer is deliberately
   partial at the top and `MigrateState` puts that half back.
6. **A renamed task** reads as removed + added → refused (§8 has the deferred `--at`).
7. **Redaction changes with the version — accepted.** A field secret-in-old, plain-in-new
   becomes visible for data stored earlier. In `input_schema` the change at least surfaces
   as a changed slot; in `config_schema` it is reported nowhere at all, since compat does
   not judge config (compat-command.md §6b). Redaction is a display concern; the DB always
   held the value.
8. **An in-flight result is judged by SCHEMA, which over-refuses on children — accepted for
   now.** An instance parked on a task that holds an outstanding result is gated on
   `old.result_schema ⊆ new`, strictly (contract optics, not storage optics: a worker's
   submission arrives from outside and no migration repairs it). That comparison is *forced*
   only for `external`, where the result is with a worker and there is no data to look at.
   A child batch is different — a running child moves with its parent, and registration
   already guarantees the version it moves TO fits; a completed child has its output on its
   row, where conforming the actual value would answer precisely. Judging both by the coarse
   relation can refuse a move that was in fact safe. Kept because the failure it prevents is
   worse than the one it causes: refusing leaves a tree paused and an operator informed,
   while allowing it wedges the parent at collect with a result nothing accepts.
9. **`only_once` may flip — accepted.** The new definition is the stated policy. The
   direction that bites: removing `only_once` from an interrupted task re-runs the side
   effect; §2 admits expired leases precisely for crashed workers, the same state this
   flag decides — read the slot report before moving such instances.

## 6. Surface

`POST /instances/{id}/upgrade` moves ONE tree: the non-terminal closure under a root, all or
nothing — a tree with one immovable member does not move, because refusing partially is the
point. It refuses a non-root instance outright: moving a child alone would leave its parent
collecting a version its own definition does not name.

    genctl upgrade <process> --from <version|channel> --to <version|channel>
                             [--status running,paused,failed] [--json]
    genctl upgrade <instance-id> [<instance-id> ...] --to <version|channel> [--json]

The CLI is the bulk form: it sweeps every instance of a process on `--from` with a cursor,
pausing a running one, moving it, and putting it back. `--status` narrows what it takes;
the default is every state that can move. Both sides are always named — there is no implicit
"latest". Never implicit in an apply or a channel move.

**Instance ids stand in for the process, and then only the target is named.** `--from` is the
SELECTOR — which rows the sweep takes — so ids, which select already, do not need it: each
version is read off its own row, goes out as that write's assertion, and its process resolves
a `--to` channel. `--status` is refused outright for the same reason, and a child is refused
before it is paused rather than after. Several ids are several calls, still one transaction per
tree: a refusal reports and the rest continue, and a tree already on the target counts as
"already there" rather than as a failure, so re-naming the same ids repairs a partial run and
exits 0. `genctl compat <instance-id> --to <version|channel>` (or `-f <file>`) asks that pair
as a question instead of making the move, scoped to the row's process — **one** id there, since
a side of a comparison carries one version per process. An id is told from a process name by
shape: a UUID, or `@last`.

**There is no `dry_run`.** It was in this doc and did not survive contact: on a RUNNING
instance the answer it gives is about a state the instance has already left, and what an
operator actually wants beforehand — "would these two versions be compatible at all" — is
what `compat` answers, from documents, without touching a row.

The write is conditional on everything that would make the migration stale (version, task,
status, lease), so a row that moved between the plan and the write loses the race rather than
being clobbered. A refusal names the instance and the reason; a tree that cannot even be
PLANNED (a child in a slot the target no longer declares) reports the same way rather than
failing the request.

## 7. Where it lives

`internal/validation` owns the dataflow and the child-ref checks, with no db/engine/api
dependencies, so the whole thing is testable from two documents. That constraint is why the
API handler owns the COMPOSITION — plan which versions the tree moves to (db), migrate each
state to the definition it is moving to (validation), write them together (db) — rather than
either package reaching into the other. It is also why the in-flight result check compares
SCHEMAS: conforming a completed child's actual output would need the object store, which
`validation` deliberately cannot reach (§5.8). **`CompareSet` is not a
loop over `Compare`**: §3b needs old-parent/new-child and new-parent/old-child in one frame.
The comparison's own internals — the two `$defs` pools, changed slots as a field comparison,
why diagnostics decompose above `isSubset` — are compat-command.md §7 and that package's
CLAUDE.md.

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

# Version compatibility and instance upgrade

**Status: partly implemented.** §1–§3 + §5a (the comparison, the changed-slot report) are
built: `internal/validation/compat.go`, `POST /definitions/compat`, `genctl compat`;
shipped behavior documented in `docs/`, silent-failure invariants in
[internal/validation/CLAUDE.md](../internal/validation/CLAUDE.md). **Not built:** §5b's
pairing check (submitted documents are validated before comparison instead, which covers
the case that mattered; the mixed stored-pair surfaces less precisely via §3b), the whole
upgrade half — gate (§3c), boundary rules (§4), tree closure (§5c), the write (§6), the
endpoints (§8) — and everything in §10.

As-built resolution notes (beyond §8's draft): `--from`/`--to` are repeatable
(`name@<version|channel>`); each side closes transitively over registered child versions
(explicit pin wins); explicitly-named-with-no-counterpart is an **error**, implicit
arrivals are **carried over**; target-only processes are `new`, same-version pairs
`nothing_to_compare` (neither pollutes the roll-up). And **"upgradable" was redefined**:
a new-version required property whose type admits null no longer breaks — not because
reads are forgiving, but because the gap is mechanically closable
(`IsSubsetAbsentAsNull` decides, `Validate(v, FillAbsentAsNull)` closes by writing the
null; the two must accept exactly the same gaps). §6 therefore gains a data write that
only ADDS keys — no destruction, downgrade leaves a stale `x: null` the old version
ignores. Applied only where nothing conforms afterwards (task contexts, input); the
output contract stays strict (collect conforms it for real).

Two halves: **compatibility** (could an instance on old continue under new without a
data-shaped failure?) and **upgrade** (move it, gated on the first). It is a **shape**
check: dollars → cents is `number` both ways and called compatible; the value is
catching accidental breaks, and §7 is the honest list of what it cannot see.

## 1. What is compared

Persisted state is `input`, `outputs.<id>`, and `error` (config is dropped — re-resolved
every tick, nothing in the row; an upgraded instance can still fail `engine.config`,
which the §3a config_schema slot makes foreseeable). `contextSchema` already folds the
must/may dataflow into one object per task, so the check is
`ctxOld(T).IsSubset(ctxNew(T))`: every context old can present at T, new accepts.
`isSubset`'s object rule covers both halves — newly-required outputs and changed types.

## 2. Why one task is enough

Output types are position-independent (all resolve through `$defs[<id>_output]`) and the
must-analysis is monotone along paths: if data satisfies `mustNew(T)`, every later task's
requirement is covered by data checked at T plus what the new definition produces en
route. So the single entry context at the instance's current task is sound for the whole
remaining run.

## 3. The comparison is the floor; the gate refines it

The comparison reads two documents, never an instance, so it reports every structural
difference. Two imprecisions, both refined later and both **monotone** (refinement only
turns "different" into "tolerable", never the reverse): branch correlation (a joined
context makes branch-only outputs merely optional) and demand (a new main-line output
becomes required everywhere, even if nothing reads it — §10's pruning).

### 3a. The report

Per-task subset verdicts plus **changed slots** — per task `action.url/headers/body/
result_schema`, `output`, `switch`, `on_error`, `timeout`, `only_once`, action type; per
definition `input_schema`, `config_schema`, `output`, `$defs`. Slot names, not a diff:
the reader applies the judgement the machine cannot (§7.7/§7.8 hazards are visible only
here — `isSubset` never inspects `secret`). `input` is hoisted to definition level (it
would report identically at every task); tasks match by ID (a rename is removal +
addition); a version that no longer analyses is reported per-version, never a 500.

### 3b. The output contract, reversed

`newOutput ⊆ oldOutput`, a separate verdict — consumers were written against the old
shape. `IsSubset`, not `NarrowsTo` (that is the privilege of a slot with a runtime
conform; nothing conforms here). Skipped when either side declares no output.

### 3c. The gate (not built)

Same comparison with the old side's **presence** taken from the row: stored output keys
required, `input`/`error` required iff non-empty, types still the old definition's
inferred ones. Loads no values (presence is a map key; big values live out of line).
Assumption stated plainly: a stored value conforms to the type the old version inferred
— registration establishes it, the engine conforms deviations at runtime. The gate may
accept what the report calls different, never the reverse.

## 4. The boundary is entry to a task (not built)

One observable state per task: the persisted entry context (`self` never survives an
advance; inline task chains are an optimization, not the model — every task end is a
boundary). Exactly two interrupted states carry an extra persisted value:

- `external` with a submitted result → require `oldResultSchema(T) ⊆ newResultSchema(T)`;
- `waiting`/`collecting` → the children's own rows (§5).

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
— no work moves, and the only effect would be re-lensing stored data (§7.7).

## 5. A running child and a waiting parent

### 5a. Prerequisite: remove `_spawn_result_schema` — **done, shipped with §1–§3**

The parent's `result_schema` used to be marshalled onto every child row at spawn.
Removed because: per-task data duplicated per child (1000 copies on a fan-out); children
and externals disagreed about a question with one answer (the external path always
resolved from the pinned definition); and a stale schema mid-path is the silent killer —
the conform *normalizes*, so a field added to both sides in one release arrived stripped
for in-flight children and the parent read null, uncatchably. Collect now conforms
against the parent's task as it currently stands. Backward-compatible, no migration.

### 5b. The pairing check (not built)

One schema governs both steps: `outC.NarrowsTo(S_parent)` as the parent currently
stands. Both-move is already guaranteed (batch registration checks it); the two mixed
rows — child moves only (`outC_new ⊆ S_old`), parent moves only (`outC_old ⊆ S_new`) —
are the cross-document check only `CompareSet` can compute. §3b transitively implies row
two but the pairing is **tighter**: a child dropping a field fails its general contract,
yet a parent that never named the field is unaffected, and only this says so. Per key
for `child_map`, per element for `child_list`; skipped without a `result_schema`.

### 5c. A running child may not move without its parent (not built)

§5b answers "will the data fit"; this answers "does the system still describe itself".
A parent's definition names its child versions (explicit `action.version`, else
self-reference, else the baked dep row); upgrading a running child alone leaves the
parent executing a version its definition does not name — the instance-level twin of
registry drift, same remedy (re-apply the parent). The rule: a non-terminal instance
with a parent moves only in an operation that also moves the parent to a version naming
the child's target. It closes both ways, so the unit of upgrade is the **non-terminal
tree closure**. Terminal descendants stay put (their outputs are frozen; §5b covers
them). No ordering imposed: any mid-migration window is one of §5b's checked mixed
cases.

## 6. Upgrade writes one column (not built)

`process_version` — but that column is also the lens for redaction and display of data
the instance already holds, so the one-column write re-interprets the row (§7.7 accepts
this; refusing terminal instances is where it stopped being free). Mirrors the
resume/retry split: reversible (downgrade = swapped arguments), idempotent (already-on-
target skipped, so partial bulk runs are repaired by repetition), auditable (an
`EventInstanceUpgraded` entry is the whole story). The case it costs: required-with-
default input properties (start-time fills defaults, upgrade does not) — §10's opt-in
conform would fix it at the price of reversibility. Bulk upgrade plans the whole closure
first, then writes one row per transaction (`applyBatch`'s shape; §5c makes partial runs
data-safe, idempotency makes them recoverable).

## 7. What this cannot catch

1. **Meaning** — dollars → cents; the likeliest mistake, invisible to any static check.
2. **Routing** — new switch conditions; includes a child gaining a raise code its parent
   has no rule for (coverage was never guaranteed — D3).
3. **Tasks already run** never re-execute.
4. **Side effects already performed.**
5. **Stale keys** — outputs of dropped tasks linger unread.
6. **A renamed task** reads as removed + added → refused (§10 has the deferred `--at`).
7. **Redaction changes with the version — accepted.** A field secret-in-old,
   plain-in-new becomes visible for data stored earlier; the definition-level slot is
   the only report. Redaction is a display concern; the DB always held the value.
8. **`only_once` may flip — accepted.** The new definition is the stated policy. The
   direction that bites: removing `only_once` from an interrupted task re-runs the side
   effect; §4 admits expired leases precisely for crashed workers, the same state this
   flag decides — read the slot report before moving such instances.

## 8. Surface (endpoints built for compat; upgrade endpoints not built)

Two **selectors** (channel | pins | submitted documents), paired by name — a
`{process, from, to}` triple cannot name a graph. Response: per-process verdicts
(`compatible`, `output_compatible` separate, hoisted `input`, per-task rows with
`changed` slots, removed/added tasks), a `children` pairing block, `unanalysable` rows
that force the roll-up **false** (an unchecked pair must never read as "fine").
`to.definitions` — compare before deploying — is the dominant workflow. Compat does not
validate child refs (that has its own endpoint; keeping resolution out stops compat
growing a second `applyBatch` planning pass).

Upgrade (not built): `POST /instances/{id}/upgrade` and a bulk form with `dry_run`
leading the docs; both operate on the non-terminal tree closure (a tree with one
immovable member does not move — refusing partially is the point). Default statuses
`running`+`paused`; `failed` opt-in; `completed`/`raised` unselectable. Never implicit
in an apply or channel move. CLI mirrors apply's ergonomics; every form names both
sides; the table carries a reason column.

## 9. Where it lives

`internal/validation` (owns the dataflow and the child-ref checks; no db/engine/api
deps, so pure two-document tests). `TaskContexts` + `Compare` + `CompareSet` —
`CompareSet` is not a loop over `Compare`: §5b needs old-parent/new-child and
new-parent/old-child in one frame. Pinned in that package's CLAUDE.md and tests: the two
`$defs` pools must stay separate (merging silently compares a schema against itself);
changed-slots is a field comparison in its own file with a struct-enumerating test;
diagnostics **decompose above `isSubset`** (slot by slot, then one property level) —
threading errors through the hot subset path was rejected; the §7.7/§7.8 hazards are
pinned as *expected* verdicts so a later change cannot quietly turn them into refusals.

## 10. Deferred

- **Demand-pruning the required set** (the gate's second refinement): prune
  `mustNew(T)` to what is actually referenced from T on, via backward reachability over
  `shape.Roots()`. Sound (unread values cannot break), applies to all three context
  slots, ~100–150 lines; four cases must be right (`AllOutputs`, `SelfPrevious`,
  on_error edges, process output at reachable terminals). Belongs to the gate — demand
  needs a position.
- **Compat at apply time** — advisory block in `applyBatch`'s planning pass; must never
  refuse. After the general command, not instead.
- **Fan-in compat** ("which live versions can move to v5?") + live instance counts per
  task.
- **Conforming the input on upgrade** — opt-in, unlocks required-with-default, costs
  reversibility.
- **Task rename / `--at <taskID>`** — mechanically easy, excluded because nothing
  validates the operator's claim and a wrong remap is unrecoverable.
- **Auto-upgrade on channel move** — deliberately not built; if ever, an explicit flag.

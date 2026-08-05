# Version compatibility and instance upgrade

**Status: partly implemented.** §1–§3 and §5a–§5b — the two-document comparison, the
changed-slot report and the parent/child pairing — are built (`internal/validation/compat.go`,
`POST /definitions/compat`, `genctl compat`), and shipped behavior is documented in `docs/`
(reference → Version compatibility). The invariants that break silently live in
[internal/validation/CLAUDE.md](../internal/validation/CLAUDE.md).

**The upgrade half is not built**: the gate (§3c), the boundary rules (§4), the tree closure
(§5c), the one-column write (§6) and the two upgrade endpoints and `genctl upgrade` (§8).
Nothing in §10 is built either. This document stays as the design record for all of it.

1. **Compatibility** — given two versions of a process, decide whether an instance running
   the older one could continue under the newer one without a data-shaped failure.
2. **Upgrade** — move a live instance from one version to another, gated on (1).

The motivating changes are the boring ones: an optional input parameter, a fixed URL, a new
header. None changes the shape of anything in flight, and today each of them strands every
running instance until it finishes — for a process parked on an `external` task, that can be
weeks.

It is a **shape** check: it compares inferred schemas, not meaning. Dollars → cents is
`number` before and after and will be called compatible. The value is catching the
*accidental* break — a required input added, an output whose type changed — not certifying a
migration. §7 is the full list of what it cannot see, and every surface says so.

## 1. What is compared

An instance's persisted state is `input`, `outputs.<taskID>` for each task that has run and
projects an `output`, and `error` once an error route has fired. (`output` is written only at
completion; `self` never survives the `advance` that builds it — see §4 for the two exceptions
and why they are the only ones; `_external*` / `_children` / `_spawn_*` are engine
bookkeeping.)

`internal/validation` already computes this per task: `computeContextSets` runs the forward
must/may dataflow and `contextSchema` folds it into one object — `{input, outputs: {<id>:
…}, error}` — with a property required where the value is guaranteed and optional where it is
merely possible. So the check is

```
ctxOld(T).IsSubset(ctxNew(T))
```

*every context the old definition can present at T is one the new definition accepts at T*.
`isSubset`'s object rule does both halves with no new code: `super.required ⊆ sub.required`
(an output the new version reads but the old never produced) and `subProp ⊆ superProp` for
co-declared properties (a type that changed).

**`config` is dropped from both sides.** It is re-resolved from the environment every tick,
so there is nothing in the row to compare. Registration refuses unset required vars, but
against the environment *at registration time* on whichever host served it — so an upgraded
instance can still fail `engine.config` on its next tick, exactly as a fresh one would. The
`config_schema` changed-slot (§3a) is what makes that foreseeable.

## 2. Why one task is enough

The instance sits at `T` and will run `T`, then wherever the new definition routes. Checking
every reachable task is unnecessary:

- **A task output's type is position-independent** — every `outputs.<id>` resolves through
  `$defs[<id>_output]`, one entry per task id.
- **The must-analysis is monotone along a path** — for any `U` reachable from `T`,
  `mustNew(U) ⊆ mustNew(T) ∪ produced(T⇝U)`, since `mustNew(U)` is the meet over all paths
  into `U` and the path through `T` contributes exactly that.

So if the data satisfies `mustNew(T)`, then at every later `U` the available set is
`data(T) ∪ produced(T⇝U) ⊇ mustNew(U)`, and each value is either one checked at `T` or one
the new definition produced. **Checking the single context at the instance's current task is
sound for the whole remaining run.** `error` follows the same argument, and its type is fixed,
so it cannot drift.

`mustNew(T)` is the *in*-set, so this is the entry context of §4 — the one state per task
that actually exists — and the argument holds whether the instance is about to run `T` or
parked inside it.

## 3. The comparison is the floor; the gate refines it

The comparison reads two **documents** and never sees an instance, so it must assume every
reachable state and report a structural difference wherever one exists — without judging
whether it would hurt. That judgement needs a position, and belongs to the gate.

Two imprecisions follow. Neither is fixed in the comparison; the gate fixes the first (§3c)
and later the second (§10):

1. **Branch correlation.** `ctxOld(T)` joins every path into `T`, losing correlation exactly
   as [path-sensitive-output.md](path-sensitive-output.md) documents. If only one of two
   branches runs `X`, then `X` is merely optional at `T`, and a new version needing it is
   reported different — though instances that came via that branch are fine.
2. **Demand.** A new main-line task carrying an `output` becomes *required* at every later
   task, so every old context lacking it is reported different — even if nothing reads it.

Both corrections are **monotone**: they only turn "different" into "tolerable". That is what
makes the layering safe — no verdict computed here is invalidated by a later refinement.

### 3a. What the comparison reports

`ctxOld(T) ⊆ ctxNew(T)` for every task in both, plus §3b. Registry-only; no instances needed.

The verdict is not the deliverable. Because the check is blind to meaning (§7.1), the report
also names **which slots differ**: per task `action.url`, `action.headers`, `action.body`,
`action.result_schema`, `output`, `switch`, `on_error`, `timeout`, `only_once` and the action
type; per definition `input_schema`, `config_schema`, `output`, `$defs`. Slot names, not a
text diff — enough for the reader to apply the judgement the machine cannot, and cheap enough
to be a field comparison over two `model.Task` values.

The **definition-level** slots are not symmetry. §7.7 and §7.8 accept hazards on the grounds
that the operator can see them coming, and `isSubset` never inspects `secret` — so a schema
changing nothing but `secret: true` compares equal. A `result_schema` is caught by its
per-task entry; `input_schema` and `config_schema` belong to no task and would have none.

Settled by how the code already works:

- **`input` is hoisted out of the per-task loop** — it sits in every task's context, so
  comparing it inside would report the same break N times. Compared once, at definition level.
- **Tasks match by ID**, the only handle. A rename reads as one removal plus one addition;
  removals are listed separately, since an instance there has nowhere to go.
- **A version that no longer analyses is reported, not fatal.** Old rows were validated under
  the rules of their day, so `Generate(old)` can fail. Per-version "cannot analyse", not a 500.

### 3b. The output contract, compared the other way round

`newOutput ⊆ oldOutput` — **reversed**, and a separate verdict never folded into the one
above. Consumers (a parent's `result_schema`, an API caller) were written against the old
shape, so every value the new version can produce must be one the old could. A single boolean
over two opposite-direction checks would be meaningless.

`IsSubset`, not the `NarrowsTo` used by `checkChildOutputType`: that is the privilege of a
slot where a runtime conform stands behind the claim, and here two inferred types are compared
with nothing conforming either. Skipped when either version declares no process `output`.

### 3c. What the gate adds

The first refinement. Same comparison, with the old side's **presence** taken from the row:

- `outputs`: the keys actually in `outputs_data`, all **required** — they are there.
- `input` / `error`: required iff the column is non-empty.
- **Types stay the ones the old definition inferred** — per-task-id, so position never
  changes them.

Precise on presence, sound on types, and it loads **no value**: a big output lives out of line
in `process_objects`, and validating real data would fetch every one for every instance in a
bulk run. Presence is a map key.

The assumption, stated plainly: *a stored value conforms to the type the old definition
inferred for it.* Registration establishes that, and where a value could deviate (a `{}`
narrowed by a `result_schema`) the engine conforms it at runtime.

This is the **gate**; §3a is the **report**. The gate may accept what the report calls
different — never the reverse.

## 4. The boundary is entry to a task

An instance has exactly one observable state per task: the persisted context on **entry** to
`T` — `computeContextSets`' in-set, available before `T`'s own output. Nothing finer is
reachable. A task's action result, its `output` projection and its `switch` all run inside a
single `advance`, and only the projection is written, which is why `self` is excluded (§1).

The engine additionally collapses a chain of call-less tasks into one claim and one write
(`maxInlineTasks`). That is an **optimization, not the model**: compat treats every task end
as a boundary, so a change at a routing task is checked like any other, and nothing here may
assume an instance cannot be found sitting at one.

Exactly two states break the entry rule — a task whose execution was interrupted with an
intermediate value **already persisted**:

| interrupted at | value already stored | read next as |
|---|---|---|
| `external`, result submitted | `_external_result`, validated under the old schema | `self.result` at `T`'s new `result_schema` |
| `waiting` / `collecting` | the children's own outputs, on their rows | §5 |

Only the first needs a rule here: **when `_external_result` holds a submitted value, require
`oldResultSchema(T) ⊆ newResultSchema(T)`.**

Everything else is entry to `T` with a timer or a counter attached, and needs no rule: a retry
backoff re-runs `T` from the start, an armed delay resumes into the switch with no result, and
a queued external holds no value yet — `resolveExternal` looks the schema up from the pinned
definition when the result arrives, which is that value's first and only contact with one.

An **action-type change on the parked task is refused**, not subset-checked: a `child_map`
that became a `child_list` leaves in-flight children carrying `_spawn_child_key` where
`buildListChildOutput` demands `_spawn_index`. No schema relation describes that.

One report note falls out of the retry case: lowering `attempts` below the instance's
`retry_count` makes the next attempt fail instead of retry — the new policy applied to an old
counter, which is what was asked for.

**A leased instance is refused.** Lease fencing is not implemented
([lease-fencing.md](lease-fencing.md)), so a worker mid-advance holds a copy at the old
version. It cannot corrupt the upgrade (`UpdateInstance` does not write `process_version`) but
would finish the current task under the old definition afterwards, which no report could
honestly describe. The write is conditional:

```sql
UPDATE ... SET process_version = :to
WHERE  id = :id AND process_version = :from
  AND  task = :task_verified
  AND  (worker_id IS NULL OR lease_expires_at <= :now)
```

**The `task` predicate is load-bearing.** The lease guard alone does not pin the task the
verdict was computed for: a worker can claim, advance and finish — `UpdateInstance` clears
`worker_id` on the way out — between the read and the write, leaving the column `NULL` again
and the instance upgraded on a verdict for a task it has left. Pinning `task` and
`process_version` makes that a lost race, which the bulk form reports and a re-run picks up.

It admits an **expired** lease deliberately: a crashed worker's instance is one an operator
most wants to move, and the alternative — clearing `worker_id` — erases the evidence
`ReclaimedExpired` derives from and re-runs `only_once` tasks that must never re-run (see
[internal/engine/CLAUDE.md](../internal/engine/CLAUDE.md)). Upgrade writes neither lease column.

By status: `paused` is the ideal point; `failed` is allowed but opt-in, since upgrading it is
a prelude to `retry`. `failing` and `pausing` are refused as draining states.
**`completed` and `raised` are refused too** — neither can execute again (a raise is a
conclusion, not an interruption, and is not retryable), so an upgrade moves no work and its
only effect is re-lensing stored data through a different schema (§7.7).

## 5. A running child and a waiting parent

### 5a. Prerequisite: remove `_spawn_result_schema`

**Decided.** A leftover, and worth removing with no upgrade feature in sight.

`resolveAndValidateChildOutput` conforms a collected child's output against
`_spawn_result_schema` — the parent's `result_schema` marshalled onto **every child row at
spawn**. It is redundant: the only functional read is `collect.go`, and
`buildChildOutput(task, siblings)` already receives the parent's task. The **external** path
looks it up from the pinned definition instead, and the two have never differed only because
a version's content is immutable and an instance's version never changes — upgrade is the
first thing that breaks that.

Three reasons, one about upgrade:

- **Per-task data duplicated per child** — `buildListChildren` computes it once and copies the
  string into every spawn context; 1000 copies for a 1000-element fan-out. It sits beside
  `_spawn_child_key` / `_spawn_index`, which genuinely *are* per-child.
- **Children and externals disagree** about a question with one right answer.
- **A stale schema mid-path**, where the conform *normalizes* and strips undeclared properties:
  a field added to child output and parent `result_schema` in one release arrives stripped for
  in-flight children, and the parent reads `null` — silently, and uncatchably if the new schema
  declares it optional.

Backward-compatible, no migration: nothing else reads the key, and for a non-upgraded instance
the value read instead is byte-identical. One read site, three writes, three comments —
including [unknown-type.md](unknown-type.md), whose argument survives; only the schema's
*address* changes.

### 5b. What remains

One schema governs both steps: the child produces `outC`, and collect conforms it against the
parent's `result_schema` *as the parent currently stands*, which is also the type the parent
reads it at. So `outC.NarrowsTo(S_parent)` — `NarrowsTo` because the conform is real, matching
`checkChildOutputType`.

| | constraint | checked by |
|---|---|---|
| both move | `outC_new ⊆ S_new` | **nobody — already guaranteed.** Applied together, `buildResolvedDeps` bakes `P_new → C_new` and `ValidateChildProcessRefs` checks exactly this. |
| child moves only | `outC_new ⊆ S_old` | the batch: old **parent** + new **child** |
| parent moves only | `outC_old ⊆ S_new` | the batch: new **parent** + old **child** |

The mixed rows are the cross-document check no single-process comparison can compute, and the
reason `CompareSet` exists. Their symmetry is the sign the model is right: a pair is
compatible when whichever side moved still fits the one that did not.

Per key for `child_map`, per element for `child_list`; skipped where the task declares no
`result_schema`. A self-reference is the same check with one process; it is single-level, so
unlike `topoSort` it needs no cycle guard.

§3b appears to subsume row two — `outC_old ⊆ S_old` holds by registration, so
`outC_new ⊆ outC_old` implies it transitively. True, and why the pairing check earns its
place: it is **tighter**. A child that drops a field fails its general output contract, but a
parent whose `result_schema` never named that field is unaffected, and only this can say so.

Scope is bounded by what was submitted; a child under a parent version outside the batch is
covered exactly by the gate, from the parent row's pinned version.

### 5c. A running child may not move without its parent

§5b answers *will the data fit*. It does not answer *does the system still describe itself*.
A parent's definition names the version of each child it runs — resolved at spawn by
`resolveChildVersion`: an explicit `action.version` first, else the caller's own version for a
self-reference, else the baked `process_dependencies` row. Upgrade a running child alone and
the parent is executing a version its definition does not name. The outputs may fit; the
record is wrong.

That is the instance-level twin of drift the registry already refuses to paper over: applying
a child does not re-register its parents (deliberately — no cascade), and `status` reports the
result as a stale ref. The remedy is the same at both levels, re-apply the parent so the dep
re-bakes:

> **A non-terminal instance with a parent may be upgraded only in an operation that also moves
> the parent to a version whose declared child version for that task and key is the child's
> target.** Checked through the same resolution order the engine uses, not by reading
> `process_dependencies` alone. A self-reference has no dep row: parent and child must land on
> the same version.

**It closes both ways.** A child moving forces its parent, which is itself an instance whose
version changed, so it forces *its* parent — up to the root. And a parent moving to a version
naming `C_new` mismatches any running child still on `C_old`, so it forces those down. The
unit of upgrade is therefore the **non-terminal tree closure**, not one row. Terminal
descendants stay put and need no rule: their outputs are already frozen, so §5b's `outC_old ⊆
S_new` covers them — which is exactly the half-collected fan-out, thirty children done and
twenty running.

**No ordering is imposed**, because none is needed: whichever row is written first, the window
before the second is precisely one of §5b's two mixed cases, and both are checked before any
write. A transient mismatch mid-migration is data-safe; a persistent one is the thing being
banned.

## 6. Upgrade writes one column

`process_version` — mechanically. But the column governs more than what runs next:
`getInstance` redacts an instance's context against
`GetDefinition(inst.ProcessName, inst.ProcessVersion)`, so the pinned version is also the lens
through which data the instance *already holds* is displayed. A one-column write re-interprets
every value in the row. Accepted (§7.7) rather than checked, and refusing terminal instances
(§4) is where it stopped being free.

This mirrors the resume/retry split ([pause-resume.md](pause-resume.md)): pause is
non-destructive, so resume is a status flip; upgrade is a version flip for the same reason.
Three properties follow, all worth more than the case they cost:

- **Reversible** — downgrade is the same operation with the arguments swapped.
- **Idempotent** — an instance already on the target is skipped, so a partial bulk run is
  fixed by repeating it.
- **Auditable** — no rewritten data to explain; the audit entry (`EventInstanceUpgraded`,
  carrying `from` / `to`) is the whole story.

The case it costs: an input property the new version makes **required with a default**. At
start `ValidateInput` fills defaults, so a new-version instance has it and an old-version one
does not — `ctxOld ⊄ ctxNew`, refused, though the author has said what absence means.
Conforming the stored input would fix it, and would also silently drop properties the new
version stopped declaring, turn the write into a value-slot write (the input may be
externalized), and break reversibility. §10 keeps it as an opt-in follow-up.

Bulk upgrade **plans the whole closure, then writes one row per transaction** —
`applyBatch`'s shape, for its reason: the §5c constraint spans instances, so nothing may be
written until every member has a verdict. The write stays per-row because the row count is
unbounded and idempotency makes a partial run safe to repeat, and §5c's ordering argument is
what makes a partial run *data*-safe as well as recoverable.

## 7. What this cannot catch

Stated at every surface in the user's words, not as a footnote:

1. **Meaning.** Dollars → cents, a reused enum member, an id in a new namespace. Same shape,
   different value. The likeliest mistake, and no static check can see it.
2. **Routing.** New `switch` conditions decide where the instance goes; shape-compatible and
   behaviourally different is a normal outcome. Includes a child gaining a `raise` code its
   waiting parent has no rule for — not checked, because R5 validates rules against the raise
   set in one direction only and coverage is already a stated non-guarantee (D3). Checking it
   here would assert something registration never did.
3. **Tasks already run.** A task inserted *before* `T` never executes for this instance.
4. **Side effects already performed.** The old URL was already called.
5. **Stale keys.** An `outputs.<id>` for a dropped task stays in the context, unread;
   `output_order` keeps its ids.
6. **A renamed task** reads as removed + added, so the upgrade is refused (§10).
7. **Redaction changes with the version — accepted.** A property `secret: true` in the old
   version and not the new becomes visible over the API, for data stored long before the
   upgrade. `isSubset` never inspects `secret`, so no verdict sees it; the definition-level
   changed-slot (§3a) is the only thing that reports it. The call was that redaction is a
   display concern — the database always held the value in the clear. Also why terminal
   instances are refused (§4), where re-lensing is the *only* effect.
8. **`only_once` may flip — accepted.** The new definition is the author's stated policy, so
   it applies and the change is reported as a slot. The direction that bites: removing
   `only_once` from a task whose previous attempt was interrupted lets it re-run, and the side
   effect happens twice. §4 admits expired-lease instances precisely so a crashed worker's
   instance can move — the same state this flag decides. Read the `only_once` entry before
   moving instances a worker died holding.

## 8. Surface

**Build the general two-version comparison first.** It stands alone — "what did I change, and
can anything running observe it?" needs no upgrade machinery — and everything else calls it.
Convenience wrappers are §10.

A `{process, from, to}` triple cannot name a graph, so the request is **two selectors**, each
resolving to one version per process name, paired by name:

```
POST /definitions/compat
{
  "from":    {"channel": "latest"}  |  {"versions": {"order_pipeline": 2, "invoice": 1}},
  "to":      {"channel": "next"}    |  {"versions": {...}}  |  {"definitions": [ ...docs... ]},
  "process": "order_pipeline"       // optional: scope to this process and its subtree
}

→ {compatible,                                    // conjunction over everything below
   processes: [{name, from, to,
     compatible,                                  // instance continuation (§3a)
     output_compatible,                           // consumer contract (§3b), separate
     input: {compatible, reason?},                // hoisted: one answer, not one per task
     tasks: [{task, compatible, reason?, changed: [slot...]}...],
     removed_tasks: [...], added_tasks: [...]}...],
   children: [{parent, parent_version, task, child_key?, child, compatible, reason?}...],
   unanalysable?: [{name, version, reason}]}
```

`to.definitions` is the dominant workflow — the documents `apply` would take, compared against
what is deployed *before* deploying them; they have no version yet, so they report `to: null`.
The `process` scope reuses the `subtree` helper in `handlers_definitions.go`.

**An unanalysable version makes the roll-up `false`, never `true`** — it was compared against
nothing, and a top-level answer indistinguishable from "checked, and fine" is worse than no
report. Same for a name present on one side only: reported unpaired, never dropped.

**Compat does not validate.** A submitted document's child refs are unresolved and `Generate`
does not need them — only `ValidateChildProcessRefs` does, and that has its own endpoint.
Keeping batch resolution out is what stops compat growing a second copy of `applyBatch`'s
planning pass.

```
POST /instances/{id}/upgrade   {version} | {channel}
POST /instances/upgrade        {process, from_version|from_channel, to_version|to_channel,
                                dry_run, statuses?}
  → [{id, from, to, upgraded, reason?}...]
```

Both forms operate on the **non-terminal tree closure** (§5c), not on the rows they name: an
id selects the tree it belongs to, and a process selects every tree containing a matching
instance. So the response always lists more rows than were asked for, and a tree with one
member that cannot move does not move at all — refusing partially is the point, not a
limitation.

`dry_run` writes nothing and is the form the docs lead with. Default `statuses` is `running` +
`paused`; `failed` is opt-in (§4); `completed` / `raised` are not selectable — they stay put
while their live relatives move, which §5b covers.

An upgrade is **never** implicit in an apply or a channel move: promoting `latest` and
migrating live instances are different decisions, and only the second can break something
already running.

```
genctl compat <process> <from> <to>          # two stored versions of one process
genctl compat -f ./processes --from latest   # submitted graph vs a channel (mirrors apply -f)
genctl compat --from stable --to latest [<process>]   # channel to channel, optional subtree

genctl upgrade <id> --to <version|channel>   # single item, id first positional
genctl upgrade --process p --from 2 --to 3 [--dry-run]
```

One endpoint shape, three ergonomics — the CLI hides the selector verbosity, not the API.
Every form names both sides: defaulting one hides which two documents were compared. The table
is per process with the §5b pairing results in their own block (a parent/child break is not a
fact about either document alone), and must carry the reason column — a bare "incompatible"
tells an operator nothing actionable.

## 9. Where it lives

`internal/validation/compat.go`. The package owns `computeContextSets`, `contextSchema` and
the child-ref checks — the closest relative is `ValidateChildProcessRefs`, the same thing
across a parent/child boundary rather than a version one — and depends on neither `db`,
`engine` nor `api`, so the whole check is testable from two `model.ProcessDefinition` values.

One thing must be exported that is currently internal to `buildInputs`:

```go
// TaskContexts returns the context schema at every task: one Generate, one
// computeContextSets. The "config" property is stripped: it is not instance state.
func TaskContexts(def *model.ProcessDefinition) (map[string]schema.Schema, error)

// Compare is §3a-3b. It reads two documents and nothing else.
func Compare(old, new *model.ProcessDefinition) (Report, error)

// CompareSet is Compare over a name-paired set, plus the §5b pairing check. Pairs with
// no counterpart are reported, never dropped.
func CompareSet(old, new map[string]*model.ProcessDefinition) (SetReport, error)
```

`CompareSet` is not a loop over `Compare`: the §5b check needs `old[parent]` with `new[child]`
*and* `new[parent]` with `old[child]` in one frame, which is why the set form exists rather
than being left to the client. A single pair is `CompareSet` with one entry. The gate later
adds a form taking observed presence sets.

**§5a is a prerequisite, not a parallel workstream** — building against the frozen-schema
behaviour means encoding a rule §5a deletes and shipping a report naming a hazard it removes.

**Tests.** `Compare` / `CompareSet` are a pure algorithm with no HTTP or DB, so Go tests in
`internal/validation`, table-driven over definition pairs — also the cheapest place to pin the
§7.7 / §7.8 accepted hazards as *expected* verdicts, so a later change cannot quietly turn them
into refusals. Endpoint, CLI table and the upgrade write (especially §4's lost race, needing a
concurrent claim) are JS e2e.

**Changed-slots is a field comparison, not a schema question** — its own file, so "which slots
differ" never acquires opinions about which differences matter. Adding a field to `Action` or
`Task` means adding it here too and nothing fails if you forget; the slot silently stops being
reported. Worth a test that enumerates the struct.

**Two `$defs` pools already compare correctly.** `subsetWith` takes `subDefs` and `superDefs`
separately, so each side dereferences its own — which is what lets independently-generated
contexts be compared even though `uniqueDefName` may name the same concept differently, and
cycle detection keys on the *pair* so same-named different definitions stay distinct. Merging
the pools silently compares a schema against itself.

**Diagnostics are the hard part, not the check.** `isSubset` returns a `bool` and is on every
hot validation path, so threading an error through it is a large change to load-bearing code
for one caller. **Decompose above it**: slot by slot, then one level into properties within a
failing slot. That yields `outputs.charge.amount: number → string` and `input.currency: newly
required` from unmodified calls. Reach for a `schema.Explain` only if a real case proves it
insufficient.

## 10. Deferred

- **Demand-pruning the required set** — §3's second imprecision. Prune what the new version
  requires at `T` to what is actually referenced from `T` onward, via a backward reachability
  fixpoint collecting `shape.Roots()` from every slot. Sound (a value nothing reads cannot
  break by being absent), preserves §2 (demand is backward-transitive), and applies to **all
  three context slots** — `Roots` reports `Input` and `Error` beside `Outputs`, and input is
  the cleanest case, since `ValidateInput` runs once at creation and upgrade never re-validates
  (§6).

  Belongs to the **gate, not the comparison**: "is this read from here on" needs a position.
  Worth building when the gate lands — roughly 40-50% of tasks in this repo's examples carry an
  `output:`, so a main-line insertion trips the false alarm about half the time. Budget 100-150
  lines: there is no dependency graph to reuse
  ([outputorder.go](../internal/validation/outputorder.go) is explicit that inference is
  demand-driven with none maintained), and four cases must be right — `AllOutputs` (demands
  everything), `SelfPrevious` (aliases `outputs.<self>`), `on_error` routes as edges, and the
  process `output` at every terminal reachable from `T`.
- **Compat at apply time** — `PUT /definitions` returning an advisory block against the
  version it supersedes, so `genctl apply` prints "v3 saved; differs from v2 at task ship"
  without a second round trip. Read-and-judge, so it lives in `applyBatch`'s planning pass and
  must never refuse. After the general command, not instead of it.
- **Fan-in compat** — "which versions with live instances can move to v5?", a loop over the
  pairwise comparison. Plus **live instance counts** per task, so a difference at a task nobody
  occupies reads as the non-event it is.
- **Conforming the input on upgrade** (§6) — opt-in, reported as a data change, unlocking
  required-with-default properties. Costs reversibility.
- **Task rename / `--at <taskID>`** — mechanically easy, since the write could carry a task
  too; excluded because nothing validates the operator's claim and a wrong remap is
  unrecoverable.
- **Auto-upgrade on channel move** — deliberately not built (§8). If ever, behind an explicit
  flag on the apply.

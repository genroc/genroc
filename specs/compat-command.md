# Compat: two checks over one comparison

`genctl compat` / `POST /definitions/compat`: what each check compares and in which direction,
how a finding is addressed, and what an operator may excuse. It reads two documents and never
an instance, so it must assume every reachable state. Moving an instance — the gate, with the
row in hand — is [version-compatibility.md](version-compatibility.md).

## 0. Status

**Built.**

## 1. Two checks

- **Upgrade** — can an instance running the old version continue under the new one? About rows
  this deployment already owns. **Non-negotiable** (§5).
- **Contract** — does the process still honour what the outside world was written against?
  About parties outside the deployment. **Excusable.**

They disagree in both directions, so one word would be wrong both ways: a main-line task
inserted breaks upgrade and no contract (`inserted-task-with-an-output`), while a newly required
nullable input property is upgradable — the migration writes the null in (§2d) — and refuses a
caller that omits it (`nullable-input-added`).

**Children are not a third check.** A bundle is checked against itself by registration, so
upgradability stays per-process; a child's own `output` is still a contract, since its
consumers include parents outside the bundle.

## 2. Upgrade: every state the old version can persist must fit the new one

### 2a. What is compared

Persisted state is `input`, `outputs.<id>` and `last_error`; `config` is stripped, being
re-resolved every tick. The check at a task is `ctxOld(T) ⊆ ctxNew(T)` over its entry context,
with `input` compared once rather than per task.

**One context per task covers the whole remaining run.** Output types are position-independent
(every `outputs.<id>` resolves through `$defs[<id>_output]`) and the must-analysis is monotone
along a path, so what holds at T covers everything reachable from it. Checking a *different*
task is wrong, not merely less precise.

### 2b. The task set: removal breaks, addition does not

**Every task the old version has, the new one must have** — an instance on a dropped task has
nowhere to continue. A set difference, reported directly; no schema relation describes it.

**Adding a task is not a second rule.** A task on a branch makes its output merely possible, so
the context marks it nullable and §2d's tolerance closes the gap. A task on the **main line**
makes `outputs.<new>` guaranteed, which a row that never passed through it cannot satisfy: a
break, even where nothing downstream reads it (§2f says why that stays).

### 2c. Parked mid-task: external and children

An action whose task can hold a **parked** instance carries state the entry context does not,
so its result schema is part of the upgrade check (`ActionType.Holds().Result`, shared with the
engine):

- **`external`** — a result still to come is conformed against the schema the instance runs
  *now*.
- **`child`, `child_map`, `child_list`** — collect conforms each child's output against the
  parent's result schema as it currently stands (version-compatibility.md §3a), so a narrowing
  strands a parent already waiting.

**`fetch` cannot park**: request and response happen inside one advance with nothing persisted
between. That is why this is a rule about parking rather than a list.

**`raises` is the same promise on the error channel, compared the same way** — per code,
`old ⊆ new`, strictly, filed under contract always and upgrade where the task parks. Both
answers arrive from outside and no migration repairs them; registration checks a child's
raisable codes but never an external worker's. A code the new version stops declaring is a
break, not a narrowing: a worker's submission of it is refused before its payload is read, and
a raised child's payload is no longer readable. A code it adds constrains nobody.

**Which process a child call names needs no rule.** Registration established that the old
call's output fits the old schema (`checkChildOutputType`, or collect's conform), so `old ⊆ new`
carries a child in flight across whatever it is an instance of. An identity check would be a
false break in the member that cannot be excused (§5).

**What a pairing cannot see is an addition**, having no old schema to carry the premise. Both
are reported directly:

- **a `result_schema` (or `raises` payload) where none was declared** — a conform now stands
  where none did; contract always, upgrade where the task parks. `{}` is not an addition: it
  can fail nothing (specs/unknown-type.md).
- **a `child_map` key** — its keys are its calls, so an added key is §2b's main-line task: a
  value a parent that spawned before it existed cannot hold. Upgrade only.

**A removed key is not judged**: collect keys siblings by `_spawn_child_key`, and the output
conform strips an orphan. It still gets a `(not judged)` row, being a call no longer made.

**`over` and the pinned `version` are unjudged**: a `child_list`'s array is consumed at spawn
(each child carries its `_spawn_index`), and a moved child reports on its own row.

**Routing is not covered.** A child in flight may raise a code the new version does not route;
that changes where an instance goes, not what it holds, and fails loudly — so it sits beside
`switch`, unjudged.

The cross-document half — a child moving without its parent — is version-compatibility.md
§3b's pairing check, also unbuilt.

### 2d. The relation may be relaxed, because the gap is closable

`IsSubsetAsStored` decides a gap is closable and `Validate(data, ConformToSchemaExactly)` closes
it. **They must accept exactly the same gaps**: a relation tolerating more promises an upgrade
that then fails; a conform closing more is dead code. The gap is null versus missing, and a
version change opens it both ways:

| the row holds | the new schema says | reconciliation |
|---|---|---|
| nothing | required, admits null | the null is written in |
| a null | optional, will not take null | the key is removed |

A **required** non-nullable property holding null can be neither kept nor removed, so the
relation refuses it.

**The conform never fills a default.** A default filled at creation precedes every read; one
filled into a half-run instance contradicts values already computed from its absence. So a
value is only ever present at upgrade because it was already there.

**The conform is total**: it runs over the whole context, not the part something reads, because
the next comparison assumes the row conforms to the version it now runs (§2f).

### 2e. One schema, two sets: before the conform and after it

A schema denotes two sets depending on when it is read:

- **as an acceptance predicate** — what may arrive. A defaulted optional property may be absent,
  and `conformObject` rejects an absent *required* one before looking for a default.
- **as a description of conformed data** — what is stored. The same property is always present,
  because the conform filled it.

The contract check compares acceptance predicates; the upgrade check compares conformed data.
Same schemas, same direction, two meanings — which is why a property gaining `required` while
carrying a default is upgradable and contract-breaking at once (§3b). This is **not** the
input/output distinction, which decides direction only (§3a).

**The modes must stay in step.** `Validate` distinguishes the cases (`Strict` fills defaults,
`ConformToSchemaExactly` does not), and the stored relation's matching rule is that *guaranteed
present* means **required or carrying a default**, at every depth — the rule `lookupProperty`
already applies to reads.

**The rule reads the sub side only.** A default on the old schema means the value is in the row.
A default on the new schema means nothing here: the migration fills none, so what the new schema
demands of a carried row is its `required` set. Reading it symmetrically reports a false break
at `input` for an edit that only adds a default; that edit's real consequence (reads become
non-null) surfaces where it is read, in the inferred context.

### 2f. Rejected: requiring only what is read

The context at T guarantees everything the new definition produces on the way, including
values nothing reads — which is why a dead main-line output still breaks. Pruning `mustNew(T)`
to what is referenced is **unsound**, though it looks monotone.

**Every upgrade must leave the row conforming to the new version in full**, because each
comparison sees only two adjacent versions and reasons from the premise that the old side's
data satisfies the old side's schema. Pruning leaves unread values unreconciled and falsifies
that premise silently: v1→v2 changes an unread `outputs.a.x` from number to string and passes,
the row keeps its number, and v2→v3 then compares the schemas correctly and still lets the
number surface where the type says string, with nothing ever having reported it.

## 3. Contract: the outside world

### 3a. Direction is decided by who submits the value

| address | submitter | relation | the conform behind it |
|---|---|---|---|
| `input` | caller | old ⊆ new | `ValidateInput` at creation |
| `output` | us | new ⊆ old | a waiting parent's result schema at collect |
| `<task>:fetch.result` | the service | old ⊆ new | collect |
| `<task>:external.result`, `.raises` | the worker | old ⊆ new | submit |
| `<task>:child*.result`, `.raises` | the child | old ⊆ new | collect |

**A value someone else submits may only widen**; **a value we produce may only narrow**.
`output` is the published type — `output_schema` where declared.

**Removal and addition are not mirrors.** Adding a process output is free; removing one breaks
every reader and is reported directly, there being no new schema to compare. Adding an input
schema breaks both checks; removing one is free. Dropping a result schema is free (we conform
less); adding one breaks (§2c).

**A verdict only where a conform stands between the parties.** A fetch request (`url`, `method`,
`headers`, `body`) goes to a service whose tolerance is unknowable — judging it would make every
URL edit breaking — so it is a changed slot; `external.input` likewise. The relation is
**strict** here: a real conform rejects an absent required key whatever its type.

### 3b. The same input schema, asked twice

Upgrade reads the stored input, which is never conformed again; the contract is what
`ValidateInput` does to the next caller. A property gaining `required` while carrying a default
is therefore **upgradable and contract-breaking at once**.

## 4. The order: validate the new side, then upgrade, then contract

A **submitted** document is validated first, alone and against its children — the pass
`POST /definitions/validate` runs — so one that does not type-check is refused with that error,
naming the task and expression, rather than reported unanalysable. **A stored version is never
re-validated**: it passed under the rules of its day, and a version whose own inference now
fails is a per-version `unanalysable` row.

## 5. Gating: the upgrade is not negotiable

Exit 1 if the upgrade check fails or any row is `unanalysable`. `--ignore contract` excuses the
contract check; it is the only token, and any other is refused rather than ignored.

**`unanalysable` cannot be excused**: excluding the absence of a verdict yields an answer
indistinguishable from "checked, and fine".

**A selection moves the exit code and nothing else** — an excused break is still printed,
marked. **`--json` moves nothing**: the flag a pipeline uses to capture findings must not stop
failing on them.

## 6. The report

The fixtures assert the whole rendered report, so the rendering is the deliverable:

    mixedign_proc  v1 → v2  breaking: upgrade; ignored: contract
      charge:fetch.result  (ignored: contract)
        fee: number → string
      settle               (breaking: upgrade)
        outputs.charge.fee: number → string

    exit 1

**Three levels**: the process and its versions; **the schema that was compared** (§6a); what
`isSubset` said about it, as a path into that schema.

**A process appears once, its verdict heading the findings it derives from**, and an exclusion
is stated where it applies — the process line where a member was excused, the row where a
finding under it was. No summary table and no trailing line, which could name neither process
nor address.

**A verdict is grouped by fate, not by member** (`breaking: upgrade, contract`): `,` joins
members, `; ` joins fates, problems first — so a colon after the versions is the whole scan.
**Every member is named on the process line**, a passing one by its own word (`upgradable`,
`compatible`), never by absence; a row says only what happened at its address. A status
(`unchanged`, `new`, `unanalysable`) stands alone, being a property of the process. **The arrow
appears only where two versions were compared.**

### 6a. Addressing

**Level two is the schema compared**; a break row is addressed by it:

    input                        the input schema — both checks (§3b)
    output                       the process output
    <task>                       the context at that task (upgrade); also a removed task
    <task>:<action_type>.result  a result schema; a child_map's is <task>:child_map.<key>.result
    <task>:<action_type>.raises  a raises table, path starting with the code; per key on a child_map
    <task>:action.type           a type change under a held instance (upgrade)
    <task>:child_map.<key>       an added key (upgrade)

Level three is a path into that schema and nothing else: a context's paths start at its roots
(`outputs.charge.fee`), a result schema's do not (`fee`).

**Change rows are addressed by slot, and meet break rows exactly where a slot is a compared
schema.** Every field of the document has an address, and `TestChangedSlots_EveryDifferentDocumentIsReported`
holds that with the marshalled document as oracle:

    config_schema, $defs          nothing judges them (§6b)
    output_schema                 the process's: contract
    tasks                         the task list's ORDER — `switch: next` routes by position
    <task>:<slot>                 output, output_schema (upgrade); switch, on_error, only_once
    <task>:<action_type>.<slot>   fetch.url, child_list.over, child.name, …
    <task>:action.type            the discriminator keeps the generic name
    <task>:child_map.<key>[.slot] a call's existence, then its slots one level down

`tasks` compares only the tasks both sides carry, so an insertion is not reported twice. A
`child_map`'s `children` is decomposed per key so its rows meet the per-key break addresses.
Action slots are addressed by the action type, since the type names the vocabulary (`url` only
on a fetch); a task whose type changed is addressed by the **old** side, the one an instance is
parked under.

**An upgrade break is reported once, at the first task that sees it** — a choice about noise;
the path already names the value's origin. **No break is addressed at `$defs`**: `Normalize`
bakes a definition into every schema that references it, so a break reports there under a
navigable path (`user.age`, not `$defs.User.age`); the `$defs` slot row covers a definition
nobody references.

### 6b. What is a row

A row is a **slot that changed** or a **value that broke**, never both: one line for both would
claim the edit caused the break, which no comparison can know.

**A changed slot gets a row only when nothing broke at its address** — otherwise the break is
the report that it moved:

- **`(breaking: <members>)` / `(ignored: <members>)`** — the process line's grammar, so one
  difference failing both checks prints once, named for both.
- **`(ok)`** — changed, covered, nothing broke **at this address**. Not a claim that the change
  is harmless.
- **`(not judged)`** — changed and no check covers it: a URL, `only_once`, a `switch`.
- **`(added)`** — a task the new version introduces. Never gates.

Which a slot can take follows from what it bears on: `input` → both; the process `output` and
`output_schema` → contract; a task's `output` and `output_schema` → upgrade; `.result` and
`.raises` → contract, plus upgrade where the task parks (a `child_map` key always does);
everything else → nothing.

**`config_schema` carries no verdict and still gets a row.** Validation type-checks every
expression against the new config schema, which covers it better than compat could; but a slot
missing from the report entirely is how a dropped `secret: true` was once reported nowhere
(`shapes/a-dropped-secret-is-reported-not-judged.yaml`).

### 6c. Rules the rendering keeps

- **No check may look unanswered** (§6).
- **Both verdicts are derived from the issues**, never tracked beside them — a separately
  maintained one printed `upgradable` beside `exit 1`.
- **One difference failing both checks prints once**, but is two findings on the wire, because
  they gate separately.

Ordering is deterministic or the fixtures churn: **findings first, then changes no finding
accounts for**; within each, the input, then tasks in the **old** version's order, then the
output; then added tasks.

### 6d. On the wire

**A finding arrives addressed** (`member`, `address`, `task`, `path`, `message`, `gating`), and
the CLI parses no prose — a bracket-quoted key may contain a space. `address` and `path` answer
different levels. A changed slot carries `affects`: empty renders `(not judged)`, non-empty
`(ok)`.

**§6b's suppression happens before the wire** (`accountedFor` in `internal/validation`), so
`changed` holds only slots no issue accounts for and every consumer reads the same report. A
verdict is `{compatible}` alone. **One difference is one issue**: the relation keeps walking
when explaining, so a schema with three breaks yields three — one per run would mean one
release per difference.

`ignore` is a request field. `compatible` is the conjunction over everything compared, ignoring
nothing; **`passes` is the gated answer**, and the two disagreeing is the intended reading of a
green run with an excused break (§8).

## 7. Where it lives

- `internal/schema` — the stored mode on `IsSubset` (§2e): a mode reaches every depth, where an
  operand transform reaches only the top.
- `internal/validation/compat.go` — both checks; `changedslots.go` — the slot vocabulary. Three
  explainer configurations, and **`swap` is the trap**:
  [internal/validation/CLAUDE.md](../internal/validation/CLAUDE.md).
- `internal/api/handlers_compat.go` — side resolution (channels, pins, closure, missing
  counterparts: [internal/api/CLAUDE.md](../internal/api/CLAUDE.md)) and `ignore`.
- `cmd/genctl/compat.go` — flags to selectors, and the rendering; it holds no rule of its own.

## 8. Open

- Does an `external.input` change deserve a verdict? The worker is usually code the same
  operator owns, an argument the fetch case cannot make.
- `SetReport.Compatible` keeps its meaning — the conjunction over everything compared —
  while the gated verdict is separate. Recorded because the two can disagree (green exit,
  `compatible: false`), which is intended.

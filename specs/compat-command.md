# Compat: two checks over one comparison

`genctl compat` answers two questions and folds them into one word. That fold answers the
wrong question. This is the whole of the check: what each half compares and in which
direction, how a finding is addressed, and what an operator may excuse.

Its other half is [version-compatibility.md](version-compatibility.md) — **moving** an
instance once this says it may. The division is sharp: the check reads two documents and
never an instance, so it must assume every reachable state; the gate has the row in hand and
may accept what the check calls different.

## 0. Status

**Proposal.** One piece is built: the token lexer, `internal/selector`
([compat-selection.md](compat-selection.md)). An implementation of the rest was written and
rolled back; findings marked **[run]** came from running it, and three of them contradicted
this document as it then stood.

**Order of work.** The two checks and the report land together (§2–§6): they share one
comparison, and every fixture moves once rather than twice. Granular selection is deferred
indefinitely (compat-selection.md), and §2f's pruning is rejected outright.

## 1. Two checks

- **Upgrade** — can an instance running the old version continue under the new one? A
  question about rows this deployment already owns. **Non-negotiable** (§5).
- **Contract** — does the process still honour what the outside world was written against?
  A question about parties outside the deployment. **Excusable.**

They disagree in both directions, so folding them costs accuracy both ways. Two shipped
fixtures report the wrong thing because of it: `nullable-input-added` reads **upgradable**
though a caller omitting the new property is now rejected at creation, and
`required-added-to-a-defaulted-property` reads **breaking** though every stored input carries
the key.

**Children are not a third check.** A bundle is checked against itself by the registration
preflight, so upgradability stays per-process. A child's own `output` is still a contract —
its consumers include parents outside the bundle.

## 2. Upgrade: every state the old version can persist must fit the new one

### 2a. What is compared

Persisted state is `input`, `outputs.<id>` and `error`; `config` is stripped, being
re-resolved every tick. `contextSchema` folds the must/may dataflow into one object per task,
so the check at a task is `ctxOld(T) ⊆ ctxNew(T)`.

**One context per task is enough for the whole remaining run.** Output types are
position-independent (every `outputs.<id>` resolves through `$defs[<id>_output]`) and the
must-analysis is monotone along a path, so what holds at T covers everything reachable from
it. Checking a *different* task is wrong, not merely less precise.

### 2b. The task set: removal breaks, addition does not

**Every task the old version has, the new version must have.** An instance sitting on one the
new version dropped has nowhere to continue — a set difference, checked directly, since no
schema relation describes it.

**Adding a task is not a second rule.** A task on a branch makes its output merely possible,
so the context marks it nullable and §2d's tolerance closes the gap — "you may add an
optional task" is that tolerance working, not an exception. A task on the **main line** makes
`outputs.<new>` guaranteed, and a row that never passed through it cannot satisfy that: it is
a break, and it stays one even where nothing downstream reads the output. §2f records why the
refinement that would fix the second case is not available.

### 2c. Parked mid-task: external and children

**Whether an instance can be sitting INSIDE a task is the question two rules turn on**, and
neither is about what the action resembles.

An action whose task can hold a **parked** instance carries state the entry context does not,
so its result schema is part of the upgrade check:

- **`external`** — the instance parks; a submitted result is persisted, and one still to come
  is conformed against the schema the instance runs *now*.
- **`child`, `child_map`, `child_list`** — the parent parks in `waiting`/`collecting`, and
  collect conforms each child's output against the parent's result schema **as it currently
  stands** (version-compatibility.md §3a). A narrowing here strands a parent already waiting.

**`fetch` is the one that genuinely is not**, which is why this is a rule about parking
rather than a list: request and response happen inside one advance with nothing persisted
between, so no instance can be holding a fetch result when the version changes.

The cross-document half — a child moving without its parent — is version-compatibility.md
§3b's pairing check, also unbuilt.

### 2d. The relation may be relaxed, because the gap is closable

The upgrade side is one half of a pair: `IsSubsetAsStored` decides a gap is closable and
`Validate(data, ConformToSchemaExactly)` closes it. They must accept exactly the same gaps —
a relation tolerating more promises an upgrade that then fails, a conform closing more is
dead code.

**The gap is the null-versus-missing distinction, and a version change opens it both ways**,
so the conform reconciles in both directions:

| the row holds | the new schema says | reconciliation |
|---|---|---|
| nothing | required, admits null | the null is written in |
| a null | optional, will not take null | the key is removed |

The second row is valid only because absence is valid there. A **required** non-nullable
property whose stored value is null can be neither kept nor removed, so the relation keeps
refusing it — there is no reconciliation to promise. An earlier draft had the conform only
ADD, which left the second row as a gap the relation had to refuse for want of a migration.

**The conform never fills a default**, and that is not symmetry with the above. A default
filled at creation is filled before anything reads it, so every `outputs.<id>` derived from it
agrees with it; filling one into a half-run instance does not, because the values computed
while it was absent are already written and nothing recomputes them. **So a value is only ever
present at upgrade because it was already there.**

**The conform is total, and that is load-bearing rather than tidy.** It runs over the whole
context, not the part something reads, because the next comparison assumes this row conforms
to the version it now runs (§2f). A partial reconciliation is how that premise gets
falsified.

### 2e. One schema, two sets: before the conform and after it

**[run]** A property that gains `required` while carrying a default is upgradable, and the
relaxed relation alone does not say so — a defaulted `integer` admits no null. What makes the
row safe is that **absence is impossible**.

A schema denotes two different sets depending on when it is read:

- **as an acceptance predicate** — what may arrive. A defaulted optional property may be
  absent, and `conformObject` rejects an absent *required* one before it looks for a default,
  so a default on a required property is inert.
- **as a description of conformed data** — what is stored. The same property is always
  present, because the conform filled it and nothing re-validates it.

The contract check compares acceptance predicates; the upgrade check compares conformed data.
Same schemas, same direction, two meanings — that is the whole of why the checks disagree
here, and it is **not** the input/output distinction, which decides direction (§3a) only.

**Both sides take it as a mode, and the modes must stay in step.** `Validate` already
distinguishes the two cases — `Strict` at creation fills defaults, `ConformToSchemaExactly` at
upgrade does not — and `IsSubset` gains the matching one: under the after-conform view,
*guaranteed present* means **required or carrying a default**, at every depth. The schema
package already applies that rule to reads (`lookupProperty` returns a non-nullable type for
a property that is required *or* defaulted), so the relation is adopting its own navigation's
rule. Nothing in `conformObject` changes.

**The rule reads the sub side, and only the sub side.** A default on the *old* schema means
the value is in the row, because creation put it there. A default on the *new* schema means
nothing here, twice over: the fill writes none, and the row was never conformed under the new
schema anyway — it was conformed under the old one and is being carried over, so what the new
schema demands of it is its `required` set and nothing more.

**[run]** Reading it symmetrically — treating a default on super as a guarantee super
*requires* — was implemented and was wrong. It reported a break at `input` for an edit that
adds a default, where the stored row is in fact still valid, and the explainer could not
articulate it: the message came out `object → object`. The real consequence of adding a
default is that every *read* becomes non-null, and that surfaces where it is read, in the
inferred context (`outputs.plan.retries: integer|null → integer`). Two addresses, one edit,
and only one of them is a break.

That asymmetry is also what distinguishes this from the relaxation
[internal/schema/CLAUDE.md](../internal/schema/CLAUDE.md) declines: there, only the super
side carries the default and a fill would have to write it.

### 2f. Rejected: requiring only what is read

**[run]** The context at T guarantees everything the new definition produces on the way,
including values nothing from T onward reads. Requiring them is why adding a main-line task
reads as breaking even when its output is dead. The obvious refinement — prune `mustNew(T)`
to what is actually referenced, by backward reachability over `shape.Roots()` — was designed,
required here, built, and **rejected**. It is unsound, and the way it fails is worth the
space because it looks monotone.

**Every upgrade must leave the row conforming to the new version's schema in full.** The
comparison only ever sees two adjacent versions, and it reasons from the premise that the old
side's DATA satisfies the old side's SCHEMA — version-compatibility.md §1 states it as an
assumption that registration establishes. Pruning falsifies it: the values nothing read are
left unreconciled, so the row no longer conforms to the version it now runs.

Nothing notices at that hop. It surfaces at the next one:

    v1  outputs.a.x : number      nothing after T reads outputs.a
    v2  outputs.a.x : string      pruned → "upgradable", and the row keeps its number
    v3  outputs.a.x : string      v2 ⊆ v3 → compatible, and it is — about the SCHEMAS

The v2→v3 comparison is correct on its own terms and still wrong about this instance,
because v1→v2 quietly broke the premise it rests on. A number surfaces where the type says
string, two versions after the report that allowed it, with no run ever having reported
anything.

So the imprecision stays, and it is the honest answer: **adding a task on the main line whose
output is guaranteed is a break**, whether or not anything downstream reads it today. A
refinement that skips reconciliation is not a refinement. §2b says the same thing from the
other end.

## 3. Contract: the outside world

### 3a. Direction is decided by who submits the value

| address | submitter | relation | the conform behind it |
|---|---|---|---|
| `input` | caller | old ⊆ new | `ValidateInput` at creation |
| `output` | us | new ⊆ old | a waiting parent's result schema at collect |
| `<task>:fetch.result` | the service | old ⊆ new | collect |
| `<task>:external.result` | the worker | old ⊆ new | submit |
| `<task>:child*.result` | the child | old ⊆ new | collect |

**A value someone else submits may only widen**: adding a required field breaks them,
dropping one does not. **A value we produce may only narrow**: removing something previously
guaranteed breaks a reader written against it, adding is free.

**Removal and addition are not mirrors, and which is free depends on the direction.**
**[run]** Adding a process output is free — no consumer can have been written against a
value that was never produced — while removing one breaks every reader and is not even a
schema comparison: there is no new schema to compare against, so it is reported directly, the
way a removed task is. Adding an *input* schema breaks both questions at once (a caller
sending nothing is rejected, and a row created before holds no input); removing one is free,
since we then demand nothing. Dropping a result schema is free for the same reason — we
conform less. The one that reads backwards was silent for a release: `compareOutput` skipped
whenever *either* side declared no output, which is right for the addition and wrong for the
removal.

**A verdict only where a conform stands between the two parties.** Everything else is a
changed slot: a fetch request (`url`, `method`, `headers`, `body`) goes to a service whose
tolerance is unknowable, and judging it would make every URL edit breaking — which
`shapes/url-changed.yaml` exists to refuse. `external.input` is the same case.

The relation is **strict** here: a conform runs for real and rejects an absent required key
whatever its type.

### 3b. The same input schema, asked twice

Upgradability reads the stored input and never conforms it again; the contract is what
`ValidateInput` will do to the next caller. A property gaining `required` while carrying a
default is therefore **upgradable and contract-breaking at once** — the row has the value,
the caller must now send it. Neither statement is available from one verdict.

## 4. The order: validate the new side, then upgrade, then contract

1. **Validate** the target definition, on its own and against its children — the same pass
   `POST /definitions/validate` runs. A document that does not parse, whose expressions do not
   type-check, or whose child refs do not resolve is refused with that error, naming the task
   and the expression, rather than compared and reported as unanalysable.
2. **Upgrade** (§2). 3. **Contract** (§3).

**The old side is taken as valid and never re-validated.** It passed under the rules of its
day; re-checking it would fail a run about two *other* versions. A version whose own
inference fails is a per-version `unanalysable` row.

## 5. Gating: the upgrade is not negotiable

Exit 1 if the upgrade check fails, or if any row is `unanalysable`. The contract check can be
excused with one flag: `genctl compat --from a --to b --ignore contract`.

**`unanalysable` cannot be excused.** It is the absence of a verdict, and excluding it
produces the answer indistinguishable from "checked, and fine" that the status exists to
prevent.

**A selection moves the exit code and nothing else.** An excused break still appears, marked,
and a trailing line names what was excluded and why the exit is 0.

Per-member and per-path selection is designed and deferred
([compat-selection.md](compat-selection.md)); `contract` is already a token in that grammar,
so this flag is its general form restricted to one value.

## 6. The report

The fixtures assert the whole rendered report, so the rendering IS the deliverable and has to
be decided here rather than discovered while regenerating them.

    PROCESS     UPGRADE          CONTRACT
    order_proc  breaking         breaking (ignored)
    child_proc  nothing changed  nothing changed

    order_proc v1 → v2
      input                        (breaking contract)
        retries: newly required field
      settle                       (breaking upgrade)
        outputs.charge.fee: number → string
      charge:fetch.result          (ok)
      charge:fetch.url             (not judged)

    not gating: contract — --ignore contract
    exit 1

**Three levels, three questions.** The process and its two versions; **the schema that was
compared**; then **what `isSubset` said about it**, as a path into that schema. The run was
`--ignore contract` and still exits 1, because the upgrade break is not excusable (§5) — the
contract column keeps its annotation anyway, since the flag did what it was asked.

### 6a. Addressing

**Level two is the schema that was compared**, and saying so answers the slot-versus-value
question. There are only four:

    input                        the input schema — both checks read it (§3b)
    output                       the process output schema
    <task>                       the context at that task (upgrade)
    <task>:<action_type>.result  a result schema — fetch.result, external.result, child.result

Level three is a path **into that schema**, relative to nothing else: a context's paths start
at its own roots and read `outputs.charge.fee` in full; a result schema's read `fee`.

**Change rows are addressed by slot, break rows by compared schema, and they overlap exactly
where a slot IS one.** A slot no check looks at can only ever be a change:

    input, output, <task>:<action_type>.result   a slot and a compared schema
    <task>:<slot>                 a slot only — output, switch, on_error, timeout, only_once
    <task>:<action_type>.<slot>   a slot only — fetch.url, fetch.headers, child.name
    <task>                        a compared schema, and the task's existence (added/removed)

An earlier draft split these into two vocabularies (`input_schema` for what an author edits,
`input` for what a row holds). The distinction does not survive a static check: no value is
ever in hand, so both are the same schema at the same place. **The `_schema` suffix is
dropped everywhere.**

**An action's slots are addressed by the action type, not by the word `action`.** They are
polymorphic — `url` only on a fetch, `name` only on a child, `for` only on a delay — so the
type says which vocabulary the slot name comes from; `action.url` said nothing an address
needs. Slots every task has need no prefix, because they mean the same thing on all of them.

**An upgrade break is addressed at the task whose context failed**, and reported once, at the
first task that sees it. That is a choice about noise, not a claim: the earliest task an
instance can be parked at and break is the useful one to name, and the value's origin is
already in the path.

**`$defs` is never an address, and a path must never reach one. [run]** A shared definition
is a pool: no instance sits at it, no caller is pointed at it. `Normalize` bakes it into
every schema that references it, so the edit reports at each of those under a path an
operator can navigate — `user.age`, not `$defs.User.age` — and a definition nobody
references is silent, which is correct.

The process name is stripped, the block header having named it. A task whose action type
changed is addressed by the **old** side; version-compatibility.md §2 refuses an action-type
change on a parked task, so nothing depends on resolving that.

### 6b. What is a row

**[run]** A row is either a **slot that changed** or a **value that broke**, never both.
Putting them on one line claims the edit caused the break, which no comparison can know: given
a task that gained a required `extra` in both its result schema and its `output`, the result
schema edit alone breaks nothing and the `output` edit alone does not even validate.

**A changed slot gets a row only when nothing broke under it.** Otherwise the break *is* the
report that the slot moved, and a second row at that address saying it is fine would
contradict the first. A slot row is what is left over — a difference the checks passed, or
one they never covered:

- **`(breaking upgrade)` / `(breaking contract)` / `(breaking upgrade and contract)`** — the
  last where one difference fails both questions, printed once.
- **`(ok)`** — it changed, a check covers it, nothing broke **at this address**. It does not
  say the change is harmless: a break it causes may be reported elsewhere, and no comparison
  can prove the connection either way.
- **`(not judged)`** — it changed and **no check covers it**. A URL repointed, an `only_once`
  flipped, a `switch` rerouted. This is what the changed-slot channel exists for.
- **`(added)`** — a task the new version introduces. Never gates.

Which phrase a slot can take follows from the question it bears on: `input` → both;
the process `output` → contract; a task's `output` → upgrade; `<action_type>.result` →
contract, and upgrade where the task can park (§2c); everything else → nothing, which is what
`(not judged)` renders.

**`config_schema` is not a slot here at all** — no verdict, no row, and not part of the
document comparison. It is a runtime check that the environment is set. The objection is that
config reaches the data through `config.x`, and **validation covers that better**: step 1
type-checks every expression against the new config schema, so removing a field something
reads is refused there, naming the task. Two costs, accepted: a `secret: true` dropped from
config is reported nowhere (`shapes/accepted-hazard-secret-dropped.yaml` goes with this), and
a config-only edit reads `nothing changed` — true of everything compat judges, false of the
document.

### 6c. Rules learned by getting them wrong

**[run]** Each shipped in the rolled-back implementation and was reported as a bug:

- **A row with no verdicts repeats its status in both columns.** Spanning one cell and
  leaving the other blank reads as *this question went unanswered*.
- **Both verdicts are derived from the issues**, never tracked beside them. Maintained
  separately, a row printed `upgradable` next to `exit 1`.
- **One difference failing both questions prints once**, with both named. It is two findings
  on the wire, because they gate separately.
- **A clean line says `(ok)`, scoped to itself.** A wider phrase denies what another line
  asserts.

Ordering must be deterministic or the fixtures churn. **Findings first, then the changes no
finding accounts for** — problems before context, which is also what makes the suppression
rule above readable. Within each, the process's shape: the input, then tasks as the **old**
version ordered them, then the output; then the added tasks.

### 6d. On the wire

`genctl` reconstructs a finding by peeling the path off a reason string at the first space
(`splitReason`). Nothing can key off that — a bracket-quoted key may contain a space. **A
finding must arrive addressed**, and the CLI parses no prose:

    {"name":"p","status":"compared","from":1,"to":2,
     "upgrade":  {"compatible":false},
     "contract": {"compatible":true},
     "changed":[{"address":"charge:fetch.url","task":"charge","affects":[]}],
     "added":["audit"],
     "issues":[{"member":"upgrade","address":"settle","task":"settle",
                "path":"outputs.charge.fee","message":"number → string","gating":true}]}

`address` and `path` are separate because they answer different levels. An empty `affects`
renders `(not judged)`, a non-empty one `(ok)`, and the entry is suppressed entirely when an
issue shares its address (§6b). A verdict carries no issues — it is `{compatible}` alone,
derived from them. `compatible` / `output_compatible` and every per-task nesting disappear.

The flag is a request field, so `--json` and the report answer the same question:

    {"from":…,"to":…,"ignore":["contract"]}   →   {"compatible":false,"passes":true,…}

`compatible` is the conjunction over everything compared, ignoring nothing; **`passes` is the
gated answer**, and the two disagreeing is the intended reading of a green run with a break
in it (§8). `openapi.json` and the TypeScript client are generated, not committed, so a shape
change lands in no diff — the typecheck in `test-int` is what notices.

## 7. Where it lives

- `internal/schema` — the after-conform mode on `IsSubset` (§2e), beside `absentAsNull`. It
  belongs here because `lookupProperty` already applies the same rule to reads, and because a
  mode reaches every depth while an operand transform reaches only the top.
- `internal/validation/compat.go` — the two checks. **Three explainer configurations, and the
  `swap` flag is the trap**: upgrade is `{absentAsNull: true}` plus the after-conform mode;
  the input contract is `{}` — strict, no swap, since it already runs old ⊆ new; the output
  contract is `{swap: true}`, running new ⊆ old while the reader asks what *they* changed.
- `cmd/genctl/commands.go` — two columns, three levels, the not-gating line. `splitReason`
  and `slotFor` are deleted rather than adapted.
- `internal/api/handlers_compat.go` — the shape it marshals, and the flag as a request field.

## 8. Open

- Does an `external.input` change deserve a verdict? The worker is usually code the same
  operator owns, an argument the fetch case cannot make.
- `SetReport.Compatible` keeps its meaning — the conjunction over everything compared —
  while the gated verdict is separate. Recorded because the two can disagree (green exit,
  `compatible: false`), which is intended.

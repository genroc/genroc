# Compat: two checks over one comparison

Refines [version-compatibility.md](version-compatibility.md), whose §3a/§3b report two
verdicts and leave `genctl compat` to fold them into one word. That fold answers the wrong
question. This records the two checks, what each compares and in which direction, how a
finding is addressed, and what an operator may excuse.

## 0. Status

**Proposal.** One piece is built: the token lexer, `internal/selector` (§8).

An implementation of everything else was built and rolled back. It is worth knowing that it
existed, because the findings below marked **[run]** came from running it rather than from
argument, and they are the ones most likely to be re-litigated by someone who has only read
the design. Three of them contradicted this document as it then stood.

## 1. Two checks

- **Upgrade** — can an instance running the old version continue under the new one? A
  question about rows this deployment already owns. **It is non-negotiable** (§5).
- **Contract** — does the process still honour what the outside world was written against?
  A question about parties that are not in the deployment. **It can be excused.**

They are independent and disagree in both directions, so folding them costs accuracy both
ways. Two shipped fixtures report the wrong thing because of the fold, and both are the same
miscategorisation:

- `shapes/nullable-input-added.yaml` reads **upgradable**, though a caller that omits the
  new property is now rejected at creation. Right about rows, silent about callers.
- `shapes/required-added-to-a-defaulted-property.yaml` reads **breaking**, though every
  stored input carries the key — creation conformed it once and persisted the filled
  default. Right about callers, wrong about rows, and pinned as a false alarm for exactly
  this reason.

**Children are deliberately not a third check.** A bundle is checked against itself by the
registration preflight, so upgradability stays a per-process question. A child's own
`output` is still a contract — its consumers include parents outside the bundle.

## 2. Upgrade: every state the old version can persist must fit the new one

### 2a. What is persisted

`input`, `outputs.<id>`, and `error`. `config` is stripped — it is re-resolved from the
environment on every tick, so nothing persisted corresponds to it. `contextSchema` folds the
must/may dataflow into one object per task, so the check at a task is
`ctxOld(T) ⊆ ctxNew(T)`: every context the old definition can present there, the new one
accepts. One context per task is enough for the whole remaining run — a task output's type
is position-independent and the must-analysis is monotone along a path
(version-compatibility.md §2).

### 2b. The task set: removal breaks, addition does not

**Every task the old version has, the new version must have.** An instance sitting on one
the new version dropped has nowhere to continue, and no schema relation describes that — it
is a set difference, checked directly.

**Adding a task is not a second rule.** Whether it is upgradable falls out of §2a: a task
added on the main line makes `outputs.<new>` *guaranteed* in every later context, so it is
required and non-nullable there, and a row that never passed through it cannot satisfy that.
A task added on a branch makes its output merely *possible*, so the context marks it
nullable and the relaxed relation closes the gap. "You may add an optional task" is that
tolerance doing its job, not an exception to anything.

### 2c. Parked mid-task: external and children

An action whose task can hold a parked instance carries state the entry context does not,
and its `result_schema` is therefore part of the upgrade check:

- **`external`** — the instance parks and the result is submitted later, then read back as
  `self.result`. A result already submitted is persisted; one still to come is conformed
  against the schema the instance runs **now**, which is the new one.
- **`child`, `child_map`, `child_list`** — the parent parks in `waiting`/`collecting` while
  children run, and collect conforms each child's output against the parent's
  `result_schema` **as it currently stands**. That is what removing `_spawn_result_schema`
  established (version-compatibility.md §5a), and it is why children are not a special case
  of the contract check: a narrowing here strands a parent that is already waiting.

The cross-document half of this — a child moving without its parent, or the reverse — is
version-compatibility.md §5b's pairing check, also unbuilt. §2c is the single-process half:
it asks whether *this* process's `result_schema` still fits what a parked instance will be
handed, and says nothing about which child version hands it over.

**`fetch` is the one that genuinely is not**, and it is the reason this is a rule about
parking rather than a list: a request and its response happen inside one advance, with
nothing persisted between them, so no instance can be holding a `fetch` result when the
version changes. A `fetch.result_schema` is a contract and nothing else (§3a).

### 2d. The relation may be relaxed, because the gap is closable

The upgrade side uses `IsSubsetAbsentAsNull`, which tolerates the new version requiring a
property the old one did not **when that property's type admits null**. The justification is
not that reads are forgiving — it is that `Validate(data, FillAbsentAsNull)` closes exactly
that gap by writing the null in, and an upgrade runs the stored state through it. The two
are a pair and must accept exactly the same gaps; a relation that tolerates more than the
fill can close promises an upgrade that then fails to conform.

**The fill writes nulls and never defaults**, and the reason is not caution — it is that a
default filled at upgrade would be *inconsistent with the row around it*. A default filled at
creation is filled before anything reads it, so every `outputs.<id>` computed from it agrees
with it. Filling one into an instance that is already half-run does not: the stored values
that would have been derived from that field were computed while it was absent, and nothing
recomputes them. The fill would have to re-run the process to be correct, which is not a
migration. **So a value is only ever present at upgrade because it was already there.**

**The fill only ever adds**, for the same family of reasons: a downgrade leaves a stale
`x: null` the old version ignores, and an upgrade that deletes a value nobody asked it to
delete is a worse failure than a refused one.

One further gap is closable in principle and is **not** closed today. If the new version
declares a property **optional and not nullable**, a row holding `x: null` — written by an
earlier upgrade's fill — no longer conforms, because null is not one of its allowed values.
Deleting the key would fix it, since the property is optional. The fill cannot delete, so the
relation must keep refusing that pair; the two move together or not at all
(internal/schema/CLAUDE.md).

### 2e. One schema, two sets: before the conform and after it

**[run]** A property that gains `required` while carrying a default is upgradable, and the
relaxed relation alone does not say so: `IsSubsetAbsentAsNull` tolerates a newly required
property only where its type admits null, and a defaulted `integer` does not. What makes the
row safe is not that absence reads as null — it is that **absence is impossible**.

The general statement is that a schema denotes two different sets, depending on when it is
read:

- **as an acceptance predicate** — what may legitimately arrive. A defaulted optional
  property may be absent, and `conformObject` rejects an absent *required* one before it
  ever looks for a default (`validate.go`), so a default on a required property is inert.
- **as a description of conformed data** — what is stored and read back. The same property
  is always present, because the conform filled it and nothing re-validates it after.

The contract check compares acceptance predicates: what a caller may send. The upgrade check
compares conformed data: what a row holds. Same schemas, same direction, two meanings — and
that is the whole of why the two checks disagree here. It is **not** the input/output
distinction, which decides direction (§3a) and nothing else.

**So it belongs in the relation, as a mode.** Under the after-conform view, *guaranteed
present* means **required OR carrying a default**, at every depth. `IsSubset` already takes
one mode for this view (`absentAsNull`); this is the second. It is not a new idea in the
schema package either — `lookupProperty` already returns a non-nullable type for a property
that is required *or* defaulted, so the relation would be adopting the rule its own
navigation applies.

**The rule reads the sub side, and must never be read as "we will fill it".** A default on
the *old* schema means the value is in the row, because creation put it there (§2d). A
default on the *new* schema means nothing for an upgrade: the fill does not write defaults,
and cannot, so a property the row lacks stays lacking however the new version declares it.
That asymmetry is the whole of the rule's soundness, and it is what distinguishes this from
the relaxation internal/schema/CLAUDE.md declines.

A mode rather than a transform on the operand matters for more than tidiness: a nested
default is filled by the conform too, so a promotion applied to the top level only reports a
break that is really closable.

**Why this needs no fill, unlike every other relaxation.** The pairing rule in
[internal/schema/CLAUDE.md](../internal/schema/CLAUDE.md) is that a relation must tolerate
only gaps `Validate(v, FillAbsentAsNull)` can close, and it explicitly declines the
required-with-default case for that reason. This is not that case. The rule above tolerates a
gap only where the **sub** side already guarantees the value — the data has it, so there is
nothing to repair. The case CLAUDE.md means is the other one, where only the **super** side
carries the default and a fill would have to write it; the rule must keep refusing that, and
does.

## 3. Contract: the outside world

### 3a. Direction is decided by who submits the value

| slot | submitter | relation | the conform behind it |
|---|---|---|---|
| process `input` | caller | old ⊆ new | `ValidateInput` at creation |
| process `output` | us | new ⊆ old | a waiting parent's `result_schema` at collect |
| `fetch.result_schema` | the service | old ⊆ new | collect |
| `external.result_schema` | the worker | old ⊆ new | submit |
| `child*.result_schema` | the child | old ⊆ new | collect |

**A value someone else submits may only widen**: adding a required field breaks them,
dropping one does not — we accept less than we did, and they were already sending it. **A
value we produce may only narrow**: removing something previously guaranteed breaks a reader
written against it, adding is free. The two directions are the whole of the contract check.

**A verdict only where a conform stands between the two parties.** Everything else is a
changed slot (§6b): a `fetch` request (`url`, `method`, `headers`, `body`) is something we
send into a service whose tolerance is unknowable, and making it a verdict would turn every
URL edit breaking — which `shapes/url-changed.yaml` exists to refuse. `external.input` is
the same case; we send it, and the worker's tolerance is its own business.

The relation here is **strict**: a conform runs for real on this path, and it rejects an
absent required key whatever its type. Relaxing would promise what the runtime refuses.

### 3b. The same input schema, asked twice

This is the case that motivates the whole split, and §2e is the half that is easy to get
wrong. Upgradability reads the stored input and never conforms it again; the contract is
what `ValidateInput` will do to the next caller's request. A property gaining `required`
while carrying a default is therefore **upgradable and contract-breaking at once** — the row
has the value, the caller must now send it. Both true, and neither statement is available
from one verdict.

## 4. The order: validate the new side, then upgrade, then contract

1. **Validate** the target definition, on its own and against its children — the same pass
   `POST /definitions/validate` runs. A document that does not parse, whose expressions do
   not type-check, or whose child references do not resolve is refused with that error,
   naming the task and the expression, rather than compared and reported as unanalysable.
2. **Upgrade** (§2).
3. **Contract** (§3).

**The old side is taken as valid and is never re-validated.** It passed under the rules of
its day, and a registry accumulates definitions that did. Re-checking it would fail a run
about two *other* versions because some legacy document no longer analyses — which is why a
version whose own inference fails is a per-version `unanalysable` row rather than an error.

## 5. Gating: the upgrade is not negotiable

Exit 1 if the upgrade check fails, or if any row is `unanalysable`. The contract check can
be excused with a single flag:

    genctl compat --from a --to b --ignore contract

**`unanalysable` cannot be excused.** It is not a verdict but the absence of one, and
excluding it produces precisely the answer indistinguishable from "checked, and fine" that
the status exists to prevent.

**A selection moves the exit code and nothing else.** An excused break still appears in the
report, marked, and a trailing line names what was excluded and why the exit is 0. This is
the rule `internal/validation/CLAUDE.md` already states for `nothing_to_compare` — an answer
indistinguishable from "checked, and fine" is worse than no answer — and a selection flag is
exactly the feature that invites breaking it.

Per-member and per-path selection is designed and **deferred** (§8). The grammar is built to
grow into it: `contract` is already a token in that grammar, so `--ignore contract` is the
general form restricted to one value rather than a flag that has to be replaced later.

## 6. The report

The fixtures assert the whole rendered report, so the rendering IS the deliverable and has
to be decided here rather than discovered while regenerating them.

    PROCESS       UPGRADE          CONTRACT
    order_proc    upgradable       breaking (ignored)
    child_proc    nothing changed  nothing changed

    order_proc v1 → v2
      input_schema                 (upgrade, contract)
      input                        (breaking contract)
        retries: newly required field
      charge:fetch.result          (contract)
      charge:action.url            (not judged)

    not gating: contract — --ignore contract
    exit 0

The two `input` lines are the vocabulary of §6a doing its work: the slot bears on **both**
questions, and only one of them broke. Neither line claims the other.

### 6a. Addressing: where in the process, then which property

An address says **where in the process** something is; the path inside it says **which
property**, in the ordinary schema path the rest of the system uses:

**A slot and a value are addressed in different vocabularies**, because they are different
places — one in the definition an author edits, one in the state an instance holds. That is
§6b's rule made spellable: nothing has to be conflated, because nothing collides.

    input_schema                 the caller's contract, and what a row was conformed under
    <task>:<slot>                one slot of a task — action.url, switch, only_once
    <task>:<action_type>.result  what a task expects back — fetch.result, external.result
    <task>                       the task itself: added, or removed
    input / outputs.<task> / error   the stored state, where an upgrade finding lands
    output                       the process output — both a slot and a contract, the one
                                 place the two vocabularies name the same thing

**An upgrade finding is addressed by the value, never by the task that noticed.** The same
difference surfaces at every task that can see it and is deduplicated to the first, so the
noticing task is an artefact of ordering. The producing task is part of the value's own path
and is stable: `outputs.plan.retries` addresses itself, and `plan:output` — the slot — is a
different row saying a different thing.

The process name is **stripped**, because the block header above already names it. A task
whose action type changed between versions is addressed by the **old** side; §4 of
version-compatibility refuses an action-type change on a parked task, so nothing depends on
resolving that ambiguity here.

**`$defs` is never an address, and a path must never reach one.** **[run]** A shared
definition is a pool: no instance sits at it and no caller is pointed at it. `Normalize`
bakes a referenced definition into every schema that uses it, which is what makes the right
answer available — the edit reports at each schema addressing it, under a path an operator
can navigate (`user.age`, not `$defs.User.age`) — and a definition nobody references is
silent, which is correct, since it can affect nothing.

### 6b. What is a row

**[run]** A row is either a **slot that changed** or a **value that broke**, never both.
They are different kinds of address, and putting them on one line claims that the edit
caused the break — which no comparison can know. The evidence is concrete: given a task that
gained a required `extra` in both its `result_schema` and its `output`, the `result_schema`
edit alone breaks nothing, and the `output` edit alone does not even validate. One line
naming both, above a message, asserts a cause that is false for one of them.

A slot row carries **the question that slot bears on**, which is a property of the slot and
not of any comparison:

- `input_schema` → upgrade **and** contract (§3b)
- a task's `output` → upgrade (it shapes the context every later task reads)
- `action.result_schema` → contract, and also upgrade where the task can park (§2c)
- everything else → **nothing**, and saying so is the point. A URL repointed, an
  `only_once` flipped, a `secret` dropped: `isSubset` never inspects them, and this is the
  only channel that reports them at all. They influence no verdict.

**`config_schema` is not a slot here at all** — not a verdict, not a change row, and not part
of the document comparison that decides whether there is anything to compare. It is a runtime
check that the environment is set: not a contract with any party, and not state an instance
holds, since §2a strips `config` from every context.

The tempting objection is that config *does* reach the data — an expression reads `config.x`,
so narrowing `config_schema` can change what a task produces. **Validation already covers
that**, and covers it better: step 1 (§4) type-checks every expression against the new
`config_schema`, so removing a field something reads is refused there, naming the task and
the expression. Compat would only be able to say that a slot moved. What is left after
validation has run is an environment that may or may not be set correctly, which is an
operational question about one deployment rather than a question about two versions.

Two costs, both accepted. A `secret: true` dropped from `config_schema` is now reported
nowhere — `shapes/accepted-hazard-secret-dropped.yaml` goes with this decision; the same drop
in `input_schema` still surfaces as a change to that slot. And a bundle whose only edit is to
`config_schema` reads `nothing changed`, which is true of everything compat judges and false
of the document — the process does get a new version and a different hash. If that ever
misleads anyone, the fix is the word the status renders as, not this list.

### 6c. Rules that were learned by getting them wrong

**[run]** Each of these shipped in the rolled-back implementation and was reported as a bug
by a reader, which is the only reason they are written down:

- **A row with no verdicts repeats its status in both columns.** Letting it span one cell
  and leaving the other blank reads as *this question went unanswered*, not *this question
  does not apply*. A report where every row was `nothing changed` was described as hiding a
  contract problem it did not have.
- **Both verdicts are derived from the issues, never tracked beside them.** Maintained
  separately, a row printed `upgradable` next to `exit 1`, because removed tasks were filed
  without the verdict being told.
- **One difference that fails both questions prints once**, with both named, not once per
  column. It is two findings on the wire, because they gate separately.
- **A clean line says `(no break here)`, not `(no breaking change)`.** The wider phrase
  denies what another line asserts: an edit reported clean at its own address may be exactly
  what broke a value reported two lines down.

Ordering must be deterministic or the fixtures churn: processes as `CompareSet` orders them,
then the definition's own slots, then tasks as the **old** version ordered them, then the
tasks the new version adds. Deduplication is unchanged — one difference in the data is
reported once, not once per task that can see it.

### 6d. On the wire

`genctl` today reconstructs a finding by peeling the path off a reason string at the first
space (`splitReason`). Nothing can key off that — not gating, not correct rendering, since a
bracket-quoted key may contain a space. **A finding must arrive addressed**, and the CLI must
parse no prose at all. Per process:

    {"name":"p","status":"compared","from":1,"to":2,
     "upgrade":  {"compatible":false},
     "contract": {"compatible":true},
     "changed":[{"task":"charge","slot":"action.url","affects":[]}],
     "added":["audit"],
     "issues":[{"member":"upgrade","path":"outputs.plan.retries",
                "message":"integer|null → integer","gating":true}]}

`compatible` / `output_compatible` disappear from the row, and so does every per-task
nesting: a slot that changed, a task that was added and a value that broke are three flat
lists, because they are three kinds of address (§6b). **A verdict carries no issues** — it is
`{compatible}` alone, derived from them (§6c).

The flag is a request field, so `--json` and the rendered report answer the same question and
a CI consumer reads a boolean rather than deriving one:

    {"from":…,"to":…,"ignore":["contract"]}

    {"compatible":false,"passes":true,"processes":[…]}

`compatible` keeps its meaning: the conjunction over everything compared, ignoring nothing.
**`passes` is the gated answer** — the exit code as a boolean — and the two disagreeing is
the intended reading of a green run with a break in it (§9).

The endpoint is generated from the action registry, so a shape change means regenerating
`openapi.json` (`make swagger`) — which is **generated, not committed** (`.gitignore`), as is
the TypeScript client `test-int` builds from it. Nothing here lands in a diff; the typecheck
in `test-int` is what notices.

## 7. Where it lives

- `internal/schema` — the after-conform mode on `IsSubset` (§2e), beside `absentAsNull`.
  It belongs here rather than in the caller because `lookupProperty` already applies the same
  rule to reads, and because a mode reaches every depth while a transform on the operand
  reaches only the top.
- `internal/validation/compat.go` — the two checks. **Three explainer configurations, and
  the `swap` flag is the trap**: the upgrade side is `{absentAsNull: true}` plus the
  after-conform mode; the input contract is `{}` — strict, no swap, because it already runs
  old ⊆ new and the arrow reads old → new; the output contract is `{swap: true}`, because it
  runs new ⊆ old while the reader is asking what *they* changed.
- `internal/selector` — **built**: the token lexer (§8), and nothing else. It knows no
  vocabulary, so the walk over its output lives with the side that does.
- `cmd/genctl/commands.go` — two columns, one row per address, the not-gating line.
  `splitReason` and `slotFor` are deleted rather than adapted: a finding must arrive
  addressed, because a bracket-quoted key may contain a space and gating cannot key off
  prose.
- `internal/api/handlers_compat.go` — the shape it marshals, and the flag as a request
  field so `--json` and the rendered report answer the same question.

## 8. Deferred: selecting what gates, finer than `contract`

§5 ships one flag. The grammar below is the general form it is a restriction of, designed
and not built: it lets an operator name a member, a process, a task or a field, so a build
can gate on part of a contract rather than all of it. It lands when someone needs it; the
lexer is already built, and nothing in §5 has to change when it does.

Everything from here down is that design, unchanged. Read `--check`/`--ignore` as the pair
that generalises `--ignore contract`.

### 8a. The token grammar

**Colons scope, dots navigate.** A token names a member and qualifies it leftward:

    token    := [ <process> ":" [ <task> ":" ] ] <member> [ "." <path> ]
    <member> := upgrade | contract | input | output | fetch | external
    <name>   := a bare name, or "quoted" where it holds a delimiter

Reading a token left to right is reading the report inward: the process it is filed under,
the task the issue names, the member it was grouped by, the path printed on the line.

    output                                  # every process's consumer contract
    order_proc:output                       # one process's
    order_proc:input.retries                # one field of one process's caller contract
    order_proc:charge:fetch.result          # what one task expects back from its service
    order_proc:charge:fetch.result.fee      # one field of that
    order_proc:upgrade.outputs.charge       # everything stored under one task's output
    "odd:name":"a.task":external.result     # names that need quoting
    order_proc:                             # every member of one process
    order_proc:charge:                      # every member of one task

**Dots nest: `fetch.result` covers `fetch.result.fee`.** A path matches as a prefix, step by
step — never as a string, or `outputs.a` would swallow `outputs.ab`. It nests in one direction
only: a token is a prefix of the finding, never the finding a prefix of the token, so
`--ignore order_proc:input.retries` still gates a break reported at `input` itself. A narrow
exclusion cannot silence a broader break that happens to pass through it. **Colons do not
nest**: process and task are exact.

**The lexer is built**: `selector.Lex(s, delims...)` returns the alternating sequence
`name, delim, name, …`, one pass over the whole flag value with `,` `:` `.` read together.
It reads the sequence and judges none of it — every rule above is the caller's, checked
against a vocabulary the lexer does not know.

Three things it settles, each pinned by a test:

- **A quote may only open a name and must close it at a delimiter.** `"a:b":c` is a name, a
  colon and a name; `a"b"c` is refused, because one name with two spellings is something
  every later comparison would have to know about. `\"` and `\\` are the only escapes.
- **One pass, commas included.** Lexing the list first and each token after would refuse
  `"odd,name":charge` — the quoted name ends at a colon the outer pass does not know — and a
  name may hold a comma for exactly the reason it may hold a colon.
- **A name always sits between two delimiters, empty where nothing was written.** That is
  what leaves `order_proc:` visible as a scope with no member, and it makes the odd/even
  positions a contract the caller reads by. An empty name reads empty however it was written,
  so a property literally named `""` is not selectable — the only empty worth a meaning is
  the trailing one.

The syntax is deliberately **not** the expression language's accessor form (§8f).

**A task segment is accepted only where the member has a task dimension** — `fetch` and
`external`. `order_proc:charge:input` is refused rather than quietly ignored: an input
contract belongs to the process, and dropping the segment silently would tell an operator
they had scoped something they had not.

**`upgrade` takes no task segment either**, for a subtler reason. An upgrade finding is
reported at the task whose context broke, but the same difference surfaces at every later
task and is deduplicated to the first — so the task it carries is an artefact of ordering,
not a property of the finding. The path already names the task that owns the value
(`upgrade.outputs.charge.fee`), and that is the stable thing to scope by.

`contract` is a group standing for `input`, `output`, `fetch` and `external`, so it takes no
path of its own.

Flags are repeatable and comma-separated (`--ignore a,b --ignore c`); matching is
case-sensitive throughout, as task and process names are everywhere else.

### 8b. Reading the sequence

`Lex` returns names and delimiters in the order written and judges neither, so the caller's
whole grammar is a walk over that sequence — small enough to state completely. **The
delimiters alone fix the shape**: the member opens the last colon-separated position, and
every dot after it is path. Read left to right, the first dot ends scoping, and
a colon after a dot (`upgrade.outputs:charge`) is refused rather than read as a third scope.
Scoping is outside-in and finite, navigation is inside-out and nests; a grammar that let them
interleave would have no answer for what the later colon scoped.

Colon count is the rest of the shape: none is a bare member, one prefixes a process, two a
process and a task, three or more is refused. **There is no task-only scope.** A task id is
unique inside its process and nowhere else, so `charge:fetch` would name a task in every
process at once — a wildcard (§8e) wearing a scope's punctuation. Scope positions fill from
the left, process first, which is also what makes the trailing empty name unambiguous rather
than merely convenient: `order_proc:` is a process with no member, never a task with none.
Two colons with an empty member is the one place a task segment survives §8a's dimension
rule — `order_proc:charge:` stands for that task's members, which are the task-dimensional
ones by construction; the rule bites only when a member is written out.

**The member vocabulary is reserved in the member position, and quoting does not exempt it.**
`"output"` is the member, because quoting removes a delimiter's meaning and nothing else — a
property the lexer's tests already pin, and this is the case that makes it load-bearing. A
process named `output` is therefore unspellable as a bare token and spellable as `output:`,
the trailing empty member doing work no convenience reading would have justified. The
alternative — the member position falling through to a process name when the vocabulary
misses — is §8f's rejected bare name in a smaller costume: `outpt` would become a process
scope that matches nothing, in silence, rather than the typo it is.

The two empty names mean opposite things, each decided by the delimiter to its left. Empty
after a colon is a scope, as above. **Empty after a dot is refused** — `input.` and
`input..retries` name no step, and a trailing empty is worth a reading only where it can
stand for *every member below this*. An empty token (`--ignore ""`, or the hole in `a,,b`)
is a usage error for the same reason, and its message names the flag value rather than the
empty string it lexed to.

**None of this is an early exit.** Every refusal here is answerable before the request is
built, and taking it as one would skip the report — so the comparison still runs, still
prints, and the refusal lands after it with everything gating (§8c). A flag that is wrong
about what to ignore is not a reason to stop answering the question the operator asked.

### 8c. A token must name something that exists

The command is a guard: it sits in a pipeline and passes, run after run, while nothing
regresses. So an exclusion is a **standing policy**, not an acknowledgement of one break, and
it is validated for **existence, never for occurrence**. A token naming a slot that is
currently fine is correct and silent — that is the state a guard spends its life in. A token
naming something that is *gone* fails the run, because the operator who deleted it should
have deleted the exclusion in the same commit.

Shape and vocabulary are settled before the request (§8b); what is left is what only the
report can answer. Refused, on both flags:

- a process the report has no row for;
- a task no side of that process declares;
- a path that resolves in neither side's schema.

Existence is judged against **either side**, never the new one alone: excusing a finding
about a removed task or a dropped field must stay expressible, and those exist only on the
old side.

A row that was **not** compared (`new`, `nothing_to_compare`, `unanalysable`) has no schemas
in play, so a token aimed at one is accepted in silence. Most processes do not move between
two channels; refusing there would fail the build on every run where the named process
happened not to change, for a list that is correct.

**An invalid selection degrades to gating everything.** The report still prints — §5's
invariant does not bend for a bad flag — with the default gating set applied and the refusal
after it. Degrading to gating *less* would let a typo buy exactly the silence the flag was
refused for.

### 8d. The server reads a token, and applies it

Member, process and task names are answerable from the report alone. **A path is not**: it
resolves against the inferred context and contract schemas, which live server-side, and
re-deriving them in genctl would mean re-implementing inference. So `--check` / `--ignore`
are request fields, like `--ignore contract` before them, and **the whole selection — parse,
existence, gating — happens where the schemas are.** genctl sends the tokens it was given
and renders what comes back.

An earlier draft split it (server resolves, client applies) to keep a second consumer free
to gate differently off the same JSON. That is given up deliberately, and cheaply: a
consumer sends the flags it wants and reads the answer, rather than reimplementing the
policy. What the split actually bought was a second parser — the grammar has to be read
server-side for a path to be resolved at all, and a token parsed in two places is a
vocabulary that can disagree with itself.

The consequences are worth naming, because they are what makes it simpler rather than merely
different. **One place owns the vocabulary**: the member table is `validation.Member`, which
is also what an issue is filed under, so a member that exists is a member that can be
selected — by construction, not by a table someone remembered to update. **The gated verdict
is in the JSON**, so `--json` and the rendered report answer the same question, and CI reads
a boolean rather than deriving one. And the exit code stops being genctl's own arithmetic
over a report it half-understood.

`schema.At` already navigates a path against a schema and reports failure, so resolution is
a lookup per token rather than new machinery: fold the token's segments with
`schema.JoinPath` — which bracket-quotes what needs it — and hand the result to `At`. Going
through the rendering rather than comparing segment lists is what keeps the two grammars
apart; a token's names are strings, and only the schema side knows how a name is spelled as
an accessor.

Resolution reuses the analyses `CompareSet` already computed. Re-analysing per token would
double the expensive half of the report, and it is also what makes §8c's silence on an
uncompared row fall out rather than need a rule: a row that was never analysed has no schema
to fail against.

### 8e. Deferred

- **Wildcards.** `order_proc:` and `order_proc:charge:` cover the two cases that would
  otherwise want one, and a pattern language is a second grammar to validate — §8c's
  existence rule has no answer for what a pattern that matches nothing means.
- **`--ignore-file`** reading a policy document. Five `--ignore` flags in a CI invocation is
  the signal that the list has become a policy, and a policy wants a home, a comment per
  entry and a diff. Nothing about the grammar changes when it lands.

### 8f. Rejected

- **A bare name meaning whatever fits.** The first draft let a token be a member, a task id
  or a path, resolved by trying each — so `retries` was a task if a task had that name and a
  field otherwise, and validation had to explain which reading it had refused. Naming the
  member always, and scoping leftward, costs a few characters and removes the class.
- **A single path tree with the process inside it** (`order_proc.contract.output`). It reads
  tidier until a name contains a dot, and it conflates two things that behave differently:
  scoping is exact and finite, navigation nests. Separating them by punctuation is what lets
  dots prefix-match without a process name ever being a prefix of anything.
- **The expression language's accessor form** (`["odd:name"]`), which an earlier draft reused
  so that one rendering served a report path and a token alike. Dropped: a selector names
  report slots, not values — it has no indices and no computed keys, and nothing it spells is
  evaluated — so the two grammars answer to different readers and tying them together invites
  a token where an expression is meant. The cost is real and accepted: a report prints a path
  in accessor form, a token spells it in selector form, and the two differ exactly where
  quoting is needed. Aligning them means rendering the issue head as a token (§5), not
  merging the grammars.
- **A second command.** The two verdicts share the expensive half (one `analyze` per side)
  and the fiddly half (`--from`/`--to` resolution, channel-vs-pin selectors, the dependency
  closure, the union-of-names rule). Duplicating that to answer half a question, for an
  operator who is deciding one thing — can I deploy this — is worse on both counts.
- **A boolean per slot.** `--allow-breaking-output` was the first of them and generalises
  into `--ignore output`; a second would have made three spellings of one idea. Deleted
  outright, with no alias and no note: genroc has no users, so a migration path is effort
  spent on a migration nobody is making.

## 9. Open

- **Should a default be filled before the required check?** `conformObject` rejects an
  absent required property *before* it looks for a default (`validate.go`), so a default on
  a required property is inert: `required: [x]` breaks a caller even where `x` carries one.
  Fill first and the acceptance predicate gains the same rule as the after-conform view —
  *guaranteed present = required or defaulted* — and §3b's motivating case stops being a
  disagreement: the caller need not send it either. It does **not** collapse the two views,
  which still differ wherever only the new side carries a default — the upgrade fill still
  writes no defaults (§2d), so `default-added-narrows-every-read` stays a break, correctly.
  It removes one disagreement, not the distinction. Against it: a runtime semantics change
  touching every conform, silently widening what every stored definition accepts. For it: a
  default on a required property doing nothing at all is a trap in its own right.
- **Demand pruning.** A new main-line task's output is required at every later task even
  where nothing reads it (version-compatibility.md §10). Refining it only turns "different"
  into "tolerable", so it is safe to land later — but until it does, adding a task on the
  main line reads as breaking whether or not anything downstream cares.
- Does an `external.input` change deserve a verdict after all? The worker is usually code
  the same operator owns, which is an argument the fetch case cannot make.
- `SetReport.Compatible` keeps its current meaning — the conjunction over everything
  compared, ignoring nothing — and the gated verdict is computed separately. Recorded
  because the two can then disagree (green exit, `compatible: false` in the JSON), which is
  intended: the roll-up is what was found, the exit code is what this operator asked about.

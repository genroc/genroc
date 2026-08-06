# Compat verdict categories and gating

Refines [version-compatibility.md](version-compatibility.md) §3a/§3b, which reports two
verdicts — continuation and the output contract — and leaves `genctl compat` to fold them
into one word. That fold answers the wrong question, and this records the split, the rule
that assigns a slot to a side, and how an operator says which side gates their build.

## 0. Status

**Proposal.** One piece is built: the token lexer, `internal/selector` (§4a). Everything
else here is unimplemented, and §8 names the slice intended next. The rendering defect in §6
is a live bug independent of everything else and is not part of this proposal.

## 1. Two questions, not one

- **Upgradability** — can an instance that is running the old version continue under the
  new one? A question about rows this deployment already owns.
- **Contracts** — does the process still honour what the outside world was written
  against? A question about parties that are not in the deployment.

They are independent, and folding them costs accuracy in both directions. Two fixtures
report the wrong thing today, and both are the same miscategorisation:

- `shapes/nullable-input-added.yaml` reads **upgradable**, though a caller that omits the
  new property is now rejected at creation. Right about rows, silent about callers.
- `shapes/required-added-to-a-defaulted-property.yaml` reads **breaking**, though every
  stored input carries the key — creation conformed it once and persisted the filled
  default. Right about callers, wrong about rows, and pinned as a false alarm for exactly
  this reason.

Split, both become accurate without touching `isSubset` or the absent-as-null relation.

**Children are deliberately not a third category.** A bundle is checked against itself by
the registration preflight, so upgradability stays a per-process question. A child's own
`output` is still a contract — its consumers include parents outside the bundle.

## 2. Who submits the value sets the direction

| slot | submitter | relation | the conform behind it |
|---|---|---|---|
| process `input` | caller | old ⊆ new | `ValidateInput` at creation |
| process `output` | us | new ⊆ old | a waiting parent's `result_schema` at collect |
| `fetch.result_schema` | the service | old ⊆ new | collect |
| `external.result_schema` | the worker | old ⊆ new | submit |

**A verdict only where a conform stands between the two parties.** Everything else is a
changed slot: a `fetch` request (`url`, `method`, `headers`, `body`) is something we send
into a service whose tolerance is unknowable, and making it a verdict would turn every URL
edit breaking — which `shapes/url-changed.yaml` exists to refuse. `external.input` is the
same case (we send it; the worker's tolerance is its own business).

Two of these are new comparison surface: a narrowing `fetch.result_schema` means **our own**
conform starts rejecting responses it used to accept, and today that shows only as a
changed slot.

## 3. The input is in both categories, under different rules

The same schema pair is asked two different questions, and this is the case that motivates
the whole split:

- **Upgradability** reads the stored input and never conforms it again, so it uses
  `IsSubsetAbsentAsNull` — absence and null navigate identically.
- **Contracts** is what `ValidateInput` will do to the next caller's request, so it is
  strict.

A property that gains `required` while carrying a default is then upgradable (the row has
the value) and contract-breaking (the caller must now send it). Both true, and neither
statement is available today.

## 4. Selection

Default: **everything gates**. Two spellings for the gating set, and they do not combine —
passing both is a usage error, not an intersection:

    genctl compat --from a --to b --check upgrade    # only these gate
    genctl compat --from a --to b --ignore output    # everything but these

**Selection changes the exit code and the emphasis, never what is computed or printed.**
A non-gating finding still appears in the report, marked, and a trailing line names what was
excluded and why the exit is 0. This is the rule `internal/validation/CLAUDE.md` already
states for `nothing_to_compare` — an answer indistinguishable from "checked, and fine" is
worse than no answer — and a selection flag is exactly the feature that invites breaking it.

The two spellings fail in opposite directions: when a contract member is added later,
`--check` silently stops gating on it (fail-open) while `--ignore` gates it by default
(fail-safe). Both are offered anyway, and the not-gating line is what makes the inclusive
one defensible: the new member appears there rather than only in behaviour.

### 4a. The token grammar

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
step — never as a string, or `outputs.a` would swallow `outputs.ab`. **Colons do not nest**:
process and task are exact.

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

The syntax is deliberately **not** the expression language's accessor form (§4e).

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

### 4b. A token must name something that exists

The command is a guard: it sits in a pipeline and passes, run after run, while nothing
regresses. So an exclusion is a **standing policy**, not an acknowledgement of one break, and
it is validated for **existence, never for occurrence**. A token naming a slot that is
currently fine is correct and silent — that is the state a guard spends its life in. A token
naming something that is *gone* fails the run, because the operator who deleted it should
have deleted the exclusion in the same commit.

Refused, on both flags:

- a member name that is not a member;
- a process the report has no row for;
- a task no side of that process declares;
- a task segment on a member that has no task dimension (§4a);
- a path that resolves in neither side's schema.

Existence is judged against **either side**, never the new one alone: excusing a finding
about a removed task or a dropped field must stay expressible, and those exist only on the
old side.

A row that was **not** compared (`new`, `nothing_to_compare`, `unanalysable`) has no schemas
in play, so a token aimed at one is accepted in silence. Most processes do not move between
two channels; refusing there would fail the build on every run where the named process
happened not to change, for a list that is correct.

**An invalid selection degrades to gating everything.** The report still prints — §4's
invariant does not bend for a bad flag — with the default gating set applied and the refusal
after it. Degrading to gating *less* would let a typo buy exactly the silence the flag was
refused for.

### 4c. Who resolves a token

Member, process and task names are answerable from the report alone. **A path is not**: it
resolves against the inferred context and contract schemas, which live server-side, and
re-deriving them in genctl would mean re-implementing inference.

So the request carries the selectors and the response says which ones resolved (§6), while
the CLI still decides what that means for the exit code. **The split is: the server resolves
a selector, the client applies it.** Keeping the applying client-side is what lets another
consumer read the JSON and gate differently without a new endpoint; moving the resolving
server-side is simply where the schemas are.

`schema.At` already navigates a path against a schema and reports failure, so resolution is
a lookup per selector rather than new machinery: fold the selector's segments with
`schema.JoinPath` — which bracket-quotes what needs it — and hand the result to `At`. Going
through the rendering rather than comparing segment lists is what keeps the two grammars
apart; a selector's names are strings, and only the schema side knows how a name is spelled
as an accessor.

### 4d. Deferred

- **Wildcards.** `order_proc:` and `order_proc:charge:` cover the two cases that would
  otherwise want one, and a pattern language is a second grammar to validate — §4b's
  existence rule has no answer for what a pattern that matches nothing means.
- **`--ignore-file`** reading a policy document. Five `--ignore` flags in a CI invocation is
  the signal that the list has become a policy, and a policy wants a home, a comment per
  entry and a diff. Nothing about the grammar changes when it lands.

### 4e. Rejected

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
- **A boolean per slot.** `--allow-breaking-output` is the first of them and generalises
  into `--ignore output`; a second would have made three spellings of one idea. It is
  **removed rather than aliased** — it is genctl-only and pre-1.0, and an alias would
  outlive the reason anyone remembers it. Its usage error should name the replacement, since
  that is the only migration note a user will get.

## 5. What an operator sees

The fixtures assert the whole rendered report, so the rendering IS the deliverable and has
to be decided here rather than discovered while regenerating them.

    PROCESS       UPGRADE     CONTRACT
    order_proc    upgradable  breaking (ignored)
    child_proc    nothing changed

    order_proc v1 → v2
      contract
        input.retries  (input_schema):
          newly required field

    not gating: order_proc:contract — --ignore order_proc:contract
    exit 0

Issues group under the member that found them, so the same process can be sound for its rows
and broken for its callers without the reader having to work out which line is which.

**The verdict word stays true and carries the exclusion beside it.** A column reading
`breaking` next to `exit 0` is a contradiction a reader has to resolve by finding the
not-gating line; `breaking (ignored)` resolves it in place. The annotation appears only when
*every* break in that column was excluded — a partially ignored column still gates, so it
still reads plain `breaking`.

A row with no verdicts spans the columns with its status rather than filling them with a
word. `CompareStatus` exists precisely so `nothing_to_compare` is not mistaken for a
check that passed, and two columns of `—` invite exactly that reading.

Verdict words: `upgradable` / `breaking` under UPGRADE, `compatible` / `breaking` under
CONTRACT. Statuses (`nothing changed`, `new`, `unanalysable`) are unchanged.

### 5a. What is a finding, and what is only context

- **Removed tasks** are upgrade findings — an instance sitting on one has nowhere to go.
- **Added tasks** are printed and never gate: nothing is running on a task that did not
  exist, and §1's imprecision (a new main-line output reads as required everywhere) is
  already reported as the break it causes, at the task where it lands.
- **Changed slots** are neither. They are the report's only channel for a difference no
  verdict can see — `secret` dropped, `only_once` flipped, a URL repointed — so they print
  whatever the gating set says and cannot be ignored: there is no verdict there to suppress.

Ordering must be deterministic or the fixtures churn: processes as `CompareSet` already
orders them, members in the order above, and within a member the definition's task order,
then the path. Deduplication is unchanged — one difference in the data is reported once, not
once per task that can see it.

### 5b. The exit code

Exit 1 if a **gating** member reports a break, or if any row is `unanalysable`. `new` and
`nothing_to_compare` never gate, as today — a deployed channel always carries processes a
bundle does not, and counting them reports almost every real comparison as incompatible.

**`unanalysable` cannot be ignored.** It is not a verdict but the absence of one, and
excluding it produces precisely the answer indistinguishable from "checked, and fine" that
the status was introduced to prevent. A selection flag must not be able to buy silence about
a version that was never compared.

### 5c. Expect every fixture to move

The table gains a column, so all ~40 expected blocks change even where the verdict does not.
Two change verdict, both named in §1, and those are the only two that should. Regenerate with
`UPDATE_COMPAT=1` and read each block before committing — the harness records whatever the
code does, including a bug.

## 6. On the wire

`genctl` reconstructs a finding by peeling the path off a reason string at the first space
(`splitReason`). Gating cannot key off that, and neither can correct rendering: a
bracket-quoted key may contain a space. The report must carry issues as
`{member, task, slot, path, message}` — `member` being one of `upgrade`, `input`, `output`,
`fetch`, `external` — and the CLI must stop parsing prose. Per process:

    {"name":"p","status":"compared","from":1,"to":2,
     "upgrade":  {"compatible":false,"issues":[…]},
     "contract": {"compatible":false,"issues":[…]},
     "changed":["input_schema"],"removed_tasks":[],"added_tasks":[]}

`compatible` / `output_compatible` disappear from the row.

The request grows the selectors, because §4c resolves them here:

    {"from":…,"to":…,"selectors":["order_proc:outputs.charge.fee","upgrade"]}

and the response answers each one — `[{"selector":"…","resolved":true}, …]`, with a reason
on the ones that did not. It reports resolution only; nothing about gating crosses the wire,
so the report a second consumer reads is the same one either way.

The endpoint is generated from the action registry, so both shape changes mean regenerating
the committed `openapi.json` (`make swagger`); the TypeScript client regenerates inside
`test-int`.

**Slot resolution moves to the server with the issue.** `slotFor` maps a path to the slot
that changed by looking only at that task's changed slots, so an `outputs.<task>.…` break
caused by a definition-level edit is annotated with nothing at all —
`shapes/default-added-narrows-every-read.yaml` pins that empty annotation today. The server
knows both slot lists and should fill the field.

**Independent of this proposal**, the path in that message is built by a local `joinPath` in
`compat.go` that only concatenates with dots, while `schema.JoinPath` exists and
bracket-quotes. Task ids are unrestricted (only `end` and `next` are reserved), so a task
called `charge-eu` already prints `outputs.charge-eu.fee` — a subtraction to the expression
language — and a task called `a.b` prints something ambiguous with task `a`'s property `b`.
Both are the failure modes `path.go`'s own comment names. That is a bug fix with a fixture,
not a feature.

## 7. Where it lives

- `internal/validation/compat.go` — the `Report` split, and `compareInput` gaining a strict
  twin beside the relaxed read (§3). **Three explainer configurations, and the `swap` flag
  is the trap**: the upgrade side stays `{absentAsNull: true}`; the input contract is
  `{}` — strict, no swap, because it already runs old ⊆ new and the arrow reads old → new;
  the output contract stays `{swap: true}`, because it runs new ⊆ old while the reader is
  asking what *they* changed.
- `internal/selector` — **built**: the token lexer (§4a), and nothing else. The grammar
  rules live with the caller that knows the vocabulary, so the member table, the delimiter
  order and the task dimension are `cmd/genctl`'s to enforce.
- `cmd/genctl/commands.go` — two columns, grouped issues, `--check`/`--ignore` over
  `selector.Lex`, the not-gating line; `splitReason` is deleted rather than adapted.
- `internal/api/handlers_compat.go` — the shape it marshals, plus selector resolution
  (§4c): it holds the analyses, so `schema.At` per path selector is a lookup it can already
  answer. It resolves and never gates — **applying the policy stays client-side**, so a CI
  consumer reading the JSON can gate differently without a new endpoint.
- `tests/cli/testdata/compat/**` — §5c.

## 8. What lands first

1. **The rendering fix** (§6, last paragraph) with a weird-name fixture: a task id and a
   property that both need bracket-quoting. Independent of everything else here.
2. **Categories**: §5's report, structured issues (§6), the strict input check beside the
   relaxed read (§3), `--check`/`--ignore` at §4a's grammar with §4b's validation, and the
   not-gating line. The lexer is done; what is left of the grammar is the member table and
   the delimiter-order check, over the sequence it returns. No new comparison surface — the
   existing checks are re-partitioned, and the fixtures whose verdicts move (§1) move because
   they were wrong.
3. **Action contracts**: `fetch.result_schema` and `external.result_schema` (§2), paired by
   task id. New surface, so it lands behind settled categories.

Three fixtures the second step must add, because nothing today covers them: an edit that is
upgradable and contract-breaking at once (the split's whole point); a run where an ignored
break still prints and the exit is 0 (§4's invariant); and a token naming a task or field
the new version deleted — refused on stderr, exit 1, with the report printed above it and
everything gating (§4b). The third is the guard rule's whole purpose, so it is the one to
pin first — and its neighbour is worth pinning beside it: the same token while the slot
exists and is simply not breaking, which must stay silent.

## 9. Open

- Does an `external.input` change deserve a verdict after all? The worker is usually code
  the same operator owns, which is an argument the fetch case cannot make.
- Path tokens are the **only** reason step 2 grows an API round-trip: members, processes and
  tasks all resolve from the report, and §4c's request/response fields exist solely so a path
  can be checked against a schema. Dropping `.<path>` from the grammar for step 2 keeps it
  entirely client-side and costs an operator only precision — `order_proc:charge:fetch` is
  the same break, more bluntly. The grammar is unchanged either way; only what the last
  segment may carry moves.
- `SetReport.Compatible` keeps its current meaning — the conjunction over everything
  compared, ignoring nothing — and the CLI computes the gated verdict separately. Recorded
  because the two can then disagree (green exit, `compatible: false` in the JSON), which is
  intended: the roll-up is what was found, the exit code is what this operator asked about.
  Revisit if the disagreement reads as a bug to anyone.

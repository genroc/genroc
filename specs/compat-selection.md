# Selecting what gates, finer than `contract`

[compat-command.md](compat-command.md) §5 ships one flag: `--ignore contract`. This is the
general form it is a restriction of — a token naming a member, a process, a task or a field,
so a build can gate on part of a contract rather than all of it.

**Deferred.** It lands when someone needs it, and nothing in compat-command has to change
when it does: `contract` is already a token in this grammar, so today's flag is this one
restricted to a single value. The lexer is built (`internal/selector`); everything else here
is design.

**It predates compat-command §6a's addressing and must be reconciled on landing.** One rule
in particular: §1 below refuses a task segment on `upgrade`, because the task an upgrade
finding carried was the one that *noticed* it. §6a addresses such a finding at the task whose
context failed and reports it once, at the first — so `order_proc:settle:upgrade` is a
meaningful scope and the objection no longer holds.

Read `--check` / `--ignore` throughout as the pair that generalises `--ignore contract`.

## 1. The token grammar

**Colons scope, dots navigate.** A token names a member and qualifies it leftward:

    token    := [ <process> ":" [ <task> ":" ] ] <member> [ "." <path> ]
    <member> := upgrade | contract | input | output | <action_type>
    <name>   := a bare name, or "quoted" where it holds a delimiter

**The members are the two categories, the process-level contracts, and the action types** —
`fetch`, `external`, `child`, `child_map`, `child_list`, `delay`, `raise`. That is the same
vocabulary §6a addresses with, which is the point: a token is spelled the way the report
prints, so a line can be pasted back as a scope.

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

The syntax is deliberately **not** the expression language's accessor form (§6).

**A task segment is accepted only where the member has a task dimension** — the action
types. `order_proc:charge:input` is refused rather than quietly ignored: an input
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

## 2. Reading the sequence

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
process at once — a wildcard (§5) wearing a scope's punctuation. Scope positions fill from
the left, process first, which is also what makes the trailing empty name unambiguous rather
than merely convenient: `order_proc:` is a process with no member, never a task with none.
Two colons with an empty member is the one place a task segment survives §1's dimension
rule — `order_proc:charge:` stands for that task's members, which are the task-dimensional
ones by construction; the rule bites only when a member is written out.

**The member vocabulary is reserved in the member position, and quoting does not exempt it.**
`"output"` is the member, because quoting removes a delimiter's meaning and nothing else — a
property the lexer's tests already pin, and this is the case that makes it load-bearing. A
process named `output` is therefore unspellable as a bare token and spellable as `output:`,
the trailing empty member doing work no convenience reading would have justified. The
alternative — the member position falling through to a process name when the vocabulary
misses — is §6's rejected bare name in a smaller costume: `outpt` would become a process
scope that matches nothing, in silence, rather than the typo it is.

The two empty names mean opposite things, each decided by the delimiter to its left. Empty
after a colon is a scope, as above. **Empty after a dot is refused** — `input.` and
`input..retries` name no step, and a trailing empty is worth a reading only where it can
stand for *every member below this*. An empty token (`--ignore ""`, or the hole in `a,,b`)
is a usage error for the same reason, and its message names the flag value rather than the
empty string it lexed to.

**None of this is an early exit.** Every refusal here is answerable before the request is
built, and taking it as one would skip the report — so the comparison still runs, still
prints, and the refusal lands after it with everything gating (§3). A flag that is wrong
about what to ignore is not a reason to stop answering the question the operator asked.

## 3. A token must name something that exists

The command is a guard: it sits in a pipeline and passes, run after run, while nothing
regresses. So an exclusion is a **standing policy**, not an acknowledgement of one break, and
it is validated for **existence, never for occurrence**. A token naming a slot that is
currently fine is correct and silent — that is the state a guard spends its life in. A token
naming something that is *gone* fails the run, because the operator who deleted it should
have deleted the exclusion in the same commit.

Shape and vocabulary are settled before the request (§2); what is left is what only the
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

**An invalid selection degrades to gating everything.** The report still prints —
compat-command.md §5's invariant does not bend for a bad flag — with the default gating set
applied and the refusal after it. Degrading to gating *less* would let a typo buy exactly
the silence the flag was refused for.

## 4. The server reads a token, and applies it

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
double the expensive half of the report, and it is also what makes §3's silence on an
uncompared row fall out rather than need a rule: a row that was never analysed has no schema
to fail against.

## 5. Deferred

- **Wildcards.** `order_proc:` and `order_proc:charge:` cover the two cases that would
  otherwise want one, and a pattern language is a second grammar to validate — §3's
  existence rule has no answer for what a pattern that matches nothing means.
- **`--ignore-file`** reading a policy document. Five `--ignore` flags in a CI invocation is
  the signal that the list has become a policy, and a policy wants a home, a comment per
  entry and a diff. Nothing about the grammar changes when it lands.

## 6. Rejected

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
  quoting is needed. Aligning them means rendering the issue head as a token
  (compat-command.md §6), not merging the grammars.
- **A second command.** The two verdicts share the expensive half (one `analyze` per side)
  and the fiddly half (`--from`/`--to` resolution, channel-vs-pin selectors, the dependency
  closure, the union-of-names rule). Duplicating that to answer half a question, for an
  operator who is deciding one thing — can I deploy this — is worse on both counts.
- **A boolean per slot.** `--allow-breaking-output` was the first of them and generalises
  into `--ignore output`; a second would have made three spellings of one idea. Deleted
  outright, with no alias and no note: genroc has no users, so a migration path is effort
  spent on a migration nobody is making.

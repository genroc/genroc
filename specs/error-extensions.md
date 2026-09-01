# Process error model: considered extensions

Status: **X2 built 2026-08-22 (§X2-c, shipped as designed); X1 and X3 remain open
discussion, neither accepted nor scheduled.** Extends
[child-error-handling.md](child-error-handling.md) — its vocabulary (raise, panic,
defect, batch, slot, raise set) and invariants (I1–I6) apply throughout. Each entry
records the shape, the case both ways, and the **trigger** that should reopen it, so
"we didn't think of it" is never confused with "we declined it".

## X1 — Routing on batch shape

**Gap.** Only `raised[0]` in slot order routes. Fan out over 100; 40 raise
`rate_limited`, slot 0 raises `invalid_input` — the parent routes on the one and never
learns of the 40. The branch that matters ("all raised the same transient code → back
off and re-spawn" vs "mixed → one item is bad; re-spawning burns the rest") is not
expressible.

**Shape.** A quantifier on the match, never a payload: `when: all` on a rule
(`raised[0]` still selects the rule; `when` decides whether it fires).

**For:** zero type cost (no context slot, no schema change, R5 untouched); additive.
**Against:** adjacent to rejected D2 (the `siblings` aggregate) and thresholds
(`when: ">50%"`) are the natural next ask — and a count IS a value; rule matching gains
a second dimension; no observed demand.

**Trigger.** A real fan-out author asks for it, or abandons the error channel for
`{ok: false}` outputs and finds that unsatisfying.

### X1-b — re-spawn only the raised slots

The more valuable half, separable. Today recovering 40 failures re-spawns all 100
(§10.1). This is D7's deferred feature arriving from the other direction, and D7's
costing stands: a per-slot attempt dimension inside the sibling queries the
lock-ordering discipline protects — **by far the most expensive item here**; everything
else is validation or presentation.

**For:** removes the waste that makes X1's mixed branch painful; I1 survives (batch
output still requires every slot completed — a slot may just take several attempts);
additive. **Against:** the concurrency/schema cost; it argues against §0's "a raise is
settled" framing; needs a per-slot "which codes are retryable" answer neither feature
specifies.

**Relationship:** X1-b lowers X1's urgency — if only one is built, build X1-b.
**Trigger:** D7's own — the §10.1 workaround used repeatedly on batches big enough for
the waste to matter.

**BUILT 2026-08-27 — this is child-error-handling.md §5.5, and D7 is reversed with it.** The costing above was right that a per-slot attempt dimension is
needed and wrong about what it costs: the count rides the child's `_spawn_*` bookkeeping,
written by the parent at insert, so the sibling queries gain neither column nor predicate and
the lock-ordering discipline is untouched. The "which codes are retryable" question it
wanted answered is answered by `on_error` itself — a rule already names codes, and R5 bounds
them to `raises(D)`. The "argues against §0" objection is gone
too: §0 now says only a *declared* condition can be retried around. X1 (routing on batch
shape) is unaffected and stays open — §5.5 re-spawns every raised slot rather than
consulting the shape.

## X2 — A payload on `raise` — **BUILT 2026-08-22** (§X2-c)

**Gap.** A raise carries code + message only (I6); a raising child computes no output.
`card_declined` plus `{decline_code: "51", retry_after: 3600}` has nowhere to go but
message prose.

The accepted design is §X2-c below. X2-a and X2-b are kept as the record of how it was
reached: X2-a's trigger is what fired, and X2-b's gate is the idea X2-c replaces with a
cheaper one. **Read X2-c first** — the two below it are history.

**Why the unrestricted version is expensive:** typed parent-readable `detail` is one
variant per raise code, discriminated by `error_code` — `Raises()` becomes
`map[string]Schema`, R5 must union shapes across wildcard patterns, and the schema
machinery becomes load-bearing on the error path. Result-enum-with-payloads is sound in
languages whose match is exhaustive; D3 declines exhaustiveness, so here it would be
variants without the check that makes them sound.

### X2-a — operator-facing only

Payload on the raising child's row (instance detail, logs, API); `error` in the parent
unchanged; not matchable, not readable by expressions.

**For:** I6 survives literally; no schema machinery (`error_internal` column already
exists); sharpens §0 — diagnostics for humans, data for branching is `output`.
**Against:** does not solve its own motivating case (`retry_after` still cannot reach a
scheduling parent); two places to look for "what went wrong"; makes "let the parent
read it" an easier next ask.

**Trigger.** Repeated structured data smuggled into `message` prose — grep before
building.

**2026-08-21: the message is a template** (rendered against the clause's own scope,
type-checked to a non-null string; the *code* stays a literal, which is what keeps the raise
set computable). This was a correction, not a loosening — R2 never should have covered the
message. It does not grant X2, but it moves this trigger: smuggling is now a
`${ }` away rather than a hand-written string, and it reaches a parent through
`error.message`, which `collect` already fills. So the grep is for interpolations in raise
messages, and finding them argues for X2-a — the payload with somewhere honest to go —
rather than against the template.

### X2-b — parent-readable, gated on exact code match

> **Superseded (2026-07-24) — rejected on principle, not deferred**: data flows through
> the success path; the error channel signals. A raised condition carrying data is a
> differently-shaped *output*. See the replacement direction below. The analysis is kept
> because the gate idea is good and would apply again if reopened.

The gate: `detail` readable only by a rule whose `code` is a single exact literal — no
`%`, no lists, no catch-all — killing the union-across-a-wildcard problem. The residual
join (raise sites × children for one code) is finite and computable. Phasing fell out
naturally: a declared `detail_schema` first (mirrors `result_schema`, the unknown-type
narrowing pattern, no inference), inferred shapes later.

**For:** actually solves the case; the gate is structural, not conventional.
**Against:** sanctioned, typed smuggling (the older wording appealed to R2's message
half, which turned out never to have been a real rule — see child-error-handling.md R2);
the declared-tier registration check is real work; and the overuse risk:

**Overuse.** Today "an error is a branch slot, not a value" is enforced by *capability*
— a raise physically cannot carry data. X2-b downgrades that to documentation, and a
typed raise is strictly more ergonomic than `{ok: false}` outputs. Structural bounds
that remain: I1 (a batch with any raise produces no output, so the canonical misuse
stays impossible); a raise forfeits `output` entirely; `raised` is terminal and
non-retryable (D4). A size cap on `detail` would be the cheapest fourth guard. Residual
exposure: single-child flows.

### The replacement direction: union outputs

Authors reach for data-in-errors because the success channel cannot express "one of
several shaped outcomes". Instead of a second typed channel, let a completed process
output a tagged union (`output: {type: declined, decline_code: "51", …}`) and narrow on
the discriminant. One mechanism; and §0's line then holds because nobody wants to climb
the wall, not because the wall exists.

This is an increment, not a subsystem — `narrowCondition`, `withGuard`, and the union
accessors exist. Three gaps, increasing in cost: (1) sibling narrowing via a
discriminant (`X.disc == lit` should narrow `X`, not just `X.disc`); (2) narrowing
across `&&`/`||` (today only the ternary's call site narrows); (3) narrowing across a
switch case into the target task (flow typing over the task graph — hold until 1+2
prove out). (1)+(2) cover the motivating case, and (1) makes ascription syntax
unnecessary — the discriminant test *is* the narrowing.

**Deferred (2026-07-24)** with a warning unlike the X-items': deferring additive
features is free, but narrowing rules are near-permanent once definitions rely on them
— draw the supported patterns from real usage, do not guess.

### X2-c — parent-readable, caller-declared (the accepted design, built)

Two arguments closed this, both from 2026-08-22. Shipped the same day, in three parts, each
landing on its own: the `output.invalid` split below, then `data` on `Fault`, then `raises`.
Two decisions were taken during the build and are recorded where they belong — the size cap
was **dropped** (see its section) and a `raises` value of `null` is **refused**, since omitting
the code already says "carries nothing" while `{}` says "present, narrow it".

**1. The trigger fired.** X2-a asked for a grep before building. Six interpolations in
`tests/playground/script-node.yaml`, the one real definition — `${error.data.name}`,
`${error.data.kind}`, `${error.data.message}` twice, `${error.message}` three times. Two
of those are machine-readable discriminators being flattened into prose that the file's
own comment forbids parsing back. Sharper than the count: `$defs/script_error` declares
`stack`, the fetch validates it, and no raise message can carry it, because a stack trace
in a one-line message is unreadable. The most useful field for debugging a failed script
is fetched, typed, and dropped.

**2. The Overuse objection rested on a false comparison.** X2-b's costing says "a typed
raise is strictly more ergonomic than `{ok: false}` outputs". It is not, because
`on_error` has no `case:` — a rule selects by code and nothing else. The gradient:

| branch on | cost |
|---|---|
| a code | one line, inline in `on_error` |
| an output | inline in `switch`, with narrowing |
| **error data** | `goto: $handler` **plus a whole extra task** |

Error data is the least convenient of the three (`script.yaml`'s `failed` task exists to
pay exactly this toll). So the principle is not downgraded from *capability* to
*documentation* as X2-b feared — it is downgraded to *ergonomics*, which is a real guard
pointing the right way: codes stay the path of least resistance and data stays the thing
you reach for when you must.

> **This makes "no `case:` on `on_error`" load-bearing.** It reads today like a syntax
> gap and is now the guard the whole feature rests on. A future proposal to add
> expression matching to a rule — an obvious convenience, and it would let you branch on
> `error.data.kind` inline — must be judged as a proposal to dissolve this, not as an
> ergonomic tidy-up.

**The guard was dissolved 2026-08-27, deliberately** — `case` on `on_error` shipped
(child-error-handling.md M2). The warning above was answered rather than overlooked, and by an
argument this section did not have: a rule's `case` keeps the instance on the task that FAILED,
where `retry` can still act, while a handler that classifies leaves it standing past the batch
(M2, "what it removes is a task, and that is the point"). So argument 2's gradient no longer
holds as written — branching on error data costs one line now, not a whole task — and the guard
that survives is a different one: **a `case` reads only what the caller declared under `raises`
for the codes its rule names**, so data still has to be asked for by name. Argument 3, which
decides the shape, is untouched.

**Third argument, which decides the shape rather than the yes/no:** the gap is created
by *reuse*. `eval-node/README.md` shows the intended pattern — call the script inline,
branch on `error.data.name == "LimitExceeded"`, mint a specific code. That works today.
The playground factored the call into a reusable `script.yaml` child, and a generic
wrapper cannot mint a caller-specific code (R2: codes are literals), so `name` dies at
the process boundary. That is the exact mirror of what `unknown-type.md` already solves
on the success path — the wrapper emits the top type, the caller narrows with
`result_schema` — and the error channel had no counterpart.

#### Syntax

The child attaches a value; the caller declares its shape. A Shape on the clause, the
same form as `output` (a string expression or a nested object of expressions), rendered
in the scope the `message` template already renders in (`faultMessage`) — so no new scope
rules, and an `on_error` rule can forward a fetch's error body untouched:

```yaml
raise:
  code: script_threw
  message: "${error.data.name}: ${error.data.message}"
  data: "$: error.data"                  # kind, name, message, stack
```

Named `data`, not X2-a/b's `detail`: it is read as `error.data` at the only place it is
ever read, and two names for one value is a second thing to keep true.

**The slot is on `Fault`, so it is on `panic` too** (both **built 2026-08-22**) — §2.1 of child-error-handling.md says
raise and panic "differ in what they do, not what they carry", and excluding one would
mean special-casing validation to refuse it. Who may read it then follows from what the
clause already means, rather than being stipulated:

| clause | catchable | who reads `data` |
|---|---|---|
| `raise` | yes | the parent, as `error.data`, where the call declares a shape in `raises` — plus an operator |
| `panic` | never | an operator only: the instance row, logs, the API |

A panic code is excluded from `raises(D)` by construction (§2.3), so no declaration could
ever apply to one and there is nothing to type. That makes the panic half **exactly X2-a**,
and it needs none of the machinery below it: no schema table, no `ruleErrorData` branch, no
typing — write the evaluated shape and stop. It is the cheaper half and could land first.

### Two errors, one per direction

An instance holds at most two errors and they are stored apart:

| | storage | is | read by |
|---|---|---|---|
| **inbound** | `error_internal` (context slot `error`) | the error it CAUGHT | its own expressions, on tasks where the layer admits it |
| **outbound** | `error_code`, `error_message`, `error_data` | the error it CONCLUDED with | its parent, where the call declares the code in `raises`; an operator |

Both are whole errors: a clause authors `code`, `message` and `data`, so the outbound one is
not a payload hung off the inbound slot.

**The outbound one is three plain columns, not a JSON object.** `error_code` is filtered on —
`GET /instances?error_code=...`, and the index behind it — and a code buried in a blob can be
neither indexed nor matched in SQL. Only the payload gets a value column with an object-store
cut, because only the payload is arbitrarily large; the other two are short scalars that want
to be columns anyway. `error_data` is ABSENT rather than null where the clause carried none,
and that absence is what tells a parent's collect there is nothing to conform.

The resemblance between the two is the trap. On a task reached through `on_error`, `error` is
that task's INPUT — part of the state a layer describes and an upgrade validates against — so
a concluding fault editing it leaves an instance holding a context no layer admits.

That is not hypothetical. A `panic` carrying no `data` used to CLEAR `error.data`, so a
handler that had just interpolated `${error.data.name}` into the panic's own message left
behind a state whose handler task requires the very key the panic had deleted — and
`genctl upgrade` refused it with `required property "data" is missing`. Keeping "the error I
am handling" apart from "the error I am reporting" is what fixes it; nothing else has to know
the difference, because a parent reads the reported one through `raises` either way.

`error_data` is completion-only for the same reason `output_data` is, so the mid-process
checkpoint does not write it, and `RetryProcess` clears it — a revived instance reports no
error. Its context key keeps an underscore: nothing an author writes reads it, since the
instance has concluded by the time it exists, and `error` is the name authors already use for
the inbound one in `on_error` and `${error.data.name}`. Reusing that name for the outbound
direction would invert an existing meaning silently.

**The wire keeps the columns apart too.** Both endpoints return `error_code` and
`error_message` under those names, and the single-instance one adds `error_data`; a list row
omits only the payload, which is the one field that can be large. Nothing is reassembled into
an object on read: the field a caller filters on (`?error_code=`) is the field it reads back,
and one name means one type at every endpoint. An earlier shape returned an `error` OBJECT
from the single-instance endpoint while the list returned an `error` STRING, which is two
types for one name and the reason this is written down.

The caught error is not a field at either endpoint — it stays inside `context` under `error`,
where the definition reads it.

Its motivating case is the one X2-a always had. `script.yaml` panics `script_broken` with
`message: "the script is broken (${error.data.kind}) - ${error.data.message}"` and drops
the stack, because a stack trace in a one-line message is unreadable. `data: "$: error.data"`
puts it on the row, which is the only place it was ever going to be read — nobody can catch
`script_broken`.

**A panic's data stays on the instance that authored it.** A panic poisons its ancestors
(§2.3) and they inherit its code and message, as an unhandled raise's do; the payload does
not travel. An operator follows `error_code` down to the deepest instance, and copying a
payload onto every ancestor row would bloat each one to say the same thing.

The caller declares shapes in a table on the **action**, keyed by raise code — the exact
analogue of a fetch's status-keyed `responses`, and on the action rather than the rule
for the same reason `responses` is: it describes what the call can hand back, not what
you do about it. One declaration serves every rule, two rules catching one code cannot
disagree about its shape, and R5 already validates rule codes against the child's raise
set, so the same check catches a typo'd key.

```yaml
action:
  type: child
  name: script-node
  result_schema: { $ref: "#/$defs/reading" }     # success channel (existing)
  raises:                                         # error channel  (new)
    script_threw: { $ref: "#/$defs/script_error" }
    script_timeout: {}                            # declared, unknown
    # script_unknown undeclared → error.data absent
```

Three declaration states, all inherited from `responses`: **absent** → `error.data`
absent (undeclared data is never accessible); **`{}`** → the unknown type, present but
requiring narrowing; **a schema** → typed and navigable. Widening a rule's `code:`
patterns widens the type — a rule catching several declared codes sees `anyOf`, one that
can also catch an undeclared code gets `| null`.

#### What this costs to build

Much less than X2-b costed, because the caller declares. No `Raises()` becomes
`map[string]Schema`, no schema propagation across child versions, no compat surface for a
child's payload shapes. And the union-across-wildcard-patterns machinery X2-b named as
the expensive part **already exists and is already generic**: `errorDataSchema` unions the
arms of every reaching rule, and its own comment says the narrowing is "done by the rules
themselves rather than by any type-system machinery"; `ruleCatches(rule, code)` already
takes an `errcode.Code`. Only `ruleErrorData`'s *source* is fetch-specific — it reads
`a.Responses`. A child branch reading `a.Raises` sits beside it.

#### A mismatch is `output.invalid`, and the success path changes to match

This applies to `raise` alone — a panic's data is never declared, so there is nothing for
it to mismatch. A `data` value that does not satisfy the caller's declared schema reports
**`output.invalid`**, catchable by an `on_error` rule on the child task.

Getting here took two reversals, and both are worth keeping because the reasoning is the
same reasoning that governs the success path.

**What the code does today, verified rather than assumed (2026-08-22).** A child whose
output is the **unknown** type, narrowed by a caller's `result_schema` it does not satisfy,
registers fine — `checkChildOutputType` skips the open type, "no declared output = open
type = nothing to check" — and then fails at runtime:

```
parent: failed / engine.collect   "output validation: expected type object, got integer"
child:  completed, output 42
```

Two things follow that survive the reversals. **The runtime conform is not vestigial**: the
static check passes by construction for exactly the generic-wrapper case this feature
exists for, so the runtime one is the only gate that ever fires there. And **a hard failure
does not destroy the error being diagnosed** — an earlier draft objected that it would, and
the two rows disprove it: on the raise path the child keeps `raised`, its code, its message
and its `data`, while the parent's row names the mismatch.

**Why not terminal, after all.** The first reading was that a mismatch means two
definitions disagree about a contract registration already cross-checked — a defect, so
uncatchable, and `engine.collect` was the existing precedent. The open type breaks that
reading. When a generic wrapper forwards an unknown and a caller narrows it, the caller is
making a *bet* about a shape neither definition states, and the bet can lose with both
definitions perfectly consistent — a script's return changed, an upstream API changed.
That is not a defect; it is precisely what `output.invalid` already means on a fetch
("the response did not satisfy its result_schema"). Two mechanisms both named
`result_schema`, failing for the same reason with different codes and different
catchability, was the real inconsistency.

Catchable also forecloses nothing: a caller with no matching rule still fails terminally,
exactly as today. `engine.collect` removed a choice and bought nothing.

**So the shipped success path changes too**, and this half is independent of the rest of
X2 — it is about `result_schema`, needs none of the `raises` machinery, and could land
first. It did: **built 2026-08-22**, ahead of the rest. Three notes:

- **A split, not a rename.** Only the conform becomes `output.invalid`. The four other
  failures reaching the same `failInstance` are corruption rather than contract — a
  sibling that is not `completed` ("an invariant, not a case to handle"), a single-child
  task with ≠1 sibling, an invalid `_spawn_index`, and object-store resolution failing while
  the value is materialised. Those stay `engine.collect`.
- **It amends E6.** §2.4 of child-error-handling.md justifies the no-namespace rule with
  "there is nothing else they can see: every other failure path … goes straight to
  `failInstance`". A child task's catchable set becomes `raises(D) ∪ {output.invalid}`.
  That stays unambiguous for the reason §2.4 itself gives — R1 forbids dots in raised
  codes and every engine code has one — but the sentence is false as written.
- **R5 admits the one dotted code** (built): `matchesSomeRaise` unions
  `errcode.OutputInvalid` into the raisable set, so `code: ["output.invalid"]` on a child
  task is reachable rather than rejected.

On the raise path the code is **replaced**, so a rule matching the original raised code no
longer fires — the fetch precedent, where a declared body that fails validation means
`code: [http.4%]` "no longer catches a 400 whose body is malformed".

#### Size cap at the raising end — **dropped 2026-08-22, not built**

The reasoning below still holds about *where* a cap would go, and it is kept for that: at
the raising end, failing the child as its own defect, because capping at the reading end
means truncating and truncated JSON cannot satisfy a declared shape. What was dropped is
the cap itself. A process `output` carries no limit either, and `data` is the same kind of
value written by the same author — a limit here and none there is a rule to remember rather
than a guard, and the object store already absorbs the size. The X2-b "cheapest fourth
guard" argument does not survive X2-c: the guard that ended up doing the work is
ergonomics (the table in argument 2), not a byte count.

A cap belongs where the value is attached, failing the *child* as its own defect — on
both clauses, since an over-cap payload is an authoring bug either way. Capping
at the reading end would mean truncating, and truncated JSON cannot satisfy a declared
shape — it would trip the mismatch above and kill the parent for the wrong reason. The
invariant to preserve: `data` either conforms or the definitions are wrong.

#### Inferring the child's `data` shapes — **built 2026-08-27**, having been out of scope

The original entry read: *"the caller declares, which is what keeps a generic wrapper generic.
If discoverability becomes the complaint, `raises(D)` is already published per version and can
carry shapes later without changing this syntax."* Both halves survive — the caller still
declares, and the syntax did not change — but the conclusion did not, because the argument
answers the wrong question. Keeping the wrapper generic is about what the CHILD may leave
unstated; it says nothing about whether the caller's bet is checkable, and the success channel
has always checked exactly that bet with `checkChildOutputType`.

What made it wrong in practice: `data: {name: …, stack: …}` under a caller declaring
`{name, message}` required is a conform that fails on **every** run, and nothing said so until
one did. The success channel would have refused it at registration.

So `SchemaFile.Raises` types every code a definition raises — the error channel's
`ProcessOutput` — and `checkDeclaredRaises` runs `NarrowsTo` against the caller's declaration,
the same relation `result_schema` gets and sound for the same reason: `Engine.raisedData`
conforms the payload against that very schema. Three rules the type follows from the runtime:

- **Two clauses on one code are a union.** Either may fire, so a declaration must accept both;
  what only one of them sets is not something a caller may require.
- **A clause attaching nothing types as `null`**, not as absent. `setErrorData` CLEARS the
  slot, so the caller conforms `null` — and a declared object shape does not admit it.
- **Panics contribute nothing**, for the reason `raises(D)` excludes their codes.

What still reaches the runtime conform is the case registration cannot type: a payload whose
own type is the top type. That is the open-type story of §X2-c intact — a generic wrapper
forwarding an unknown, a caller narrowing it, and the bet losing with both definitions
consistent. What no longer reaches it is a bet that could never have won.

## X3 — Opt-in exhaustiveness over a child's raise set

**Gap.** R5 checks only that every rule can fire; a code added to a child with no rule
surfaces at runtime (§3.1 row 3).

**Framing that decides the shape:** the motivation is change *subscription* ("tell me
if this child's raise set drifts"), not strictness — closer to a lockfile than a
linter, so opt-in is correct, not a compromise. The flag goes on the **child entry**,
not the task: a task-level flag subscribes to the union across all children, making it
noisiest exactly where it looks most useful.

**For:** D3 untouched for everyone else; one boolean, no rule-level syntax; reversible.
**Against:** only helps the already-careful; permanent schema surface for
undemonstrated demand; and a cheaper alternative may dominate — `raises(D)` is already
published per version, so a `genctl` diff + CI step answers the question with zero
engine surface.

Opt-in is defensible only because the default is loud: an unhandled raise fails the
parent with the child's own code, naming child and slot.

**Trigger.** A team reports a production surprise from §3.1 row 3. Once is anecdote;
twice is a signal.

### X3-alt — required catch-all (rejected on judgement)

Rule considered: a child task with partial rules and uncovered codes must carry an
explicit catch-all (verb-less = "the rest are defects") — the `switch` catch-all rule
made conditional on coverage, with the required-fallthrough-acknowledgement precedent
behind it. The opt-out marker even exists already (a verb-less catch-all is legal and
behaves identically to no rule).

**Rejected because:** it breaks every existing definition with partial rules (the
constraint D3 set); it cannot be uniform with `switch` (an action task's engine-code
space is open, so the rule would apply unevenly and lose the analogy's consistency);
and every opt-out spelling is unpleasant (`code: []` reads unfinished; `panic: true`
costs a `Fault | true` union; `goto: panic` breaks §0's field→outcome mapping and adds
a third reserved bare word next to `end`'s existing sharp edge). On-by-default is the
wrong default for something most parents do not want.

## Summary

| | adds | for | against |
|---|---|---|---|
| **X1** | `when: all` quantifier | real branch, zero type cost | adjacent to rejected D2; threshold slope |
| **X1-b** | partial re-spawn | removes the waste that makes X1 matter | per-slot attempts inside the deadlock discipline |
| **X2-a** | operator-only detail | I6 survives; sharpens §0 | misses its own example; widening pressure |
| **X2-c** | caller-declared `raises` | **built** — the panic half is X2-a exactly | costs a caller a declaration per code it reads |
| **X2-b** | typed detail, exact-gated | ~~solves the case~~ | **closed** — data belongs in the success path |
| **X3** | per-entry `exhaustive: true` | right shape for a subscription | helps only the careful; CLI diff may dominate |
| **X3-alt** | required catch-all | catches the careless | breaking, non-uniform, unpleasant syntax |

Carry forward: X1-b over X1 if either; and X2-b's closing principle — *data flows
through the success path* — applies to whatever looks like the next X2-b.

# Declared slot schemas: the slot's published type, conformed at the boundary

Seven slots hold a **shape** — a templated value whose type is inferred and then, at some of
them, checked against a target the slot fixes. This is the option to write that target down:
`input_schema` beside a child's `input`, `body_schema` and `query_schema` beside a fetch's,
`output_schema` beside a task's and the process's own.

Where one is written it becomes that slot's **public type**: what the editor completes against,
what `$process` spreads, what the version comparison reads — and the value is conformed to it
on the way out, so the declaration is a true description of what left rather than a claim about
it.

The point is not expressiveness. It is that a schema can be **imported** — from an OpenAPI
document ([openapi-resolver.md](openapi-resolver.md)), from a child definition
([source-resolution.md](source-resolution.md) §`$process`), from a file a worker fleet
publishes — and a slot that takes one is a slot an author can check a call against without
running it, without a database, and without the other side being registered.

## 0. Status

**BUILT 2026-09-18.** Every slot in §2, the closed relation, the conform, the editor half and
the `$process` spread. What is NOT built is listed in §11, and one decision moved in the
building: the fetch request side folds into `engine.input` rather than earning a code of its
own (§4), on the argument §11 itself made.

Two things the build found, both recorded where they bite. The `$process` fixture in
`tests/lsp/spread_test.ts` carried a **latent type error** — a parent forwarding an optional
`n` into a child that requires it — which nothing could report until this check ran offline;
that is the feature working on its first real document. And the `closed` rule's open-map arm
(§3) is not decoration: without it the conform's strip stays reachable and §4's assertion is
quietly false, which no table of declared properties would have caught.

The first draft made a declaration a **floor**: checked against the inferred type and
replacing it nowhere, with no runtime conform. That is reversed here. A declaration is a
boundary, it is the slot's published type, and §4 is the conform that earns the word. What
survives unchanged is §3, which is what keeps the reversal honest.

The conform is an **assertion** — every value it sees was computed from already-conformed
values by expressions the checker typed, so a failure is a defect in genroc's type system and
never a condition in the data. That is §4's second half, and it is load-bearing rather than a
remark: it decides the relation (§5), it decides what a declaration may say (§8), and it is
why the failure is uncatchable. Two things in the revision that added the conform were wrong
because they were written before it, and both are corrected in place.

It needs nothing unbuilt. The check is `Shape.Schema`
([shape.go:24](../internal/shape/shape.go#L24)), which has done this for child input and for
`headers` since the beginning. The conform is `ConformToSchemaExactly`
([validate.go:168](../internal/schema/validate.go#L168)), built and pinned in both directions.
[typed-values.md](typed-values.md) §Where it applies listed *per-action payload schemas* as
deferred behind the `$:` grammar; that grammar is built, and this is the deferral coming due.

§4's relation question was settled the way its second ending suggested: `nullRemoval` split
out of `afterConform`, so the new relation takes the removal rule without the defaults rule
that `ConformToSchemaExactly` does not perform. `schematest/conforms_exactly_test.go` is the
pairing, and `TestConformsExactlyToHasNoDefaultsRule` is why it is a fourth relation rather
than a flag on the third.

## 1. Thesis: where a declaration exists, it is the type

> A declared schema is the slot's public type. The value is conformed to it before it leaves
> the slot, so what the declaration says is what left. Where no declaration exists, nothing
> changes and the inferred type remains the only answer.

Two consequences, and they are the reason for the shape:

- **A declaration is worth trusting.** A published type that merely *described* a value
  would be a second thing to keep true, and every consumer would have to decide whether to
  believe it. Conforming makes it true by construction, which is the same move
  `input_schema` already makes at the other end of a process.
- **The author stops doing bookkeeping the system can do.** §4 is the worked case: an
  optional non-nullable property fed a null needs a key removed, and there is no builtin that
  removes one. Without the conform the author writes around it or the declaration is unusable.

The conform is an **assertion**, not a check: every value it sees was computed from values
already conformed, by expressions the checker typed, so it cannot fail unless genroc's type
system is wrong. §4 makes that precise, and it is what decides two questions this document
previously got the other way round.

The cost is real and is named rather than buried: a declaration changes what a slot publishes,
so adding one is a version event (§9).

## 2. The slots

| slot | shape | target today | declared |
|---|---|---|---|
| `tasks.<id>.action.input_schema` | child / `child_list` / external `input` | the child's `input_schema` at registration, or nothing | §5 |
| `tasks.<id>.action.children[k].input_schema` | that entry's `input` | same | same |
| `tasks.<id>.action.body_schema` | fetch `body` | nothing — free projection | §5 |
| `tasks.<id>.action.query_schema` | fetch `query` | `object` of scalar-or-array-of-scalar, nullable | §5 and §6 |
| `tasks.<id>.output_schema` | task `output` | nothing | §5 |
| `output_schema` | process `output` | nothing | §5 |

Every one is optional, and every one sits beside its shape under the name `<slot>_schema` —
the spelling `result_schema` and the definition's own `input_schema` already use.

**`child_list` is the one row that is not what its name suggests.** It has no `input` shape at
all: each element of `over` is one child's input, so the declaration types **one element**,
matching `result_schema` there. Both halves follow from that and both were wrong in the first
build — the check ran against an absent `input` (an empty object, which any schema of optional
properties accepts, so it asserted nothing), and the conform runs per element rather than once.
`over` with no declared item type is refused by name, since there is nothing to check.

`headers_schema` is deliberately **not** in the table. Headers already have a fixed target
(`object<string>`) and a declaration would add only required-ness. The one producer that would
fill it is an importer's request side, which does not exist; build it when that does.

**Placement is per action type**, refused by name where it does not belong, the way
`validateActionRequiredFields` refuses `responses` on a child
([validate.go:598](../internal/model/validate.go#L598)). `body_schema` and `query_schema` are
fetch-only; `input_schema` belongs to the action types that send an input and is refused on a
fetch, where `body_schema` is the name.

## 3. The check is CLOSED, and the conform is why that is a choice

`IsSubset` lets the sub side carry a property the super side never declares, whenever super
has no `additionalProperties` ([subset.go:354](../internal/schema/subset.go#L354)). That is
correct where it is used, because a conform strips the extras at the boundary.

These slots now have a conform too (§4), and it strips undeclared keys like every other. So
the open relation would not merely say nothing about `pgae=2` — it would **drop it silently**,
which is strictly worse than the first draft's complaint. A parameter the author wrote,
removed on the way out, with nothing said anywhere.

Therefore: **a key the declared schema does not declare is refused.** Not because nothing
could handle it, but because the thing that would handle it is a silent deletion of something
a person typed. The conform's stripping stays where it belongs — for keys nobody wrote.

It is a fourth flag on `subsetMode` ([subset.go:14](../internal/schema/subset.go#L14)) —
`closed` — not a walk beside the relation, for the reason `ConformMode` and `ExplainSubset`
are also modes and not walks: a parallel walker rediscovers unions, `$ref` cycles and open
maps badly and then has to stay in step forever
([internal/schema/CLAUDE.md](../internal/schema/CLAUDE.md)). It reaches `checkObject` as one
rule — every property sub declares must be declared by super — and rides through unions, refs
and array items with the walk that already exists. New break kind `BreakUndeclared` beside the
five in [subsetbreak.go:20](../internal/schema/subsetbreak.go#L20), because a caller wording it
is saying something the other five do not say.

**It must read sub's `additionalProperties`, not only sub's `properties`.** An inferred type
can be an OPEN MAP — `object<string>`, a `child_map`'s output — and a value of one carries keys
no schema names, so a closed declaration over it would leave the conform's strip reachable
after all. That is the hole that would make §4's assertion false while every table-driven test
of declared properties still passed, so the rule is: an open-map sub against a closed super is
refused, at every depth.

**`additionalProperties` in a declared schema is refused, at any depth.** It is the keyword
that would say "and extras are fine here", and admitting it would make the closed rule
conditional on a keyword. Refusing is the reversible direction: it costs an author a
projection today and can be relaxed to exactly the existing open behaviour on the day the
argument arrives. The refusal walks the declared document with `mapChildren` and lives beside
`validateActionSchemas` ([validate.go:710](../internal/model/validate.go#L710)), not in
`CheckDoc` — the keyword is perfectly valid in a `result_schema` and this is a per-slot
restriction, not a schema rule.

**The trigger to revisit is a count, not a debate.** An imported request-body schema will
carry `additionalProperties` sometimes; the resolver's translate row already handles the
keyword for responses ([openapi-resolver.md](openapi-resolver.md) §3). Because the refusal
names the keyword and the slot, running the importer over real documents counts the cases for
free — the same measurement §6 of that spec asks for before building its `allOf` fallbacks.

## 4. The conform, and the null the author should not have to think about

The declared schema is applied to the value with `ConformToSchemaExactly` before the value
leaves the slot. That mode exists, is built, and is a **mode on the one schema-and-value walk**
rather than a traversal beside it.

The case that motivates it. An author declares an optional, non-nullable property:

```yaml
output_schema:
  type: object
  properties: { discount: { type: number } }
```

and the output expression yields `null` there, because a `??` chain ran out or a member read
missed. `{"discount": null}` does not satisfy that schema. Absence does. There is **no filter
builtin** and no way to write "omit this key when null" — the same gap that forced arrays into
`query` ([fetch-http-surface.md](fetch-http-surface.md) §1) — so without a repair the author
either cannot use the declaration or writes around it at every site.

The repair is one line that already exists
([validate.go:168](../internal/schema/validate.go#L168)):

> a stored null that the schema will not hold cannot stay — but where the property is
> OPTIONAL, absence is valid, so removing the key reconciles the value instead of failing it.

Its limits are the design, not a shortfall. It does not fire on a **required** property
(neither state is valid, and nothing can fix it), not on an **array element** (dropping
shortens the array), and never where the target is **also nullable** (both states are valid,
so removing would invent a canonical form the schema does not name). Which means the
declaration is how an author says which they want: `type: number` drops the null, `type:
[number, "null"]` sends it. That is the whole ergonomic — `{"discount": null}` and `{}` are
different requests to a real API, and this is the slot where you say which one you meant.

**`query` already behaves this way** and always has: a null value omits its parameter. So this
generalises a rule the system already has rather than introducing one, which is the strongest
argument for applying it at every slot in §2 rather than at the process output alone.

### The relation must accept exactly what the conform closes

This is the invariant the schema package is most emphatic about, and it is where the
implementation starts:

> A relation that tolerates more than the fill can close promises a migration that then fails
> to conform; a fill that closes more is dead code.
> ([internal/schema/CLAUDE.md](../internal/schema/CLAUDE.md))

`ConformToSchemaExactly` has two halves and **they are pinned against two different
relations**: the insert half (write a null into an absent required nullable) against
`IsSubsetAbsentAsNull` in `schematest/absent_test.go`, and the remove half — the one this
section is about — against `IsSubsetAsStored` in `schematest/conform_exact_test.go`. The
difference between those two relations is the `afterConform` flag
([subset.go:337](../internal/schema/subset.go#L337)), which gates the removal **and** carries
a second rule: a property the sub side declares with a `default` is guaranteed present,
because creation filled it.

Nothing has conformed our sub side. It is the inferred type of an expression, and inference
does not emit `default` — so that rule looks vacuous here, and "looks vacuous" is not the
proof this invariant asks for. **Settling it is the first task**, and there are two honest
endings: prove the defaults rule cannot fire on an inferred schema and reuse
`{closed, absentAsNull, afterConform}`, or split `afterConform` into the removal and the
defaults rule and take only the first. Either way the test is the existing shape — every gap,
both directions, and the conformed value passing a strict re-check.

### Where it runs, and what it costs

| slot | conform point |
|---|---|
| process `output` | at completion, before the value is stored ([advance.go:632](../internal/engine/advance.go#L632)) |
| task `output` | when the output map is evaluated, before it becomes `outputs.<id>` ([advance.go:472](../internal/engine/advance.go#L472)) |
| fetch `body`, `query` | before the request is built, so a dropped null is never serialised |
| child / external `input` | before the payload is handed over — and **before** the child's own `ValidateInput`, which stays the child's boundary and is unaffected |

Idempotence matters here because a task output is conformed and then read by a process output
that is conformed again. `conform_exact_test.go` already asserts it; this spec adds a caller
that depends on it.

### This conform is an ASSERTION, and that is the whole reason it is safe

A boundary conform in genroc is one of two things, and which one depends on **where the value
came from**:

- a value arriving from **outside** — a fetch response, a child's output, a worker's
  submission, an instance's input — is unknown until it arrives, so conforming it is a genuine
  **check** with a legitimate failure. That is why `result.invalid` is catchable.
- a value **computed here** from values already conformed, by expressions the checker typed,
  is one the type system has already proven. Conforming it is an **assertion**.

Every slot in §2 is the second kind. The context is built from values each conformed at their
own boundary, the expressions over them are typed at registration, and §5's relation proves the
result fits the declaration. So **the conform cannot fail, and a failure is a bug in genroc** —
unsound inference, a relation that accepted a gap its fill cannot close, or a defect in the
conform itself. It is never a condition in the author's data.

Three things follow, and the first two are corrections to this document:

1. **Unknowns must stay refused** (§5). Admitting `{}` on the value side is the one thing that
   would make a failure legitimate, because an unknown really can be anything at runtime. The
   draft that licensed `NarrowsTo` here was trading the assertion away for a convenience.
2. **A declaration may not narrow** (§8). The same argument, from the other end.
3. **It is uncatchable, and that is not a limitation.** A catchable failure would be routed by
   an `on_error` rule an author wrote, the instance would carry on, and genroc would never
   learn that its type system is unsound. Making it catchable *hides the bug it exists to
   reveal*.

The `engine.*` family is already exactly this category — *"the engine failed the instance
itself, not a call. These are TERMINAL: they go straight to `failInstance` and are never routed
through `on_error`, so they cannot be caught"*
([errcode.go:106](../internal/errcode/errcode.go#L106)) — and **`engine.input` is already this
assertion**, for a child's input failing its `input_schema` after registration checked it. So
the input slots need no new code. The output slots want `engine.output` beside it; the fetch
request side is §11's one naming question.

**Not a Go panic.** A worker advances many instances, so a panic takes down work that has
nothing to do with the defect — `engine.panic` exists precisely to contain one that escapes.
A terminal uncatchable code is genroc's spelling of "crash this instance loudly": the run stops,
nothing routes around it, and `error_code` names it for anyone grepping.

**The unrepairable case is unreachable, and the pairing is why.** The one input the conform
cannot fix is a required non-nullable property holding null — and the removal rule is gated on
the property being optional ([subset.go:337](../internal/schema/subset.go#L337)), so the
relation refuses that gap statically rather than accepting it. Relation and fill agree at the
edge, which is the invariant above doing its job. The same holds for stripping: §3 refuses an
undeclared key at registration, so the conform's strip has nothing left to remove.

## 5. What each check compares

**The relation is `IsSubset`, closed, plus §4's null rules — and NOT `NarrowsTo`.** This was
written the other way in the revision that added §4, on the reasoning that a conform licenses
`NarrowsTo`: the schema package permits an unknown `{}` on the value side *only* where a
runtime conform stands behind the claim ([accessors.go:242](../internal/schema/accessors.go#L242)),
and §4 supplies one.

That reasoning is wrong, and §4 is what refutes it. An unknown really can be anything at
runtime, so admitting one is precisely the thing that would give the conform a **legitimate**
failure — and the conform has to be an assertion. Licensing `NarrowsTo` would buy
`body: "$: outputs.x"` over an unknown `x` and pay for it by making every conform failure
ambiguous between a genroc bug and an author's untyped value, which is the distinction the
whole design rests on. So the schema package's original line stands as written: *an unknown
flowing into a typed input is rejected on purpose*.

The combination to settle is therefore `closed` plus §4's null rules over the plain relation,
which is the pairing §4 says to pin first.

**Child input.** Two checks, and they are different checks. `inferred` against `declared` runs
**closed** and needs no database, which is the entire point — it is the first input check the
editor and an offline `genctl` can run at all, since `ValidateChildProcessRefs` needs a
`DefinitionGetter` ([validate_children.go:22](../internal/validation/validate_children.go#L22))
and therefore never runs in either. `declared ⊆ child.InputSchema` runs at registration and
runs **open**: the child's own schema is not ours to close, and closing it would refuse a
declaration that is perfectly good.

**The second check REPLACES the old one where a declaration exists, and must.** The old check
compares the INFERRED type against the child, and the inferred type still carries the nulls the
conform removes — so a call that works at runtime is refused at registration. A nullable input
declared non-nullable is exactly the case §4 exists for, and leaving both checks in place makes
the feature unusable on the slot it was written for.

**`$process` should spread `input_schema`.** It fills `name`, `result_schema` and `raises`
today ([structural.go](../internal/sources/structural.go)) and the input side is the one it
leaves out. Unlike the others this is a **copy, not an inference**: a definition's
`input_schema` is written by its author, so the spread reproduces it through
`selfContainedSchema` and nothing is derived. That makes the registration check above a check
that the copy is still current — the `$process` analogue of a stale generated client.

**Fetch body.** A note for whoever writes the importer's request side: the dialect table strips
`format` and `pattern`, and the argument that stripping is safe
([openapi-resolver.md](openapi-resolver.md) §3) is a **response-side** argument — it accepts
more, which is the harmless direction for something arriving. On a request, a stripped
`pattern` means the check passes a value the server rejects, and the conform will not catch it
either, since the stripped keyword is not in the schema being conformed against.

**Task and process output.** The conform is what makes `output_schema` a published type rather
than an assertion, which is §8.

## 6. `query_schema` has a target above it

A query value is a scalar, null, or an array of scalars (`queryValueSchema` in
[definition.go](../internal/model/definition.go)), and a declaration does not get to widen
that. So `query_schema` is checked against the built-in target when it is declared, before any
shape is inferred against it — a bad declaration is then reported as a bad declaration rather
than surfacing later as a confusing complaint about a shape that was doing what it was told.

The null rule composes rather than conflicting. A null omits its parameter at serialisation;
§4 removes the key earlier, at the conform. Both land on the same wire bytes, and the
declaration is what lets an author say that an optional parameter is genuinely optional.

**So the conform is unobservable here, and that is not a gap.** Every case it could change is
already closed: a null is omitted either way, an undeclared key never reaches runtime because
§3 refuses it at registration, and a query value is a scalar or an array of them so there is no
nesting to repair. The slot keeps the conform for uniformity — one rule at every slot — and its
e2e test pins the two rules AGREEING rather than pretending to exercise it.

## 7. What the editor does with it

Every slot in §2 is a mapping whose keys are the author's own, so the editor offers **nothing**
inside one today. Completion has two sources and neither can answer there: `legalKeys` walks
the *language's* generated schema, which describes a `body` as a permissive object because that
is what it is, and `membersOf` reads the author's own inferred types but only on the right-hand
side of a `$:`. A declared schema is an author's type in a KEY position, which is precisely the
missing half. So the payoff is not the diagnostic — it is that typing inside a `body` starts
offering the fields the endpoint accepts, with their types and their prose.

**It follows a precedent the error channel already set.** `raises` exists for this reason. A
child's raise set is knowable from the child's file and `findProcess` would find it, and
completion deliberately does not look: *an answer that depends on another buffer's state is one
a reader cannot check* ([internal/lsp/CLAUDE.md](../internal/lsp/CLAUDE.md)). So the caller
writes the codes down at the call site, `$process` fills them in, and the editor answers from
this document. `input_schema` is that same move on the input channel and `body_schema` is it for
an endpoint.

| the cursor is | answers with | today |
|---|---|---|
| on a key inside a declared shape | the declared properties not yet written, required first | nothing, the mapping is open |
| on a value whose declared property is an `enum` | those values | nothing |
| hovering such a key | the DECLARED type and its `description` | the expression's type |
| inside a `$:` in that slot | the scope, unchanged | unchanged |

Key completion is **`legalKeys`'s item shape fed from `membersOf`'s source**, and saying it that
way is the design. The required-first `sortText`, the colon the item writes, the rule that drops
what is already written are all built and are all about the key position; the type summary,
`MayBeAbsent` and the null-strip are all built and are all about a `schema.Schema`. Neither half
is new. What is new is that they meet.

**Hover on a key is the declaration's answer, not the expression's**, and the two diverge
exactly where the conform repairs something: the expression beside an optional non-nullable
property is nullable and what arrives is not. Answering with the expression there shows a reader
the value they wrote rather than the value the far side receives. It fires on the KEY only —
inside the expression the type of the expression is still the question being asked.

A declaration is **described, not read**. `Schema.At` walks a path the way an expression would,
so an optional property comes back nullable because a missing key reads as null — correct for a
value and wrong for a schema, where it makes the editor contradict the document the author is
looking at. `declaredNodeAt` walks declared properties instead and reports optionality as the
`?` mark `Summary` already uses.

The enum row is the easy one for once. Three value slots have a closed set today and each needed
a bespoke function — `routingValues`, `typeValues`, `errorCodeValues` — *because none of them is
declared as one*. This one is declared as one, so it is read off the navigated schema and is
generic.

**One branch, and the trap in it.** `completeKey` gains a test before it calls `legalKeys`: is
the cursor's document path inside a shape slot that carries a declaration? If so, the remainder
below the slot root navigates the declared schema — `SlotAt`'s longest-prefix-then-`Navigate`
pattern ([internal/validation/CLAUDE.md](../internal/validation/CLAUDE.md)), not a new one. The
generated schema cannot absorb this and must not be asked to: "this mapping's keys come from the
value of a sibling key, possibly via a file" is not expressible as a JSON Schema. So it is a
second source beside `processSchema`, not a repair of it — unlike the user-schema nesting, which
was a lossy projection being restored.

**The declaration is read from the resolved definition, never from `Doc`.** A `$process` spread
supplies it with no node in the document's index, and reading a declaration off the text as
written is exactly how three handlers were each found answering about a document nobody applies.
`Doc` gives the cursor its path; `definition()` gives the schema.

**Closedness is what makes the list authoritative.** A list drawn from an open schema is a
suggestion — the author may write anything, and §4's conform would then delete it. §3 refuses
it instead, so the offered list is the complete legal set and the diagnostic catches exactly
what completion failed to prevent.

## 8. Publishing, hiding, and being more specific

A declared `output_schema` is what `$process` spreads and what the comparison reads (§9). That
is the "public API" property, and §4 is what makes it honest — the value is conformed to the
declaration, so publishing it is a statement about what left rather than about what was meant.

**Hiding is still refused, and now for a better reason.** A declaration that omitted keys the
output produces would let a process expose `{status}` while computing `{status, debug}`. The
conform would happily strip them. §3 refuses the undeclared key at registration instead, so the
author cannot write a key their declaration does not name. The first draft argued hiding was
*impossible*; with a conform it is mechanically easy, and the answer is that we decline to
delete what someone wrote. Admitting `additionalProperties` (§3's count) is what would reopen
it, and it should be reopened deliberately rather than as a side effect.

**Being more specific is the real limit, and it is mostly temporary.** The relation runs
`inferred` against `declared`, so a declaration may widen and may resolve an unknown, and may
**not** narrow: an author who knows a field is `enum: [sent, failed]` where inference says
`string` is refused. Two things to say about that.

What a declaration can already add is the part a public API most needs and inference cannot
produce at all: `description` on every property, stable names, and the optionality the author
means rather than the one that fell out of a `??` chain.

The second argument is §4's and is independent of taste: a narrowing declaration is a claim
the value side cannot prove, so the conform behind it would have a **legitimate** failure and
would stop being an assertion. Every narrowing declaration is a runtime failure genroc could
not have told the author about at registration.

And the appetite to narrow is **mostly literal types**.
[literal-types.md](literal-types.md) is the doc that would supply `enum: [sent]` by inference,
at which point the declaration no longer needs to narrow to say it. So the ordering is: do not
weaken this relation to buy what another change supplies properly. The alternative — accept any
declaration not provably disjoint and let the conform be the only check — trades registration
failure for the failure §4 already calls the expensive one, a process that runs to completion
and then cannot deliver.

## 9. The seams, and what is silent when broken

- **`Shape` picks the relation.** `CheckWith` calls `norm.IsSubset(*s.Schema)`
  ([infer.go:147](../internal/shape/infer.go#L147)); the slot selects the mode instead. The
  existing fixed targets (`headers`, `query`, `accepted_status`) keep the open relation and no
  conform — they are `object<string>` and friends, where undeclared is the normal case.
  Flipping one of those is a behaviour change to a shipped slot and is not part of this.
- **The version comparison is NOT untouched, and this is the reversal to read twice.** The
  published type of a slot becomes the declared schema where one exists, so `Compare` reads it.
  Adding a declaration therefore *changes* what a process publishes: since `inferred` fits
  `declared` and not the reverse, the published type **widens**, and compat-command.md's
  direction rule says what we produce may only narrow. So adding a declaration is a reportable
  contract event, once, correctly — a parent whose `result_schema` was narrower than the new
  declaration really does stop fitting. After that the process is free to refactor inside it,
  which is the whole trade the feature buys.
- **The upgrade gate's floor rule still binds.** Nothing here may turn a tolerable verdict into
  a refusal ([internal/validation/CLAUDE.md](../internal/validation/CLAUDE.md)). A declared
  slot makes the *stored* data more precisely described, which is the direction that helps —
  but `IsSubsetAsStored` is the relation reading it, and §4 already has that relation under the
  microscope. The two must be settled together.
- **The editor schema is per variant.** `actionSchemaTemplate`
  ([definition.go:198](../internal/model/definition.go#L198)) makes each action variant
  `additionalProperties: false`, so a new key absent from a variant is refused by the editor
  while the server accepts it — the reverse of the usual skew and just as confusing. One entry
  per variant, and `output_schema` on the task and definition schemas.
- **Diagnostics point at the shape, not the schema.** A closed break is the *shape* naming a
  key, so it is reported at the shape's existing slot address with `inField` naming the
  sub-field. A malformed declaration is the other case and reports at the `_schema` slot.
  Getting this backwards underlines the imported document when the call site is what is wrong.
- **`genctl schema type` prints the published type**, which is the declaration where there is
  one and the inferred type otherwise — the same rule `$process` and `Compare` follow, because
  three answers to "what is this slot" is how they drift. The inferred type stays reachable and
  is what a diagnostic about the shape is phrased against.

## 10. Tests

Go for the relation and the conform, because they are pure algorithms over schemas with no
endpoint behind them. The `closed` mode as a table in `schematest/`, one row per shape the walk
descends — a union arm, an array item, behind a `$ref`, inside a recursive definition, an open
map — asserting both the verdict and that every false yields a complete break, as
`assertSubset` requires of every other relation.

Then the pairing, which is the test that decides §4: every gap the conform closes accepted by
the relation and every gap it refuses rejected, in both directions, with the conformed value
passing a **strict** re-check afterwards. That is the shape `absent_test.go` and
`conform_exact_test.go` already use, and the new combination must earn its own copy rather than
borrow their confidence.

End to end in `tests/cli/`, mirroring `spread_test.ts`. Five run **offline** (`GENROC_SERVER`
at a dead port), and that is the claim worth pinning, because reporting with no server is the
feature:

1. a `body_schema` catching a misspelled key, and the same definition passing once fixed
2. a child `input_schema` written by hand, catching a misspelled key in the `input` beside it
3. `output_schema` on a process, refused for a missing required key
4. `additionalProperties` in a declared schema, refused by name
5. a `query_schema` declaring a non-scalar, refused as a declaration rather than as a shape

Two need a server, and are the other half of the child slot: a hand-written `input_schema` that
does not fit the registered child is refused at registration, and a `$process` spread fills the
same slot so that it does.

**The assertion needs a test that it is one.** A property test over the pairing: generate a
value of the inferred type, and assert the conform against a declaration the relation accepted
neither fails nor strips a key. Failing or stripping is the type-system bug §4 says cannot
happen, and without this the claim is a comment. It is the same test that catches the open-map
hole in §3, from the other side.

**The null repair needs a running instance, so it is an e2e test and not a CLI one**: a process
whose output expression yields null in an optional non-nullable slot, asserting the stored
output has **no such key** rather than a null one. Its mirror is the case that must still fail
— the same null in a *required* slot — and the one that must not fire, a target that is itself
nullable, where the null is kept. Without all three the repair passes by doing nothing.

Each must be checked to **bite** — deleting the feature must fail it — which for the first five
means asserting the error arrives *without* a server, not merely that it arrives.

In `tests/lsp/`, where the rule is that **every bug a real user found was at a position nobody
picked**. So the fixture gains a task carrying a declared schema — carefully, since it is
load-bearing and adding a task to it once made 17 tests ambiguous — and the sweep covers it,
asserting the completion KIND as it does everywhere else. Two positions are worth naming by
hand: a key inside a declared `body`, which must answer `Property` where it answers nothing
today, and the same key when the declaration arrived by `$process` spread rather than being
written, which is §7's `Doc`-versus-`definition()` trap.

The differential sweeps need a second look rather than a new case. They rest on a mapping being
an open map of the author's own names, and a declared schema is exactly what stops one being
that — so "pressing Enter adds no key and removes none" must still hold in a slot that now has
keys to offer.

## 11. Open

- **`genctl schema type` reaches only `input` and `output`.** So the published-type rule (§9)
  is observable at the process output and nowhere else; the action addresses in
  specs/schema-command.md §2 are still proposal, and that is where the rest of it lands.
  Nothing here waits on it — the editor answers those positions already.
- `headers_schema`, when an importer's request side exists to fill it (§2).
- `additionalProperties`, and with it hiding (§3, §8). Count first.
- Narrowing declarations, if [literal-types.md](literal-types.md) does not remove the appetite
  (§8). The measurement that decides it is how many real declarations want to say something
  inference will never produce once literals are inferred.
- Whether `$process` spreading `input_schema` should be a **required** part of the spread or an
  entry an author may drop. It is the only one of the four that can be checked against its
  source, so dropping it is more visibly a choice than dropping `raises`.
- Filtering EXPRESSION completion by the declared target type. Tempting and declined for now:
  the scope view is one thing everywhere, and a slot that hides a legal read is worse than one
  that offers a wrong one, which the diagnostic catches anyway.
- A code action filling every missing required key from the declaration. The obvious companion
  to an import; out of scope because it is an edit, and every answer in §7 is a read.

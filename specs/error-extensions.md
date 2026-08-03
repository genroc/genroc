# Process error model: considered extensions

Status: **open discussion, none accepted, none scheduled (2026-07-24).** Extends
the shipped design in [child-error-handling.md](child-error-handling.md) — read
that first; the vocabulary (raise, panic, defect, batch, slot, raise set) and the
invariants (I1–I6) are all from there.

Three gaps were identified while reviewing the error model; two of them turned
out to have a second candidate shape, so there are six entries below. None was
built. This doc records what each one is, the case for and against, and the
signal that would justify revisiting it — so that the next person to hit one of
these does not re-derive the argument from scratch, and so that "we didn't think
of it" is never confused with "we thought about it and declined".

Nothing here is a commitment. Each section ends with a **trigger**: the concrete
observation that should reopen it.

---

## X1 — Routing on batch shape

**The gap.** Only `raised[0]` in slot order routes ([collect.go:26](../internal/engine/collect.go#L26)).
Fan out over 100 items; 40 raise `rate_limited` and one raises `invalid_input` in
slot 0. The parent routes on `invalid_input` and never learns about the 40. Given
I1 (all-or-nothing output), the 59 successes are unreachable too.

The parent's available actions after a raise are: fail, raise, or `goto` — and a
`goto` back to the spawning task re-spawns the *whole* batch (§10.1). So the
distinction that matters is:

- **all raised the same transient code** → back off and re-spawn; it will likely work.
- **mixed** → one item is genuinely bad; re-spawning burns the other 99 for nothing.

That branch is not expressible today.

**Shape considered.** A quantifier on the *match*, not a payload — no data enters
the parent's context:

```yaml
on_error:
  - code: [rate_limited]
    when: all          # every raised child matched this rule
    goto: $backoff
  - code: [rate_limited]
    goto: $give_up     # some did, some didn't
```

`raised[0]` still selects which rule is consulted; `when` only decides whether it
fires.

**For**

- Zero type cost. No new context slot, no schema change, `Raises()` and R5
  untouched. This is the whole reason to prefer a predicate over exposing
  siblings.
- The distinction is real, not hypothetical — see the two bullets above.
- Additive: no definition written today changes meaning when it lands.

**Against**

- **Adjacent to something already rejected.** D2 removed the `siblings` aggregate
  from `$error` on the grounds that the engine never routes on it. A quantifier
  is the narrow form of the same idea, and shipping it makes "and now let me
  *see* the siblings" the obvious next request.
- **Thresholds are the natural next ask** (`when: ">50%"`), and a count *is* a
  value — the exact thing X1 is designed to avoid. The line between `all`/`any`
  and a threshold has to be held deliberately, and it is not self-evident to
  someone reading the feature.
- **Rule matching gains a second dimension.** Today "which rule fires" is one
  ordered pattern match. With `when` it becomes code × quantifier, which is
  harder to reason about and harder to explain in the same breath as switch.
- No demonstrated demand. The case above is constructed, not observed.

**Trigger.** Someone writing a real fan-out asks for it, or works around its
absence by abandoning the error channel for `{ok: false}` outputs *and* finds
that unsatisfying. Until then the workaround is documented and adequate.

### X1-b — re-spawn only the raised slots

Separable from the quantifier, and the more valuable half. Today the only way to
re-run a batch is a `goto` back to the spawning task (§10.1), which re-spawns
**all** slots — so recovering 40 failures costs re-executing the 60 that already
succeeded. A partial re-spawn would keep completed slots' outputs and re-run only
the raised ones.

**This is D7's deferred feature, arriving from the other direction.** D7 rejected
parent-side retry partly on semantics (a raise is a *settled* condition, §0) but
mostly on cost: it "would have touched the concurrency-sensitive sibling queries —
a per-slot attempt dimension (`_spawn_attempt`, live-attempt selection,
`CountActiveSiblings`), which sits inside the lock-ordering discipline that
prevents deadlocks" ([db_lifecycle.go:27-30](../internal/db/db_lifecycle.go#L27-L30)).
That costing still stands, and it makes X1-b **by far the most expensive item in
this document** — everything else here is validation or presentation; this one
touches the queries the deadlock discipline protects.

D7 also named its own trigger: the §10.1 workaround "doing it repeatedly is the
signal to build the real thing." A fan-out large enough for the waste to matter
is exactly that signal.

**For**

- Removes the cost that makes X1's `mixed` branch painful. Re-running 40 of 100
  is a different proposition from re-running 100.
- **I1 survives in its final form** — batch output is still written only when
  every slot is `completed`. What changes is that a slot may reach `completed`
  across several attempts rather than one.
- Purely additive, as D7 already established: no definition written under R4
  changes meaning when it lands.

**Against**

- The cost above. This is a schema and concurrency change, not a validation rule.
- **It argues against §0's framing.** A raise is defined as a settled condition
  the child already gave up on; re-spawning it says the parent knows better.
  Defensible for `rate_limited` (the child's retry budget was too small for the
  parent's patience) but it does blur a line the design drew deliberately.
- Needs a per-slot answer to "which failures are retryable" — mixed batches raise
  several codes, and re-spawning a slot that raised `invalid_input` is pointless.
  That interacts with X1's rule matching and neither feature fully specifies it.

**Relationship to X1.** They are complements, but X1-b **lowers X1's urgency**:
much of the quantifier's value is avoiding a wasteful full re-spawn, and if the
re-spawn is cheap, getting the `all`-vs-`mixed` call wrong costs much less. If
only one is built, X1-b is the one that matters.

---

## X2 — A diagnostic payload on `raise`

**The gap.** A raise carries a code and a message and nothing else (I6). A raising
child computes no output at all — a raise is not an `end`
([error.go:141](../internal/engine/error.go#L141)) — so there is no side channel
either. `card_declined` plus `{decline_code: "51", retry_after: 3600}` has
nowhere to go, and the pressure is to stuff it into the message prose.

**Why the unrestricted version is expensive.** Making `detail` typed and readable
by the parent does not add a second output to a process — it adds **one variant
per raise code**, discriminated by `error_code`. `Raises() []string`
([definition.go:281](../internal/model/definition.go#L281)) becomes
`map[string]Schema`, and everything follows: the definition endpoint publishes
shapes, R5 must union the shapes a pattern spans (`order_%` matching three codes
means `$error.detail` is a union of three), and `internal/schema`'s join/subset
machinery becomes load-bearing on the error path.

Rust does exactly this — `Result<T, E>` with an enum `E` carrying payloads — and
it is safe there **because `match` is exhaustive**. D3 declines exhaustiveness,
so an unrestricted version would be variant payloads without the check that makes
them sound: a parent reads `detail.decline_code`, the child's next version
reshapes it, and nothing catches it at registration. Version pinning bounds the
blast radius; it does not close the hole.

Two shapes get around that, at different cost and with different reach.

### X2-a — operator-facing only

A payload that never enters the parent's context.

- Lands on the raising child's row; visible on instance detail, logs, the API.
- `$error` in the parent stays exactly `{task, message, code, child_key}`.
- Not matchable in an `on_error` pattern, and not readable by any expression.

**For**

- **I6 survives literally** — no data crosses into the parent's context, which is
  what the invariant actually says.
- `Raises()` stays `[]string`; no variants, no schema machinery on the error path.
- **Sharpens §0 rather than blurring it**: diagnostics are for humans, data for
  branching is `output`. Today the two are conflated because prose is the only
  outlet.
- Storage is not the constraint — the row already carries an `error_data` JSON
  column ([db_instances.go:118](../internal/db/db_instances.go#L118)).
- Strictly additive; X2-b remains open on top of it.

**Against**

- **It does not solve the motivating case it is named after.** `retry_after`
  reaching a parent that wants to schedule around it stays impossible.
- **Two places to look for "what went wrong."** Authors will be unsure whether
  something belongs in `message` or `detail`.
- **It weakens I6 in spirit if not in letter.** "The dashboard can see it but the
  parent cannot" invites the question of why, and the honest answer is a statement
  about our implementation, not about the domain.
- **It raises the pressure it was meant to relieve.** Once the payload exists and
  is visibly useful, "let the parent read it" is a much easier ask.

### X2-b — parent-readable, gated on an exact code match

> **Superseded (2026-07-24) — not merely deferred.** Unlike everything else in
> this document, X2-b is **rejected on principle**, not held pending a trigger:
> data flows through the success path, and the error channel is for signalling.
> A raised condition that carries data is not an error — it is *a different shape
> of output*, and the answer is to make the success channel able to express that.
> See [the replacement direction](#the-replacement-direction-union-outputs) below.
> The analysis is kept because the exact-match gate is a genuinely good idea that
> would apply again if this is ever reopened, and because the reason it was
> declined is more useful than the fact.

The restriction that makes the typed version tractable: **`detail` is readable
only by a rule whose `code` is a single exact literal — no `%`, no multi-code
list, no catch-all.**

```yaml
on_error:
  - code: [card_declined]        # exact, single → $error.detail available
    detail_schema: { … }
    goto: $offer_alternative
  - code: [psp_%]                # pattern → $error.detail not in scope
    goto: $fallback
```

That kills the union-across-a-wildcard problem outright. What remains is a
**smaller and tractable** union, and it must be stated plainly rather than
waved past:

- one child can raise the same code at several sites, each with a different
  payload shape;
- in a `child_map`, several children can raise the same code with different
  shapes.

So `detail` for a given code is still a join over raise sites × children of that
task — but it is a join `internal/schema` can already compute, over a set that is
finite and statically known.

**Two cost tiers, and the cheaper one comes first:**

| | how the shape is known | cost |
|---|---|---|
| **declared** | parent writes `detail_schema`, runtime-checked | low — mirrors `result_schema` on a child entry; `Raises()` unchanged; this is exactly the `unknown` top type of [unknown-type.md](unknown-type.md), narrowed by the consumer |
| **inferred** | derived from the child's raise sites | high — `Raises()` must carry shapes and propagate through R5/R6, the definition endpoint and the published schema |

Declared-first is the phasing that falls out: it needs no inference at all, and
inference can land later on the same machinery planned for child `result_schema`,
at which point `detail_schema` becomes optional rather than required.

**For**

- Actually solves the motivating case, which X2-a does not.
- The exact-match gate is a **structural** restriction, not a documented
  convention — a generic handler cannot harvest detail across codes.
- The declared tier is cheap and consistent with a direction already planned.
- Consistent with how the system already treats opaque external data: `fetch`-style
  narrowing is the established pattern, not a new concept.

**Against**

- **It reverses R2's reasoning for the message half.** R2 forbids a computed
  `message` because it "would smuggle data across the boundary" — and `detail` is
  exactly that smuggling, sanctioned and typed. The code half of R2 still stands
  (codes stay literal), but the boundary argument does not survive intact, and
  saying so is more honest than claiming the invariant is untouched.
- The residual join (sites × children) is real work even at the declared tier, if
  registration is to check the parent's `detail_schema` against what children can
  actually produce. Skipping that check makes it purely runtime — cheaper, and
  weaker.
- **The overuse risk below**, which is the strongest objection and is not
  technical.

### The overuse question

The concern, stated as sharply as it deserves: **today §0's "an error is a branch
slot, not a value" is enforced by capability — a raise physically cannot carry
data. X2-b makes it enforced only by documentation.** That is a real downgrade in
kind, not merely in degree, and it is the reason this is not a straightforward
yes.

Once `raise` can carry typed data, it is strictly more expressive than a
`{ok: false, reason}` output for any control-flow-with-data case, and more
ergonomic: one clause, automatic routing, and a code that is already filterable
in every dashboard. Authors will notice.

Three things bound it structurally rather than by convention:

- **I1 blocks the most tempting misuse.** "8 of 10 shipped, here is why 2 didn't"
  cannot move to the error channel, because a raising child contributes no output
  and a batch with any raise produces none — so the 8 successes are lost either
  way. The canonical §0 example stays impossible.
- **A raise forfeits `output` entirely** ([error.go:141](../internal/engine/error.go#L141)).
  Using it for a normal outcome costs the whole result channel, so `detail` is not
  additive to output — it replaces it.
- **`raised` is terminal and non-retryable (D4).** A flow routed through raise
  gives up retry, which is a real and permanent disincentive.

A fourth is available if wanted, and would be the cheapest structural guard:
**cap `detail` size** (a few KB). A diagnostic fits; a result set does not. That
converts "please don't use this as a data channel" from documentation back into
capability, which is where §0's line currently lives.

The residual exposure after all four is single-child flows, where a legitimate
`{ok: false}` output could migrate to a raise. That is a narrower surface than the
concern's first framing suggests — but it is not zero, and there is no proposed
mechanism that closes it.

**Trigger.** For X2-a: repeated instances of structured data smuggled into
`message` prose — grep for it before building anything. X2-b has no trigger; it
is closed, see below.

### The replacement direction: union outputs

The reason authors reach for data-in-errors is that the **success** channel
cannot express *"one of several shaped outcomes"* — so the error channel gets
abused as a poor man's tagged union. X2-b answers that by building a second typed
channel, with its own narrowing rule (the exact-match gate), its own schema
plumbing (`Raises()` carrying shapes) and its own invariant erosion (I6, R2's
spirit). The alternative adds narrowing to the channel that **already** carries
typed data:

```yaml
# not a raise — a completed process with a union output
output: { type: declined, decline_code: "51", retry_after: 3600 }
```

and the parent narrows on `.type`. One mechanism instead of two.

This also changes *why* §0's line holds. Today it holds because a raise
physically cannot carry data — a wall. With expressive union outputs nobody wants
to climb it, which is a better guarantee than either a wall or a convention.

**State of play — this is an increment, not a new subsystem.** Flow-sensitive
narrowing already exists: [`narrowCondition`](../internal/schema/infer.go#L510)
narrows on `==`/`!=` against a literal, installs guards by dot-path via
`withGuard`, and strips null on the negative branch, with dedicated tests
(`union_narrowing_test.go`, `union_literal_narrowing_test.go`). Union accessors
`IsUnion()` / `Variants()` / `Enum()` are in
[accessors.go](../internal/schema/accessors.go#L72). Three gaps, increasing in cost:

1. **Sibling narrowing via a discriminant.** `result.type == "type1"` currently
   narrows `result.type` and nothing else. Needed: when the subject path is
   `X.disc` and `X` is a union, also guard **`X`**, narrowed to the variants whose
   `disc` accepts the literal — then `result.data` follows for free. Contained;
   `nodePath` already decomposes member chains.
2. **Narrowing across `&&` / `||`.** `narrowCondition` has one call site
   ([infer.go:488](../internal/schema/infer.go#L488)), inside the ternary — so
   `cond ? a : b` narrows and `cond && expr` does not. Small.
3. **Narrowing across a switch case into the target task.** Flow-sensitive typing
   over the task graph: a task reachable from two branches needs a joined context
   type. Genuinely large; hold until 1 and 2 prove the idea.

(1) + (2) cover the motivating case. Note that if (1) lands, **no ascription
syntax is needed** — the discriminant test *is* the narrowing, as in TypeScript's
`if (r.type === "a") { r.data }`. An explicit ascription form solves a different
and smaller problem: pinning a type where inference cannot reach.

**Status: deferred with everything else** (2026-07-24), on the grounds that the
system is a prototype and the narrowing patterns worth supporting should be drawn
from real definitions rather than guessed. Note the asymmetry with the rest of
this document, though: the X-items are additive, so deferring them is free.
Inference is **not** — once definitions rely on a narrowing rule, that rule
cannot be made less generous, and the *shape* chosen for it (which discriminants
count, whether it reaches through arrays) is close to permanent. Deferring costs
nothing; getting it wrong early would.

---

## X3 — Opt-in exhaustiveness over a child's raise set

**The gap.** R5 checks one direction only: every rule *can* fire. Nothing checks
that every raisable code *has* a rule. §3.1's third row is the cost — a code
added to a child, with no rule for it, surfaces at runtime.

**The motivation is not strictness.** It is change notification: *"I have taken a
dependency on this child's exact raise set; tell me if it drifts."* That framing
is what makes opt-in the correct shape rather than a compromise — it is closer to
a lockfile than to a linter, and most parents genuinely do not want it.

**Shape considered.** A flag on the **child entry**, not the task:

```yaml
action:
  type: child_map
  children:
    charge: { name: charge-card, exhaustive: true, ... }
    ship:   { name: ship-order, ... }
```

Rules stay on the task. The check reads: *every code in `raises(charge-card)` is
matched by some rule on this task* — the same `MatchCode` loop as R5, run in the
other direction, over sets [validate_children.go:97](../internal/validation/validate_children.go#L97)
already computes.

**Entry-level, not task-level, is the load-bearing detail.** A task-level flag
asserts over the *union* across entries, so a `child_map` with five children
subscribes you to changes in all five — including ones you never route on. The
noise grows with batch size, and you would re-register because `ship-order` added
a code while you only ever cared about `charge-card`. That makes the flag
unpleasant on exactly the tasks where it looks most useful. `child_list` has a
single child definition, so it degenerates correctly either way.

**For**

- Opt-in is right for a change-subscription, and D3 is untouched for everyone who
  does not opt in — a shared child adding a raise site still breaks nobody by
  default.
- **No rule-level syntax at all.** One boolean. The `panic: true` / `code: []`
  marker business below exists only if the check is on by default; opt-in removes
  the need for it, and with it the `Fault | true` anyOf and the hand-written
  wire-format work (§7.2).
- Reversible and additive.

**Against**

- **It only helps people already careful enough to opt in** — the classic failure
  of opt-in strictness. It does not catch the author who never considered the
  question, which is the population that most needs catching.
- **Permanent schema surface for undemonstrated demand.** Nobody has asked for it.
- **A cheaper alternative may dominate.** `raises(D)` is already published per
  version on the definition endpoint
  ([handlers_definitions.go:53](../internal/api/handlers_definitions.go#L53)), so
  "did this child's raise set change" is answerable *today* by diffing two
  versions — a `genctl` command plus a CI step, zero engine surface, zero schema.
  Weaker (it lives outside the system, and only helps teams that wire it up), but
  it should be compared against before committing syntax.

**Why opt-in is acceptable here at all:** the default is loud. An unhandled raise
fails the parent carrying the child's own code and a message naming the child and
slot ([collect.go:37](../internal/engine/collect.go#L37)). §3.1's third row is a
visible runtime failure with a precise diagnostic, not a silent wrong answer. If
the fallback were silent, opt-in would not be defensible.

**Trigger.** A team reports a production surprise from §3.1's third row — a child
added a code and a parent found out the hard way. One occurrence is anecdote; two
is a signal.

### X3-alt — required catch-all (considered and rejected)

Seriously considered, and worth recording because it was rejected on judgement,
not on a defect. The rule: for a child task that has *at least one* `on_error`
rule, if any code in the child's raise set is uncovered, require an explicit
catch-all saying what happens — where a verb-less catch-all means "the rest are
defects".

This is the `switch` rule, which already requires a catch-all last case
([validate.go:254](../internal/model/validate.go#L254)), made conditional on
coverage. The precedent is good: Rust's `_ => {}` is behaviourally identical to
what a non-exhaustive match would do, and is required anyway — forcing the author
to acknowledge the fallthrough is the point, so "the marker is a no-op" is not an
objection.

**For**

- Catches the careless author, which X3 does not.
- **It threads D3's needle.** D3 rejected exhaustiveness because the cost was
  unbounded: any new raise site breaks every parent, and fixing it means genuinely
  handling the new code. Here the fix is one line, and only for parents that
  already have rules. Bounded and mechanical.
- The opt-out marker **already exists** and already has the right semantics: on a
  child task, a catch-all with none of `goto`/`raise`/`panic` is legal (R3's
  at-most-one form permits zero,
  [validate.go:286-295](../internal/model/validate.go#L286-L295)) and
  `resolveRaisedBatch` treats it identically to no rule at all
  ([collect.go:33](../internal/engine/collect.go#L33)). So the check needs no new
  syntax whatsoever.

**Against — and these carried**

- **It is a breaking change** to every existing definition with partial rules,
  which is the constraint D3 set for itself. Survivable warn-first, but real.
- **It cannot be uniform with `switch`.** R5 is child-only because the raise set
  is finite; an action task's `on_error` matches an open engine-code space with
  nothing to be exhaustive over. So the rule would apply to `switch` everywhere
  but to `on_error` only on child tasks — losing the consistency the analogy was
  buying.
- **The syntax is unpleasant.** `code: []` reads as an unfinished edit;
  `panic: true` makes `panic` a `Fault | true` union with a meaningless `false`,
  and costs hand-written wire-format work (§7.2). `goto: panic` was also
  considered and is worse: §0 maps each *field* to an outcome (`goto` → routed or
  `completed`, `raise` → `raised`, `panic` → `failed`), and putting a defect in
  the `goto` slot breaks that reading. It would also add a third reserved bare
  word to a namespace where the `$` sigil is stripped on decode
  ([wire.go:199](../internal/model/wire.go#L199)), so a task named `panic`
  written `$panic` collides with the keyword — the same latent sharp edge `end`
  already has.
- **On-by-default is the wrong default** for something most parents do not want,
  which is the argument that decided it in favour of X3's framing.

---

## Summary

| | what it adds | main argument for | main argument against |
|---|---|---|---|
| **X1** | `when: all` quantifier on a rule | real branch, zero type cost | adjacent to rejected D2; thresholds are a slope |
| **X1-b** | re-spawn only the raised slots | removes the waste that makes X1 matter | D7's cost: per-slot attempts inside the deadlock discipline |
| **X2-a** | operator-only raise detail | I6 survives; sharpens §0 | narrower than its own example; raises pressure to widen it |
| **X2-b** | parent-readable detail, exact-match gated | ~~solves the real case~~ | **closed** — data belongs in the success path; superseded by union outputs |
| **X3** | per-entry `exhaustive: true` | right shape for a change-subscription | only helps the already-careful; a CLI diff may dominate |
| **X3-alt** | required catch-all | catches the careless author | breaking, non-uniform, unpleasant syntax |

None is blocking, and all are additive — declining now costs nothing later.

One relationship is worth carrying forward: **X1-b lowers X1's urgency** — cheap
partial re-spawn makes the `all`-vs-`mixed` call much less consequential, so if
only one is ever built it should be X1-b.

**X2-b is closed**, and its closure is the most useful conclusion here. The
principle that settled it — *data flows through the success path; the error
channel signals* — is worth applying to anything that looks like the next X2-b.
A raised condition carrying data is not an error with a payload; it is a
differently-shaped output, and the work it implies belongs in the type system
rather than the error model. That work is deferred too, but for the opposite
reason: not because it lacks a trigger, but because inference rules are close to
permanent once definitions depend on them, and the patterns worth supporting
should be read off real usage rather than guessed.

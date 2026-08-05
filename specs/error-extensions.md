# Process error model: considered extensions

Status: **open discussion, none accepted, none scheduled (2026-07-24).** Extends
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

## X2 — A diagnostic payload on `raise`

**Gap.** A raise carries code + message only (I6); a raising child computes no output.
`card_declined` plus `{decline_code: "51", retry_after: 3600}` has nowhere to go but
message prose.

**Why the unrestricted version is expensive:** typed parent-readable `detail` is one
variant per raise code, discriminated by `error_code` — `Raises()` becomes
`map[string]Schema`, R5 must union shapes across wildcard patterns, and the schema
machinery becomes load-bearing on the error path. Result-enum-with-payloads is sound in
languages whose match is exhaustive; D3 declines exhaustiveness, so here it would be
variants without the check that makes them sound.

### X2-a — operator-facing only

Payload on the raising child's row (instance detail, logs, API); `$error` in the parent
unchanged; not matchable, not readable by expressions.

**For:** I6 survives literally; no schema machinery (`error_data` column already
exists); sharpens §0 — diagnostics for humans, data for branching is `output`.
**Against:** does not solve its own motivating case (`retry_after` still cannot reach a
scheduling parent); two places to look for "what went wrong"; makes "let the parent
read it" an easier next ask.

**Trigger.** Repeated structured data smuggled into `message` prose — grep before
building.

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
**Against:** reverses R2's boundary argument for the message half (sanctioned, typed
smuggling); the declared-tier registration check is real work; and the overuse risk:

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
| **X2-b** | typed detail, exact-gated | ~~solves the case~~ | **closed** — data belongs in the success path |
| **X3** | per-entry `exhaustive: true` | right shape for a subscription | helps only the careful; CLI diff may dominate |
| **X3-alt** | required catch-all | catches the careless | breaking, non-uniform, unpleasant syntax |

Carry forward: X1-b over X1 if either; and X2-b's closing principle — *data flows
through the success path* — applies to whatever looks like the next X2-b.

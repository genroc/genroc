# Process error model: considered extensions

Status: **X2 built (§X2-c); X1 and X3 are open.** Extends
[child-error-handling.md](child-error-handling.md), whose vocabulary (raise, panic, defect,
batch, slot, raise set) and invariants (I1–I6) apply throughout. An open entry records the case
both ways and the **trigger** that should reopen it.

## X1-b — re-spawn only the raised slots

Built as child-error-handling.md §5.5. The attempt count rides the child's `_spawn_*`
bookkeeping, so the sibling queries gain neither column nor predicate, and `on_error` itself
answers which codes retry. It does not consult batch shape, so X1 is unaffected.

## X2 — A payload on `raise`

Built as §X2-c. A raise carried code and message only, so a structured value (`card_declined`
with `{decline_code, retry_after}`) had nowhere to go but message prose.

### X2-c — parent-readable, caller-declared

**The gap is created by reuse.** A generic wrapper (a reusable script child) cannot mint a
caller-specific code — codes are literals (R2) — so a discriminator like `error.data.name` dies
at the process boundary. That mirrors the success path, where the wrapper emits the top type and
the caller narrows with `result_schema` (unknown-type.md); `raises` is the error channel's
counterpart. **The caller declares**, so a generic child stays generic and no payload schema
propagates across child versions.

**The guard is asking by name.** A rule's `case` (child-error-handling.md M2) makes branching on
error data one line, so what keeps codes the path of least resistance is that `error.data` is
readable only for codes the caller declared under `raises` and its rule names.

#### Syntax

`data` on `Fault` is a Shape (an expression or an object of expressions), evaluated in the scope
the `message` renders in, before the clause concludes — so an `on_error` rule can forward the
error it caught. It is never degraded like a message: a payload that will not evaluate fails the
instance (`engine.expression`). Named `data` because it is read as `error.data`, the only place
it is read.

**The slot is on `Fault`, so `panic` carries it too** — raise and panic differ in what they do,
not what they carry. A panic code is excluded from `raises(D)`, so nothing types it and only an
operator reads it (instance row, logs, API). **A panic's data stays on the instance that
authored it**: poisoned ancestors inherit its code and message, not the payload.

The caller declares shapes in a `raises` table on the **action** (per entry on a `child_map`),
keyed by code — like a fetch's `responses`, it describes what the call can hand back, not what a
rule does about it, so one declaration serves every rule and two rules cannot disagree. A key
the child never raises is refused (`checkDeclaredRaises`). Four states:

- **absent** → `error.data` is absent; undeclared data is never accessible;
- **`null`** → declared, carrying no payload. On an `external` task the keys are the closed set a
  worker may submit, which is why a payload-less code needs a declaration
  ([external-task-queue.md](external-task-queue.md));
- **`{}`** → the unknown type: present, narrow it;
- **a schema** → typed and navigable.

A rule catching several declared codes sees their `anyOf`; one that can also catch an undeclared
code gets `| null`. The union across a rule's patterns is the machinery fetch `responses`
already used (`errorDataSchema`, `ruleErrorData`, `ruleCatches`).

#### Two errors, one per direction

| | storage | is | read by |
|---|---|---|---|
| **inbound** | `error_internal` (context `last_error`) | the failure that ROUTED it to the task it sits on | its own expressions |
| **outbound** | `error_code`, `error_message`, `error_data` | the error it CONCLUDED with | its parent, where the call declares the code; an operator |

**A concluding fault never edits the inbound error.** On a task reached through `on_error`,
`last_error` is part of the state that task's layer describes and an upgrade validates against;
a fault editing it leaves a context no layer admits (a panic that cleared `error.data` once made
`genctl upgrade` refuse its row).

**The outbound error is three plain columns.** `error_code` is filtered on (`?error_code=`), and
a code inside a blob can be neither indexed nor matched in SQL; only the payload, being
arbitrarily large, gets a value column with the object-store cut. `error_data` is stored absent
where the clause carried none, is written only at completion (like `output`), and is cleared by
`RetryProcess`. Its context key is `_error_data`: nothing an author writes reads it, and `error`
/ `last_error` already name the inbound direction. On the wire the columns keep their names —
`error_code` and `error_message` everywhere, `error_data` only on single-instance responses (a
list row omits the one field that can be large) — so the field a caller filters on is the field it
reads back. The inbound error is not a field: it
stays in `context` under `last_error`.

#### A mismatch is `result.invalid`, on both channels

A raised `data` that does not satisfy the caller's declaration reports **`result.invalid`** in
place of the raised code, catchable on the child task; a rule matching the original code no
longer fires (as a fetch body failing its declared schema stops `http.4%` catching it).

**The success path matches**: a child output failing the caller's `result_schema` is also
`result.invalid`, not `engine.collect`. When a generic wrapper forwards an unknown and a caller
narrows it, the caller is making a *bet* about a shape neither definition states, and the bet can
lose with both definitions consistent — not a defect. The runtime conform is the only gate there:
the static check passes by construction for exactly this case. The other collect failures — a
non-`completed` sibling, a single-child task with ≠1 sibling, a bad `_spawn_index`, object-store
resolution failing — are corruption and stay `engine.collect`. R5 admits `result.invalid` on a
child task (`matchesSomeRaise`), which is why its catchable set is `raises(D) ∪ {result.invalid}`.

**No size cap on `data`.** A process `output` has none, and the object store absorbs size. If one
is ever added it belongs at the raising end, failing the child: truncation at the reading end
cannot satisfy a declared shape.

#### Inferring the child's `data` shapes

The caller still declares, but registration checks the bet the way `checkChildOutputType` checks
`result_schema`: `SchemaFile.Raises` types every code a definition raises, and
`checkDeclaredRaises` runs `NarrowsTo` against the caller's declaration — sound because
`Engine.raisedData` conforms the payload against that same schema. Three rules follow the
runtime:

- **Two clauses on one code are a union** — either may fire.
- **A clause attaching nothing types as `null`**, not absent: the caller conforms `null`, which a
  declared object shape does not admit.
- **Panics contribute nothing**, as `raises(D)` excludes their codes.

What still reaches the runtime conform is a payload whose own type is the top type — a generic
wrapper forwarding an unknown, and a bet that may lose.

## Summary

| | adds | for | against |
|---|---|---|---|
| **X1** | `when: all` quantifier | **open** — real branch, zero type cost | adjacent to rejected D2; threshold slope |
| **X1-b** | partial re-spawn | **built** — child-error-handling.md §5.5 | |
| **X2** | caller-declared `raises` | **built** — §X2-c | costs a caller a declaration per code it reads |
| **X3** | per-entry `exhaustive: true` | **open** — right shape for a subscription | helps only the careful; CLI diff may dominate |
| **X3-alt** | required catch-all | **rejected** | breaking, non-uniform, no good opt-out |

## Open

### X1 — Routing on batch shape

Only `raised[0]` in slot order routes: fan out over 100, 40 raise `rate_limited`, slot 0 raises
`invalid_input`, and the parent never learns of the 40 — "all raised one transient code → back
off and re-spawn" vs "mixed → one item is bad" is not expressible. The shape is a quantifier on
the match, never a payload: `when: all` on a rule (`raised[0]` still selects the rule; `when`
decides whether it fires). Zero type cost and additive, but thresholds (`when: ">50%"`) are the
next ask, and a count is a value — rejected D2's `siblings` aggregate.

**Trigger.** A fan-out author asks for it, or abandons the error channel for `{ok: false}`
outputs and finds that unsatisfying.

### X3 — Opt-in exhaustiveness over a child's raise set

R5 checks only that every rule can fire; a code added to a child with no rule surfaces at
runtime (child-error-handling.md §3.1). The motivation is change *subscription* ("tell me if
this child's raise set drifts"), so opt-in is correct, and the flag goes on the **child entry**:
a task-level flag subscribes to the union across all children, noisiest where it looks most
useful. Opt-in is defensible only because the default is loud — an unhandled raise fails the
parent with the child's code, naming child and slot. A cheaper alternative may dominate:
`raises(D)` is published per version, so a `genctl` diff in CI answers it with no engine surface.

**Trigger.** A team reports a production surprise from an unruled code. Once is anecdote;
twice is a signal.

**X3-alt, a required catch-all, is rejected**: it breaks every definition with partial rules
(D3), cannot be uniform with `switch`, and has no pleasant opt-out spelling.

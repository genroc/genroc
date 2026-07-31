# `delay`: human durations and calendar deadlines

**Status: IMPLEMENTED, 2026-07-31.** `for`, `until` and `tz` are the delay action's slots;
`ms` was **removed outright** rather than deprecated, which is the decision this document
was written to force. The window for that closes at the first release — see
*Compatibility*.

The grammars live in `internal/delayspec` (no engine or DB dependency, so the calendar
edge cases are table-tested in isolation). Line numbers below are as of 2026-07-31 and
refer to the pre-change code where they describe the motivation.

## The idea in one line

A `delay` can only say *"pause N milliseconds"*, spelled as a decimal string — so
everything a human actually wants to say (*in two hours*, *in two days at 08:00 Prague*,
*on the 1st at 08:00*) has to be pre-computed by the caller, and the calendar ones cannot
be computed at all.

## Motivation

`Action.Ms` (`internal/model/definition.go:87`) is a template string, type-checked to
`number | string` at registration (`internal/validation/infer.go:166-168,200-210`),
evaluated once at arm time into `wake_at` (`internal/engine/action.go:120-132`) and
converted by `durationFromValue` (`internal/engine/action.go:224-254`). Three gaps:

**It is unreadable.** `"86400000"` is a day. `"2592000000"` is thirty days — or is it
three hundred? A wrong digit is a silent 10× error that survives review, and there is no
unit to check it against.

**There is no calendar arithmetic.** "the 1st at 08:00" is not a fixed number of
milliseconds from now, and neither is "in two days at 08:00" across a DST boundary. No
amount of arithmetic in the definition expresses it.

**Absolute deadlines are unexpressible at all.** `ms` is relative-only, and the expression
language has no clock: the context roots are `input` / `error` / `outputs`
(`internal/expression/refs.go:135-141`) and the only builtin is `map`. So *"wait until the
deadline the vendor sent"* cannot be written even in principle. This is the same wall the
`Retry-After` open question in [fetch-http-surface.md](fetch-http-surface.md) hits from
the other side.

## Design

Two slots replacing `ms`, and one classification rule that does most of the work.

### The slots

| slot | means | number form |
|---|---|---|
| `for` | a duration, relative to arm time | milliseconds |
| `until` | an instant | unix milliseconds |

Exactly one of `for` / `until`. A bare number means something different in each slot, which
is unambiguous *within* a slot and matches what the slot is named — and together they cover
strictly more than `ms` did. `ms` is exactly `for` with a bare number, which is why it
could be removed rather than aliased.

```yaml
- id: wait
  action: { type: delay, for: "2h30m" }

- id: wait
  action: { type: delay, until: "+2d 08:00", tz: "Europe/Prague" }   # in two days at 8

- id: wait
  action: { type: delay, until: "*-*-01 08:00", tz: "Europe/Prague" } # next 1st at 8

- id: wait
  action: { type: delay, until: "$: outputs.job.deadline_ms" }        # computed instant
```

### The classification rule

The core of the design: **a slot's accepted type depends on how the value is written**,
and that is decidable syntactically, before any inference runs.

| written as | example | treated as |
|---|---|---|
| pure literal | `for: "2h30m"` | the grammar below, parsed at `CheckDoc` |
| `$:` typed expression | `until: "$: outputs.job.deadline_ms"` | must infer to `number` |
| `${ }` interpolation | `for: "${ input.hours }h"` | **rejected** |

This needs no new machinery. `template.Parse` already returns exactly this
discrimination (`internal/template/template.go:88-99`): a leaf whose first non-whitespace
content is an unescaped `$:` sets `Template.expr`; everything else is a template. So the
slot checker branches on the parse, and the internal validator only ever sees a plain
`number` schema — on the one branch that reaches it. There is no *"string if literal,
number if expression"* type to express, which is why this looks like a missing mechanism
and is not one.

**The third row must be explicit, not a fallthrough.** `for: "${ input.hours }h"` produces
a string at *runtime*, which is precisely the failure mode this design exists to remove.
It has to be rejected by name, with a message pointing at the two forms that work.

**Why expressions get numbers only.** The human grammar is *authoring* syntax — it exists
to be typed into a YAML file by a person. A value arriving from `input`, a fetch result or
an external task is machine-produced, and the machine representation of an instant is a
number. Nobody computes a cron expression at runtime either. The payoff is that a literal
is fully static (a typo fails at registration, not three days into a run) and an expression
carries no parse at all.

### The duration grammar (`for`)

`time.ParseDuration`'s grammar extended above `h`: units `ms`, `s`, `m`, `h`, `d`, `w`,
`mo`, `y`, concatenated, whitespace optional — `2h30m`, `90m`, `1d 12h`, `3mo`.

A **unitless string is rejected**: `for: "5000"` errors with *"did you mean 5000ms or
5s?"*, while `for: 5000` (a JSON number) is milliseconds. The asymmetry is deliberate —
the string form is the human one and a bare number there is exactly the ambiguity the
feature is removing.

With `tz` set, `d` / `w` / `mo` / `y` are **calendar** arithmetic in that zone: `1d` means
"the same wall clock tomorrow" (23h or 25h across a DST boundary), because that is what
"in two days at 8" means to a person. Without `tz` they are fixed (`1d` = 24h).

> **Trap.** Go's `time.AddDate` normalizes overflow: Jan 31 + 1 month is **Mar 3**, not
> Feb 28. `mo` and `y` need explicit end-of-month clamping.

### The instant grammar (`until`)

Three closed forms, next match after now, no natural language:

1. **absolute** — RFC 3339, RFC 9557 with the IANA annotation
   (`2026-09-01T08:00:00+02:00[Europe/Prague]`), or relaxed `2026-09-01 08:00`
2. **offset + wall clock** — `+2d 08:00`: two days from now, at 08:00 in `tz`
3. **calendar pattern** — a `systemd` `OnCalendar` subset: `*-*-01 08:00`, `mon 09:00`

### `tz`

IANA names (`Europe/Prague`) or fixed offsets (`+02:00`). **Abbreviations are rejected**:
`CET` silently means the wrong thing for half the year (`CET` vs `CEST`), and Go's
`time.LoadLocation` on an abbreviation is platform-dependent. Literal-only in v1.

Never fall back to the worker's local zone — the same definition must resolve identically
on every worker in the fleet.

### Why not natural language

`chrono-node`, `dateparser` and `olebedev/when` parse "in two days at 8" literally, and are
the wrong tool for a definition that is stored, versioned and replayed: locale-dependent,
ambiguous ("next Friday" *on* a Friday), and a parser upgrade silently changes the meaning
of rows already in the database. The three closed forms above cover every motivating
example with exactly one parse each.

### Prior art

| Format | Taken |
|---|---|
| ISO 8601 duration (`PT2H30M`) | the calendar-unit set; rejected as the surface — nobody enjoys writing it |
| Go `time.ParseDuration` | the grammar for `ms`…`h`, extended above `h` (Go stops at `h` deliberately, because `d` is calendar-ambiguous — hence `tz`) |
| `systemd.time(7)` | the calendar-pattern form and `+2d`-style relative stamps; a subset, not the whole grammar |
| RFC 5545 `RRULE` | nothing — recurrence-shaped, and recurrence is out of scope |
| RFC 9557 / IXDTF | the `[Europe/Prague]` annotated-instant form verbatim |
| AWS EventBridge (`rate()`/`at()`) | the precedent for *separate named slots* over one magic string |

## What must not regress

- **Pause semantics.** Timers keep running while paused; a delay that elapses meanwhile is
  due the moment it resumes (`CLAUDE.md`, `internal/engine/error.go:178`). So an `until`
  already in the past when it resolves must **clamp to now and log**, never fail — that is
  the rule the pause design already implies.
- **Arm-once.** `runDelay` guards on `WakeAt == nil` (`internal/engine/action.go:121`), so
  the value resolves exactly once per task entry. Keep it: a calendar target must not drift
  on re-claim.
- **Claim predicate and storage.** `wake_at` semantics are untouched
  (`internal/db/db_claim.go:53-62`). Everything here resolves to the same BIGINT
  millisecond column.

## Compatibility

- **Definitions.** `Action` decodes with plain `encoding/json` — no custom
  `UnmarshalJSON`, no `DisallowUnknownFields` — so new `omitempty` fields are absent in
  every stored definition, decode to zero, and change nothing.
- **`ms` was removed, not deprecated.** The constraint that it "must stay decodable
  forever" was entirely a function of *deployed* databases carrying rows that use it.
  Pre-release there are none, so `Action.Ms` is gone from the struct and the whole
  deprecation question is moot. This is the one part of the design that could only be done
  before the first release.
- **The removal inverts the skew hazard, and that needs a guard.** `Action` decodes with
  plain `encoding/json` and no `DisallowUnknownFields`, so a row carrying only `ms` now
  decodes with both slots empty. Previously that failed loudly only by accident —
  `evalDurationMsCtx("")` reached `strconv.ParseInt("")` and errored. The new slots accept
  a bare JSON number, so an absent slot and a present zero are no longer distinguishable
  by parse failure. `delayArity` therefore fails explicitly rather than resolving either
  bad arity to a default — **neither** slot is not zero, and **both** slots do not silently
  prefer `for` (if `until` held the far-future deadline, preferring `for` waits a fraction
  of the intended time and nothing reports it). This is permanent, not migration-only:
  `CheckDoc` runs at registration, the decoder runs over stored rows, and nothing
  re-validates them.
- **Editor schema.** The delay variant declares `ms` required and sets
  `"additionalProperties": false` (`internal/model/definition.go:213-222`), so `for` /
  `until` / `tz` must be added there or editors reject a definition the server accepts.
  `required: ["type", "ms"]` becomes a `oneOf` over the three slots. Both a literal and an
  expression are JSON strings, so the only discriminator available in that schema is
  `pattern` — write the variant by hand rather than routing it through `shape.RelaxedSchema`,
  whose `relax(S)` transform produces a symmetric "literal or expression" node and cannot
  express an asymmetric one.
- **Storage / wire.** No migration, no API type change, `openapi.json` unaffected beyond
  the action schema.

**Version skew is why this was release-blocking, and shipping it first is what resolved
it.** The silent-degradation hazard recorded in
[fetch-http-surface.md](fetch-http-surface.md) applies here and is *worse*: an older engine
decoding a definition that uses `for` and omits `ms` does not merely drop an optional
extra — it sees an empty `ms` and delays **zero**, turning a two-day wait into no wait at
all, silently. Landing `for`/`until` before any binary is released avoids the skew
entirely, because there is no older engine to decode it. Had this shipped afterwards it
would have needed either the `min_engine` assertion from that doc's open questions or a
hard registration error, both larger and themselves breaking.

## Ledger

**Where the build diverged from this draft** (all three make the design more
deterministic, none change the surface):

- **Calendar units with no `tz` resolve in UTC**, rather than being a separate "fixed"
  case. UTC has no transitions, so `1d` is exactly 24h and the draft's stated outcome falls
  out of the general rule — and `mo` / `y`, which "fixed" left undefined (30 days? 365?),
  get real calendar semantics for free.
- **Calendar components apply before fixed ones**, regardless of written order. Only a DST
  boundary can distinguish the two orders, and fixing it keeps a spec's meaning independent
  of how it was typed.
- **Unsatisfiable calendar dates are rejected at parse, not by resolving at registration.**
  The draft implied resolving a literal once to catch `*-02-30`. That would make
  registration depend on the wall clock — the same definition validating differently on
  different days, and a concrete far-future year failing against the five-year search
  bound. A concrete month/day pair is decidable statically, so `parsePattern` checks it
  against a leap year (keeping `*-02-29` legal). Patterns needing a calendar walk to
  disprove (`mon 2026-01-01`) still fail at resolve.

**The build:**

- `Action.For`, `Action.Until`, `Action.TZ` (`internal/model/definition.go:74-88`) and the
  action doc comment (`internal/model/definition.go:54`). Their Go type must admit both a
  string and a bare JSON number, unlike `Ms string` — today a bare YAML number never
  reaches that field, and numbers arise only as expression *results*
  (`internal/engine/action.go:226-238`).
- A self-contained `delayspec` parser: the duration grammar, the three instant forms, `tz`
  resolution, end-of-month clamping. No engine or DB dependency, so it is table-testable in
  isolation — which is where the DST and month-overflow cases belong.
- Classification and checks beside `checkMsTemplate` (`internal/validation/infer.go:200-210`):
  literal → `delayspec.Parse`; `$:` → check against `number`; `${ }` → reject by name.
- Exactly-one-of and `tz` validation in `internal/model/validate.go:142`.
- `runDelay` (`internal/engine/action.go:120-132`): resolve per slot against `db.Now()`,
  keeping the `WakeAt == nil` guard.
- Logging. `EventDelayArmed` (`internal/model/logs.go:53`) currently logs `"%dms"`
  (`internal/engine/action.go:128`). It must log the **source spec and the resolved
  absolute instant** — without that, a calendar target is undebuggable.
- `genctl` should echo the resolved instant for a literal at registration, so an author
  sees `*-*-01 08:00 → 2026-09-01 08:00 +02:00` and confirms intent.

**Reused as-is:** `wake_at` and the claim predicate; `durationFromValue`'s numeric
normalization (`internal/engine/action.go:224-254`); the `$:` discrimination in
`template.Parse`.

**Traps to avoid:**

- `time.AddDate` month overflow (above).
- **Do not route the literal grammar through `shape.Shape`.** It is not a template, does
  not participate in the Solver, and needs no relaxed editor node. *(Done: the literal
  branch calls `delayspec` directly.)* The stale `internal/shape/shape.go:34` comment
  claiming "delay ms" was an `Expr` slot has been corrected — it never was.
- **The `$:` branch uses a plain shape, not an `Expr` shape.** An `Expr` shape carries a
  *bare* expression body, the way a switch case passes `c.Case`; the delay slot's string
  still has its `$:` marker. Passing it as `Expr` would hand `$: x` to the expression lexer
  verbatim. A plain shape routes it through `template.Parse`, whose `$:` leaf is
  type-preserving, so the inferred type is the expression's own — which is what the
  `number` check needs.
- **Do not add a `now` root to the expression language** to solve the absolute-deadline
  case. See open questions.

## Open questions

- **Ceiling on the resolved delay.** A literal `for: "1y"` is visible at registration; a
  `$:` number is not, and a units slip parks an instance for a decade. A server-level
  maximum with its own error code, or unbounded. Shipped **unbounded**, which matches what
  `ms` did — so this is a gap carried forward, not a regression. *(open)*
- **Should a `$:` on `until` also accept RFC 3339?** It is the format vendors actually
  send, and it reintroduces a runtime parse — which would need a catchable `delay.invalid`
  rather than today's uncatchable `engine.expression` → `failInstance`
  (`internal/engine/action.go:124`, `internal/errcode/errcode.go:56`). Deferred: the number
  form covers every case where the caller can convert. *(open)*
- **`tz` from an expression.** *"Remind them at 08:00 their local time"* is a plausible
  workflow and is dead under literal-only. IANA names are a closed set and `LoadLocation`
  failure is crisp, so this is the easiest slot to relax later — and relaxing is
  backward-compatible where tightening would not be. Deliberately out of v1. *(open)*
- **A `now` root in expressions** would make `ms: "$: max(0, deadline - now)"` work.
  Considered and declined: it makes every expression impure, and switch cases and outputs
  *are* re-evaluated on retry, where a shifting clock is a real hazard — `runDelay` itself
  would be safe, but the keyword could not be scoped to it. *(closed unless a second use
  case appears)*
- **DST-ambiguous wall clocks.** *(decided)* A nonexistent wall clock (spring forward)
  **normalizes forward**; an ambiguous one (autumn fall-back) takes the **first, earlier**
  occurrence. `resolveWall` pins both rather than inheriting `time.Date`, whose own docs
  decline to guarantee the ambiguous case. The ambiguity is detected by the same wall clock
  existing one hour earlier, which makes the rule hold whichever occurrence `time.Date`
  happens to return. Not handled: transitions that are not a whole hour (Lord Howe's 30
  minutes).
- **Definition-level default `timezone`**, with per-task `tz` overriding, vs. per-task
  only. *(open)*
- **Deprecation appetite for `ms`:** *(closed)* Removed outright before the first release,
  so there is nothing to deprecate. `ms: "30000"` is written `for: 30000`.
- **Recurrence is out of scope.** `until` resolves to exactly one instant; a repeating
  schedule is a different feature and probably a different action type. *(closed for this
  doc)*

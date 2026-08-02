# `delay` and `timeout`: human durations and calendar deadlines

**Status: IMPLEMENTED.** `for` / `until` / `tz` landed 2026-07-31, replacing the old `ms`
slot outright (pre-release, so there was nothing to deprecate — `ms: "30000"` is now
`for: 30000`). Clock wildcards and steps landed 2026-08-01. A task's `timeout` adopted the
same grammar on 2026-08-02, replacing `timeout_ms` the same way.

The grammars live in `internal/delayspec`, deliberately free of engine and DB dependencies
so the calendar edge cases are table-testable in isolation.

Two slots, two homes: a `delay` action names when to **wake up**, a task's `timeout` names
when to **give up**. Everything from here to [§ As a `timeout`](#as-a-timeout) applies to
both; that section covers only where they differ.

## Syntax

A `delay` takes **exactly one** of `for` / `until`, plus an optional `tz`.

| slot | means | bare number |
|---|---|---|
| `for` | a duration, from arm time | milliseconds |
| `until` | an instant | unix milliseconds |
| `tz` | the zone wall clocks and calendar units resolve in | — |

Every example below is checked against the parser by the package's tests.

### How a value is written

A slot's accepted type depends on **how it is written**, decided syntactically before any
inference runs. `template.Parse` already makes exactly this distinction.

| form | example | treated as |
|---|---|---|
| literal string | `for: "2h30m"` | the grammar below, parsed at registration |
| bare JSON number | `for: 5000` | milliseconds (unix ms for `until`) |
| `$:` expression | `until: "$: outputs.job.deadline_ms"` | must infer to `number` |
| `${ }` interpolation | `for: "${ input.hours }h"` | **rejected by name** — yields a string at runtime |

### `for` — a duration

Units `ms` `s` `m` `h` `d` `w` `mo` `y`, concatenated, spaces optional between components
but not inside one (`1d 12h` yes, `1 d` no).

```yaml
for: "500ms"     for: "45m"       for: "1d"        for: "3mo"
for: "90s"       for: "2h30m"     for: "1d 12h"    for: "1y 6mo"
for: 5000                                    # bare number: milliseconds
for: "$: input.hours * 3600000"              # expression: milliseconds
```

With `tz`, the calendar units `d` `w` `mo` `y` are calendar arithmetic in that zone — `1d`
is "the same wall clock tomorrow", 23h or 25h across a DST boundary. Without `tz` the zone
is UTC, which has no transitions, so `1d` is exactly 24h; the fixed-unit case falls out of
the general rule rather than being a separate one. Month arithmetic clamps to the end of
the target month (`time.AddDate` would roll Jan 31 + 1mo to Mar 3). Calendar components
apply before fixed ones whatever order they were typed in.

### `until` — an instant

Four forms, always resolving to the next match strictly after now.

```yaml
# 1. absolute — carries its own offset, so tz is not consulted
until: "2026-09-01T08:00:00+02:00"
until: "2026-09-01T08:00:00Z"
until: "2026-09-01T08:00:00+02:00[Europe/Prague]"   # RFC 9557; the name is validated, then dropped

# 2. wall clock — no zone, so it is read in tz
until: "2026-09-01 08:00"        until: "2026-09-01T08:00"
until: "2026-09-01 08:00:00"     until: "2026-09-01"        # a bare date is midnight

# 3. offset and clock — a duration from now, then a wall clock on that day
until: "+2d 08:00"       # in two days, at 8
until: "+1mo 08:00"
until: "+12h 23:30:15"

# 4. calendar pattern — a systemd OnCalendar subset: [weekday] [Y-M-D] clock
until: "*-*-01 08:00"           # the next 1st, at 8
until: "*-12-25 00:00"          # the next Christmas
until: "2027-*-01 08:00"        # a pinned year
until: "*-*-31 23:59:59"        # months without a 31st are skipped
until: "mon 09:00"              # the next Monday
until: "mon *-*-13 09:00"       # weekday and date are an AND, not cron's OR

# and as numbers
until: 1789000000000                       # unix milliseconds
until: "$: outputs.job.deadline_ms"        # expression: unix milliseconds
```

#### Clock fields

Every field of a pattern's clock takes one of three shapes. This is what expresses a
schedule finer than a day.

| shape | means |
|---|---|
| `08` | that value only |
| `*` | every value |
| `base/step` | `base`, `base+step`, `base+2×step` … within the field's range |

| example | means | cron |
|---|---|---|
| `*:*:00` | every whole minute | `* * * * *` |
| `*:*:*` | every second | — (cron has no seconds field) |
| `*:*:0/5` | every five seconds | — |
| `*:0/15:00` | every quarter hour | `*/15 * * * *` |
| `*:2/5:00` | every five minutes, from :02 | `2-59/5 * * * *` |
| `*:30:00` | every hour at half past | `30 * * * *` |
| `0/6:00:00` | every six hours | `0 */6 * * *` |
| `12:*:00` | every minute of the 12th hour | `* 12 * * *` |
| `mon *:0/30:00` | every half hour on Mondays | `*/30 * * * 1` |
| `*-*-01 0/6:00:00` | every six hours on the 1st | `0 */6 1 * *` |

A clock is `HH:MM` or `HH:MM:SS`; an **omitted** seconds field stays `:00`, so `08:00`
keeps naming one instant a day. Only a written `*` or step widens a field.

### `tz`

IANA names, `UTC`, or fixed offsets — `Europe/Prague`, `+02:00`, `-05:30`. Omitted is UTC.
Literal-only: no expressions, no abbreviations.

## As a `timeout`

A task's `timeout` is the same two slots aimed at a deadline. It takes an extra shorthand,
because the overwhelmingly common case is a plain duration and `timeout: {for: "30s"}`
would be ceremony:

```yaml
timeout: 30s                                      # shorthand → for, resolved in UTC
timeout: 5000                                     # bare number: milliseconds
timeout: "$: input.budget_ms"                     # expression: milliseconds
timeout: {for: "1d", tz: Europe/Prague}           # long form, for calendar units in a zone
timeout: {until: "fri 17:00", tz: Europe/Prague}  # external tasks only — see below
```

The shorthand desugars to `for` at decode, the way `switch: next` desugars to a single
case, so the stored definition is always the long form and nothing downstream branches on
which was written.

Four rules are specific to this home:

**`until` is accepted only on an `external` task.** A deadline in the past has to mean
something, and it only means something coherent where the task parks: the instance is
simply due, which is the honest reading of "the review window closed". A `fetch` would
instead build a context that expired before the request was sent, and
`transport.ClassifyGoError` reports that as `http.timeout` — a member of the unknowable set,
so on an `only_once` task it can never be retried, ever, for a request that provably never
left. There is no honest code to report for it, so the slot is refused at registration.
`for` remains available everywhere.

**Absent means no deadline; it is not zero.** `timeout_ms: 0` used to spell "wait forever".
A slot carrying a duration grammar cannot also carry a magic zero meaning the opposite of
its smallest value, so absence is now the only spelling — a `fetch` falls back to the 30s
engine default, an `external` waits indefinitely.

**A deadline already in the past clamps on an `external` and is refused on a `fetch`.** The
rule is not "delays clamp, timeouts refuse" — it is *clamp wherever there is a truthful
code to report*:

| | past deadline | why |
|---|---|---|
| `delay` | clamps, wakes now | a delay that is late is simply due |
| `external` timeout | clamps, raises `external.timeout` | the window closed; that is exactly what the code means, and it is what the definition's `on_error` is written against |
| `fetch` timeout | refuses, fails the instance | an expired context reports `http.timeout` for a request never sent — a lie, and an unknowable one |

A past deadline reaches an external legitimately: a re-arm after a retry, or a resume from a
pause that outlasted the window. Failing there would hand an author who wrote
`on_error: [external.timeout]` an uncatchable `engine.expression` instead of the route they
asked for.

**Only `fetch` and `external` honour one at all.** The engine reads the slot in exactly two
places, so a timeout on a `child` or `delay` task is rejected rather than silently ignored.

**When it resolves differs by action type.** A `fetch` resolves per attempt, so a retry of
an expression-valued timeout gets the budget the context says it should have *now*. An
`external` resolves once at arm time, so a re-arm after an `external.timeout` retry keeps an
`until` pinned to the same instant while a `for` budget starts over.

### What is rejected

| written | why |
|---|---|
| `for: "5000"` | unitless string — 5000ms or 5s? The bare number form exists for this |
| `for: "2x"`, `for: ""` | not the duration grammar |
| `for: "${ input.hours }h"` | an interpolation yields a string at runtime |
| `until: "in two days"` | natural language is deliberately unsupported |
| `until: "*-*-01"` | a pattern with no clock names a day, not an instant |
| `until: "*-02-30 08:00"` | no year has one; decidable at registration |
| `until: "*:*:*/5"` | cron's step spelling — write the base, `0/5` |
| `until: "*:*:0/0"`, `"*:*:0/61"` | a step must be positive and within the field's range |
| `until: "+2d *:00"`, `"+2d 0/5:00"` | the offset form names one clock on its day |
| `until: "mon tue 09:00"` | two weekdays would mean "either", which this grammar cannot say |
| `tz: "CET"`, `tz: "Local"` | an abbreviation means the wrong thing for half the year, and resolves per host |
| both slots, or neither | exactly one of `for` / `until` |
| `timeout: {until: …}` on a `fetch` | a deadline already past reports `http.timeout` for a request never sent |
| `timeout:` on a `child` / `child_map` / `child_list` / `delay` task | nothing honours it there; silence would be worse |
| `timeout: {untill: …}`, `{timeout_ms: …}` | unknown key — it would decode to an empty timeout, i.e. no timeout |
| a `fetch` `timeout` resolving to now or earlier | the request would time out before it was sent (an `external` clamps instead) |

## Why it is this way

- **Expressions carry numbers only.** The literal grammar is *authoring* syntax, for a
  person typing YAML. A value from `input`, a fetch or an external task is machine-produced,
  and the machine form of an instant is a number. Nobody computes a cron expression at
  runtime either. The payoff: a literal is fully static (a typo fails at registration, not
  three days into a run) and an expression carries no parse at all.
- **`base/step`, not cron's `*/step`.** The base *is* the phase (`2/5` is :02, :07, :12) and
  `*/5` has nowhere to put one, which makes it a special case rather than an alternative.
  Recognition does not beat one spelling per concept when the error message names the right
  one. Steps are clock-only; `systemd` allows them on date fields, but nothing has asked for
  "every third day".
- **A step that does not divide its range wraps short** — `0/7` on seconds runs :00, :07 …
  :56, then :00, a 4-second gap. Inherent, and cron's behaviour too: alignment to the minute
  and even spacing cannot both hold.
- **No natural language.** `chrono-node` and friends are locale-dependent, ambiguous ("next
  Friday" *on* a Friday), and a parser upgrade would silently change what stored rows mean.
- **No recurrence in the slot.** `until` resolves to exactly one instant, `*:*:00` included;
  repetition is a back edge in the process, which is where genroc's recurrence has always
  lived. The loop has one property cron's default lacks: each iteration re-resolves against
  the current now, so a body that overruns its period waits for the next match instead of
  starting an overlapping run.
- **No `now` root in the expression language.** It would make every expression impure, and
  switch cases and outputs *are* re-evaluated on retry, where a shifting clock is a hazard.
  `runDelay` alone would be safe, but the keyword could not be scoped to it.
- **Unsatisfiable dates fail at parse, not by resolving.** Resolving once at registration
  would make the same definition validate differently on different days. A concrete
  month/day pair is decidable statically (checked against a leap year, so `*-02-29` stays
  legal); patterns needing a calendar walk to disprove still fail at resolve.

## DST

- **Spring forward.** A deleted wall clock normalizes **forward**: `*:*:00` goes 01:59 →
  03:00, which is also exactly 60 seconds of real time. `resolveWall` computes this itself
  by reading the request against both offsets around the gap and taking the later —
  `time.Date` is not consistently forward (Prague's 02:30 gives 03:30, but Santiago's
  midnight gap turns 00:49 into **23:49 the previous day**, which cost a whole day before
  the randomised cross-check caught it).
- **Fall back.** The repeated hour fires on both passes, which takes two rules because a
  search over increasing wall clocks sees only one of them:
  - `resolveWall` names the *first* occurrence; when that is already behind now, the
    **second occurrence** is the next match.
  - When now sits in the *first* pass, matches whose wall clock is behind now still occur
    again ahead of it. `repeatedMatch` searches the repeat from its own start and offsets
    the result; the sooner of the two searches wins. Without it, `*:30:00` at 02:30 summer
    time waits two hours instead of one.
- **Not handled:** transitions that are not a whole hour (Lord Howe's 30 minutes).

## How the search works

The date is **walked** a day at a time, bounded at five years: that keeps `*-*-31` and leap
days correct with no special cases, and a satisfiable date pattern is ~1500 integer
comparisons away at worst.

The clock is **computed**, never walked. `*:*:00` matches 1440 times a day and `*:*:*`
86 400, so `*-02-29 *:*:*` would be ~50 million steps. `nextClock` is a carry cascade over
hours → minutes → seconds instead — a fixed handful of comparisons however dense the
pattern is. That asymmetry is what makes per-second schedules affordable.

## What must not regress

- **Clamp wherever a truthful code exists; refuse only where none does.** Timers keep
  running while an instance is paused, so a target behind now is legitimate on resume. A
  `delay` clamps and wakes; an `external` timeout clamps and raises `external.timeout`; only
  a `fetch` timeout refuses, because the code it would otherwise report (`http.timeout`) is
  false and unknowable. `resolveTimeout` therefore returns a past instant untouched and lets
  each caller decide — do not move that decision into it, and do not make the two callers
  agree.
- **Arm once.** `runDelay` guards on `WakeAt == nil`, so a calendar target resolves exactly
  once per task entry and cannot drift on re-claim.
- **`delayArity` fails loudly on any slot count but one.** `CheckDoc` enforces it at
  registration, but the decoder also runs over stored rows that never re-validate, and
  `Action` decodes with no `DisallowUnknownFields` — a row carrying only the removed `ms`
  decodes to *neither* slot, which must not become a zero-length wait. *Both* slots must not
  silently prefer one either: if `until` held the far-future deadline, falling back to `for`
  waits a fraction of the intended time with nothing reporting it.
- **`model.DelaySpec` must never gain an `UnmarshalJSON`.** `Action` embeds it — which is
  what keeps `{"type": "delay", "for": "1h"}` flat on the wire — so a decoder on it is
  promoted to `Action`, and json would hand that decoder the *whole action object*: `type`,
  `url` and every other field would decode to nothing, silently. The `timeout` shorthand
  therefore lives on the `Timeout` wrapper, which nothing embeds.
- **An absent `timeout` must stay absent on the wire.** Absence is the only spelling of "no
  deadline", and a present timeout must be in the future — so anything that turns absent
  into zero (a dropped `omitzero`, a defaulted struct) makes every such task unrunnable.
- **Storage.** `wake_at` and the claim predicate are untouched; everything resolves to the
  same BIGINT millisecond column. No migration, no API type change — a `timeout` deadline
  goes into the same column a delay's wake-up does.
- **Do not route the literal grammar through `shape.Shape`**, and use a **plain** shape for
  the `$:` branch, not an `Expr` one — an `Expr` shape carries a bare expression body, while
  the delay slot's string still has its `$:` marker.

## Prior art

| Format | Taken |
|---|---|
| Go `time.ParseDuration` | the grammar for `ms`…`h`, extended above `h` (Go stops there because `d` is calendar-ambiguous — hence `tz`) |
| `systemd.time(7)` | the calendar-pattern form, clock wildcards and steps, `+2d`-style stamps; a subset, not the whole grammar |
| RFC 9557 / IXDTF | the `[Europe/Prague]` annotated-instant form verbatim |
| ISO 8601 duration (`PT2H30M`) | the calendar-unit set only; rejected as a surface |
| AWS EventBridge (`rate()`/`at()`) | the precedent for separate named slots over one magic string |
| RFC 5545 `RRULE` | nothing — recurrence-shaped, and recurrence is out of scope |

## Open questions

- **Lists** (`*:*:0,15,30,45`). `systemd` and cron both have them; a step covers every
  regular schedule, so this is only for genuinely irregular times.
- **Steps on date fields** (`*-*-1/3`). Same shape as the clock change, unasked for.
- **`tz` from an expression.** "Remind them at 08:00 their local time" is dead under
  literal-only. IANA names are a closed set and failure is crisp, so this is the easiest
  slot to relax later — and relaxing stays backward-compatible where tightening would not.
- **Definition-level default `tz`**, with per-task override, vs. per-task only.
- **A ceiling on the resolved delay.** A literal `for: "1y"` is visible at registration; a
  `$:` number is not, and a units slip parks an instance for a decade. Shipped unbounded,
  which is what `ms` did — a gap carried forward, not a regression. Applies to a `timeout`
  equally, where the slip is quieter: an over-long deadline looks like a hung task.
- **Per-attempt vs. whole-task timeouts.** A `fetch` timeout bounds one attempt, so a task
  with retries can elapse far more than its timeout in total — `timeout: "30s"` with three
  retries is up to four 30s attempts plus backoff. That is the same behaviour `timeout_ms`
  had and it is what a per-attempt deadline should do, but there is no way to express "give
  up on this task after 2 minutes however many attempts that takes". An `until` on a fetch
  would be a *worse* answer to this (see § As a `timeout`); a separate whole-task budget
  would be the honest one.
- **Should `$:` on `until` accept RFC 3339?** It is what vendors send, but it reintroduces a
  runtime parse, which would need a catchable `delay.invalid` rather than today's
  uncatchable `engine.expression`. The number form covers every case where the caller can
  convert. Same wall the `Retry-After` question in
  [fetch-http-surface.md](fetch-http-surface.md) hits from the other side.

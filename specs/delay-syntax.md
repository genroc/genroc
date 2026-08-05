# `delay` and `timeout`: human durations and calendar deadlines

**Status: implemented** (`for`/`until`/`tz` 2026-07-31, clock wildcards 2026-08-01,
`timeout` adoption 2026-08-02; the old `ms`/`timeout_ms` slots replaced outright,
pre-release). Grammars in `internal/delayspec`, dependency-free so calendar edge cases
are table-testable; user-facing reference belongs to `docs/`. Silent-failure invariants
in [internal/delayspec/CLAUDE.md](../internal/delayspec/CLAUDE.md) and
[internal/model/CLAUDE.md](../internal/model/CLAUDE.md).

## Syntax

Exactly one of `for` (duration from arm time; bare number = ms) / `until` (instant;
bare number = unix ms), plus optional `tz` (IANA name, `UTC`, or fixed offset —
literal-only, no abbreviations: `CET` means the wrong thing half the year and resolves
per host). How a value is written decides its treatment, syntactically: literal string
→ the grammar, parsed at registration; bare number → ms; `$:` expression → must infer
to number; `${ }` interpolation → **rejected by name** (yields a string).

- **`for`**: units `ms s m h d w mo y`, concatenated (`2h30m`, `1d 12h`). With `tz`,
  calendar units are calendar arithmetic in that zone (`1d` = same wall clock tomorrow,
  23h/25h across DST); without, UTC makes fixed units fall out of the general rule.
  Month arithmetic clamps to month end (`time.AddDate` would roll Jan 31 + 1mo to
  Mar 3). Calendar components apply before fixed ones regardless of typed order.
- **`until`**: four forms, always the next match strictly after now — absolute RFC 3339
  (own offset; RFC 9557 `[Europe/Prague]` accepted, validated, dropped), wall clock
  read in `tz` (bare date = midnight), offset+clock (`+2d 08:00`), and a systemd
  OnCalendar subset `[weekday] [Y-M-D] clock` (`*-*-01 08:00`, `mon 09:00`; weekday AND
  date, not cron's OR).
- **Clock fields**: `08` (that value), `*` (every), `base/step` (base, base+step, …
  — the base IS the phase: `2/5` is :02, :07…). `HH:MM[:SS]`; omitted seconds stay
  `:00` — `08:00` keeps naming one instant a day; only a written `*` or step widens.

### Accepted spellings

Every example here is parsed by `doc_examples_test.go` — this reference is executable, so
a spelling that stops working fails there rather than in someone's definition.

```yaml
# for — duration from arm time (units ms s m h d w mo y, concatenated)
for: "500ms"      for: "90s"        for: "45m"        for: "2h30m"
for: "1d"         for: "1d 12h"     for: "3mo"        for: "1y 6mo"
for: 5000                                     # bare number: milliseconds
for: "$: input.hours * 3600000"               # expression: milliseconds

# until — an instant; always the next match strictly after now
until: "2026-09-01T08:00:00+02:00"            # absolute, carries its own offset
until: "2026-09-01T08:00:00Z"
until: "2026-09-01T08:00:00+02:00[Europe/Prague]"   # RFC 9557; name validated, then dropped
until: "2026-09-01 08:00"                     # wall clock, read in tz
until: "2026-09-01T08:00"
until: "2026-09-01 08:00:00"
until: "2026-09-01"                           # a bare date is midnight
until: "+2d 08:00"                            # offset then a clock on that day
until: "+1mo 08:00"
until: "+12h 23:30:15"
until: "*-*-01 08:00"                         # calendar pattern: the next 1st, at 8
until: "*-12-25 00:00"
until: "2027-*-01 08:00"
until: "*-*-31 23:59:59"                      # months without a 31st are skipped
until: "mon 09:00"
until: "mon *-*-13 09:00"                     # weekday AND date, not cron's OR
until: 1789000000000                          # unix milliseconds
until: "$: outputs.job.deadline_ms"

tz: "Europe/Prague"    tz: "UTC"    tz: "+02:00"    tz: "-05:30"
```

#### Clock fields

Each field of a pattern's clock is one value, `*` (every value), or `base/step` — the
base is the phase. This is what expresses a schedule finer than a day.

| pattern | means |
|---|---|
| `*:*:00` | every whole minute |
| `*:*:*` | every second |
| `*:*:0/5` | every five seconds |
| `*:0/15:00` | every quarter hour |
| `*:2/5:00` | every five minutes, from :02 |
| `*:30:00` | every hour at half past |
| `0/6:00:00` | every six hours |
| `12:*:00` | every minute of the 12th hour |
| `mon *:0/30:00` | every half hour on Mondays |
| `*-*-01 0/6:00:00` | every six hours on the 1st |

A clock is `HH:MM` or `HH:MM:SS`; an omitted seconds field stays `:00`, so `08:00` keeps
naming one instant a day. Only a written `*` or step widens a field.

## What is rejected

| written | why |
|---|---|
| `for: "5000"` | unitless string — 5000ms or 5s? the bare-number form exists for this |
| `for: "2x"` | not the duration grammar |
| `for: "${ input.hours }h"` | an interpolation yields a string at runtime |
| `until: "in two days"` | natural language is deliberately unsupported |
| `until: "*-*-01"` | a pattern with no clock names a day, not an instant |
| `until: "*-02-30 08:00"` | no year has one; decidable at registration |
| `until: "*:*:*/5"` | cron's step spelling — write the base, `0/5` |
| `until: "*:*:0/0"` | a step must be positive and within the field's range |
| `until: "+2d *:00"` | the offset form names one clock on its day |
| `until: "mon tue 09:00"` | two weekdays would mean "either", which this grammar cannot say |
| `tz: "CET"` | an abbreviation means the wrong thing for half the year, and resolves per host |
| `tz: "Local"` | resolves per host |

Also refused, above the grammar: both slots or neither; `until` on a `fetch` timeout; a
`timeout` on a child/delay task; unknown keys (`untill`, `timeout_ms` — they would decode
to *no* timeout); and a fetch timeout resolving to now or earlier.

## As a `timeout`

Same slots aimed at "give up" instead of "wake up", plus a scalar shorthand
(`timeout: 30s` desugars to `for` at decode, so stored definitions are canonical).
Home-specific rules:

- **`until` only on `external`** — the one type where a past deadline coherently means
  "due now". On a fetch it would build a pre-expired context reporting `http.timeout` —
  false, unknowable, unretryable on `only_once`, for a request that never left.
- **Absent means no deadline, and is the only spelling of it** — a duration slot cannot
  also carry a magic zero (fetch defaults to 30s, external waits forever).
- **Past deadlines: clamp wherever a truthful code exists, refuse where none does.**
  Delay clamps (late = due); external clamps and raises `external.timeout` (exactly
  what its `on_error` is written against — past deadlines arrive legitimately via
  re-arms and long pauses); fetch refuses. Not "delays clamp, timeouts refuse".
- **Only fetch and external honour one** — elsewhere it is rejected, never ignored.
- **Resolution timing differs**: fetch per attempt (a retry gets today's budget);
  external once at arm (a re-arm keeps an `until` pinned, restarts a `for`).

## Why it is this way

- **Expressions carry numbers only.** The literal grammar is authoring syntax; machine
  values are numbers. A literal is fully static (typos fail at registration, not three
  days in); an expression carries no parse at all.
- **`base/step`, not `*/step`** — `*/5` has nowhere to put a phase, making it a special
  case of base/step; one spelling per concept, the error names it. A step that does not
  divide its range wraps short (cron's behaviour too — even spacing and minute
  alignment cannot both hold).
- **No natural language** (locale-dependent, ambiguous, and a parser upgrade would
  silently change stored rows). **No recurrence in the slot** — `until` names one
  instant; repetition is a back edge in the process, which re-resolves per iteration so
  an overrunning body skips to the next match instead of overlapping. **No `now`
  root** — it would make every expression impure exactly where re-evaluation happens
  (retries).
- **Unsatisfiable dates fail at parse, never by resolving at registration** — resolving
  once would make the same definition validate differently on different days.

## DST and the search

Spring forward: a deleted wall clock normalizes **forward**, computed by `resolveWall`
itself — `time.Date` is not consistently forward (Prague's 02:30 → 03:30, but
Santiago's midnight gap turns 00:49 into 23:49 *the previous day*; the randomised
cross-check caught a whole lost day). Fall back: the repeated hour fires on both
passes, needing two rules — `resolveWall` names the first occurrence, and
`repeatedMatch` searches the repeat from its own start for matches behind now that
recur ahead of it (else `*:30:00` at 02:30 summer time waits two hours instead of one).
Sub-hour transitions (Lord Howe) unhandled. The date is **walked** a day at a time
(bounded five years — keeps `*-*-31` and leap days correct for free, ≤~1500
comparisons); the clock is **computed** as a carry cascade, never walked (`*:*:*` is
86,400 matches/day; stepping would make a leap-day pattern ~50M steps).

## What must not regress

- The clamp/refuse split lives in the *callers*: `resolveTimeout` returns a past
  instant untouched — do not centralize the decision or make the callers agree.
- **Arm once**: `runDelay` guards on `WakeAt == nil`, so a calendar target cannot drift
  on re-claim.
- **`delayArity` fails loudly on any count but one** — decoders run over stored rows
  that never re-validate; a row with only the removed `ms` must not become a zero wait,
  and preferring one of two slots silently waits the wrong time.
- **`DelaySpec` must never gain an `UnmarshalJSON`** — `Action` embeds it, so the
  decoder would be promoted and silently swallow the whole action; the shorthand lives
  on the unembedded `Timeout` wrapper.
- **Absent timeouts stay absent on the wire** (a dropped `omitzero` makes every fetch
  unrunnable). Storage untouched: everything resolves into the same `wake_at` column.
- Don't route the literal grammar through `shape.Shape`; the `$:` branch uses a plain
  shape, not `Expr` (the slot's string still carries its marker).

## Prior art

Go `time.ParseDuration` (the sub-day grammar; Go stops at `h` because `d` is
calendar-ambiguous — hence `tz`); systemd.time(7) (patterns, wildcards, steps, `+2d`
stamps — a subset); RFC 9557 (the annotated instant, verbatim); ISO 8601 durations
(the unit set only; rejected as a surface); hosted cron services (named slots over one
magic string); RRULE (nothing — recurrence is out of scope).

## Open questions

Lists (`0,15,30,45`) — steps cover every regular schedule. Steps on date fields —
unasked. `tz` from an expression ("their local time") — the easiest slot to relax
later; relaxing is compatible, tightening is not. A definition-level default `tz`.
A ceiling on resolved delays (a `$:` units slip parks an instance for a decade;
shipped unbounded, a gap carried forward from `ms`). Per-attempt vs whole-task
timeouts — `timeout: 30s` with three retries is up to four attempts plus backoff;
"give up after 2 minutes total" is inexpressible, and an `until` fetch would be the
wrong answer — a separate whole-task budget is the honest one. `$:` accepting RFC 3339
on `until` — reintroduces a runtime parse needing a catchable code; the number form
covers callers that can convert (same wall as `Retry-After` in fetch-http-surface).

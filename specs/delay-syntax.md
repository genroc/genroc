# `delay` and `timeout`: human durations and calendar deadlines

Status: **Built.** Grammars in `internal/delayspec`, dependency-free so calendar edge cases are
table-testable. The user reference is `docs/src/content/docs/reference/definition/delay-syntax.mdx`;
silent-failure invariants are in [internal/delayspec/CLAUDE.md](../internal/delayspec/CLAUDE.md)
and [internal/model/CLAUDE.md](../internal/model/CLAUDE.md).

**This file is executable.** `internal/delayspec/doc_examples_test.go` parses every slot literal
above the "What is rejected" heading as must-parse, every one under it as must-fail, and each clock
in the "Clock fields" table; it keys on those exact headings (their first occurrence) and needs at
least 25 accepted examples. Do not rename them, quote them above the split, or write a rejected
spelling above it.

## Syntax

Exactly one of `for` (duration from arm time) / `until` (an instant), plus optional `tz` (IANA
name, `UTC`, or fixed offset). How a value is written decides its treatment, syntactically: a
literal string → the grammar, parsed at registration; a bare number → milliseconds (`until`:
unix ms); a `$:` expression → must infer to number; a `${ }` interpolation → **rejected by name**
(it yields a string).

- **`for`**: units concatenated. With `tz`, calendar units are calendar arithmetic in that zone
  (`1d` = same wall clock tomorrow, 23h/25h across DST); without, UTC. Month arithmetic clamps to
  month end (`time.AddDate` would roll Jan 31 + 1mo to Mar 3). Calendar components apply before
  fixed ones regardless of written order.
- **`until`**: always the next match strictly after now. RFC 9557 `[zone]` annotations are
  validated, then dropped. A calendar pattern requires weekday AND date, not cron's OR.
- **Clock fields**: a value, `*`, or `base/step` (the base IS the phase). Omitted seconds stay
  `:00`, so `08:00` names one instant a day; only a written `*` or step widens.

### Accepted spellings

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
`timeout` on a child/delay action; `for` / `until` / `tz` on an action that is not a delay
(the embed makes them decode everywhere, and a fetch wanting a deadline writes `timeout`);
unknown keys (`untill`, `timeout_ms` — they would decode to *no* timeout); and a fetch
timeout resolving to now or earlier.

## As a `timeout`

The same slots aimed at "give up" instead of "wake up", plus a scalar shorthand (`timeout: 30s`
desugars to `for` at decode, so stored definitions are canonical). It is a slot of the
**action**, because which slots are legal depends on the action's type — a timeout with no call to
bound cannot be written.

- **`until` only on `external`** — the one type where a past deadline coherently means "due now".
  On a fetch it would build a pre-expired context reporting `http.timeout`: false, unknowable,
  unretryable on `only_once`, for a request that never left.
- **Absent is the only spelling of "no deadline"** — no magic zero (fetch defaults to 30s,
  external waits forever).
- **Past deadlines clamp wherever a truthful code exists and refuse where none does.** A delay
  clamps (late = due); external clamps and raises `external.timeout`, which is what its
  `on_error` is written against (past deadlines arrive legitimately via re-arms and long pauses);
  fetch refuses.
- **Only fetch and external honour one** — elsewhere it is rejected, never ignored.
- **Resolution timing differs**: fetch per attempt (a retry gets a fresh budget); external once at
  arm (a re-arm keeps an `until` pinned and restarts a `for`).

## Why it is this way

- **Expressions carry numbers only.** The literal grammar is authoring syntax; a literal is fully
  static (typos fail at registration, not three days in), and an expression carries no parse.
- **`base/step`, not `*/step`** — `*/5` has nowhere to put a phase; one spelling per concept. A
  step that does not divide its range wraps short (even spacing and alignment cannot both hold).
- **`tz` is literal-only, with no abbreviations** — `CET` means the wrong thing half the year and
  resolves per host.
- **No natural language** (locale-dependent, and a parser upgrade would silently change stored
  rows). **No recurrence in the slot** — repetition is a back edge, which re-resolves per
  iteration, so an overrunning body skips to the next match instead of overlapping. **No `now`
  root** — it would make every expression impure exactly where re-evaluation happens (retries).
- **Unsatisfiable dates fail at parse, never by resolving at registration** — resolving would make
  the same definition validate differently on different days.

## DST and the search

Spring forward: a deleted wall clock normalizes **forward**, computed by `resolveWall` itself —
`time.Date` is not consistently forward (Santiago's midnight gap turns 00:49 into 23:49 *the
previous day*). Fall back: the repeated hour fires on both passes — `resolveWall` names the first
occurrence, and `repeatedMatch` finds matches inside the repeat (else `*:30:00` at 02:30 summer
time waits two hours instead of one). Sub-hour transitions (Lord Howe) are unhandled. The date is
**walked** a day at a time (bounded at five years, `maxPatternDays` — keeps `*-*-31` and leap days
correct for free); the clock is **computed** as a carry cascade, never walked (`*:*:*` is 86,400
matches a day).

## What must not regress

- The clamp/refuse split lives in the *callers*: `resolveSpec`/`resolveTimeout` return a past
  instant untouched — do not centralize the decision.
- **Arm once**: `runDelay` guards on `WakeAt == nil`, so a calendar target cannot drift on
  re-claim.
- **`delayArity` fails loudly on any count but one** — decoders run over stored rows that never
  re-validate; a row with only the removed `ms` must not become a zero wait, and preferring one of
  two slots silently waits the wrong time.
- **`DelaySpec` must never gain an `UnmarshalJSON`** — `Action` embeds it, so the decoder would be
  promoted and silently swallow the whole action; the shorthand lives on the unembedded `Timeout`.
- **Absent timeouts stay absent on the wire** (a dropped `omitzero` makes every fetch unrunnable).
  Everything resolves into the same `wake_at` column.
- Don't route the literal grammar through `shape.Shape`; the `$:` branch uses a plain shape, not
  `Expr` (the slot's string still carries its marker).

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

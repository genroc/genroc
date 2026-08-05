# `retry`: from a count to a policy

Status: **implemented (2026-08-02).** An `on_error` rule's `retries: N` became
`retry: {attempts, delay, factor, max_delay}`, with `retry: 3` as the scalar shorthand.
Line numbers and behaviour are as of that date.

## What was wrong with a count

The rule had exactly one knob, and everything about *when* a retry fired was fixed in
code: `2^attempt` seconds, ceiling 5 minutes, jitter into the upper half. The only
override in the system was `--immediate-retries`, a testing flag.

Two consequences, and the second is the one that mattered:

- **The wall-clock budget was not authorable and barely predictable.** Authors wrote a
  count; what they cared about was duration. `retries: 5` meant "somewhere between thirty
  seconds and a minute", and the jitter — which is correct and stays — made the mapping
  fuzzy on purpose.
- **The curve was wrong at both ends.** A rate-limited API answering 429 with a window
  got hammered five times inside that window and then failed. A DNS blip that clears in
  200 ms waited two seconds anyway.

The escape hatch existed and still does:
[examples/polling-task/poller.genroc.yaml](../examples/polling-task/poller.genroc.yaml)
is a hand-rolled retry loop — `on_error → goto $backoff`, a `delay` task, a counter in
`self.previous.attempt`, a `raise` when the budget is spent. It expresses *any* policy,
at the cost of an extra task and a hand-maintained counter, and it only works because
`delay`'s `for` accepts expressions while `retries` accepted an integer literal. That
asymmetry was the actual finding: the delay grammar is the richest thing in the
language, and retry was the one timer that could not reach it.

## The field set

Not invented here: `{initial delay, growth factor, max interval, attempts}` is the
industry-standard quartet — genroc had one of the four. Two findings behind it: the
ceiling must be an explicit field (its job is to stop a runaway `factor`; deriving it
from the base is not the same thing), and one ordered `on_error` list that both routes
and retries per code pattern beats splitting retry-eligibility from the catch into two
lists over the same error names.

## The shape

```yaml
on_error:
  - code: [pre.error, pre.timeout]
    retry: 3                                            # == {attempts: 3}
  - code: [http.429]
    retry: { attempts: 4, delay: 30s, factor: 2, max_delay: 10m }
```

Decisions worth recording, since each had a cheaper alternative:

- **An object, not four flat fields.** `ErrorCase` already carried six keys. A nested
  policy keeps the rule readable, makes D7's child-task rejection one key rather than
  four, and leaves an obvious home for a budget or a jitter setting later.
- **A scalar shorthand, following `Timeout`.** Same reason and same mechanism: the
  shorthand lives on a wrapper type nothing embeds, and `MarshalJSON` writes the object
  form, so a stored definition is canonical and everything downstream sees one shape.
- **The default curve is 1s, factor 2, ceiling 5m** — the standard default elsewhere.
  The old fixed curve started at 2s, but only as an artifact: it passed
  an already-incremented attempt counter into `2^attempt`, so its 1s step was never
  emitted. With `delay` now documented as the wait before the *first* retry — which is
  what every engine means by its interval field — 2s was a number with no argument behind
  it.
- **The ceiling is absolute, not relative to the base.** A relative ceiling is only safe
  when a wall-clock budget backstops it; genroc deferred that budget (below), so a
  relative ceiling would have nothing behind it.
- **A default ceiling never truncates an authored base.** `Ceiling()` is
  `max(5m, Base())`, so `delay: 1h` alone does not clamp back to 5m; an *authored*
  `max_delay` below `delay` is refused instead of silently winning.
- **Durations are fixed-unit only.** `delay: 1d` is refused with `"24h"` as the fix: the
  curve multiplies the value and compares it to a ceiling, and a calendar duration has no
  length until a timezone and a start instant fix it. This is why `RetryDuration` is its
  own type rather than a reuse of `DelaySpec`.
- **`retries` is refused, not accepted as an alias.** Two spellings for one field is a
  second thing to keep true, and the hint names the replacement. Accepting it silently
  was never an option: a dropped key leaves a rule that still matches and still routes,
  and only never retries.

## What was deliberately left out

- **`jitter`**, as a strategy or a factor. It is always on, always in the upper half, and
  the integration tests depend on it only ever shortening. Nobody has asked to tune it.
- **A wall-clock budget** (`retry_for: 10m`, or `until:`). The right unit conceptually,
  but it collides with pause/resume (does a 10-minute budget survive a two-day pause?)
  and with the per-attempt `timeout`. Real design work, deferred until asked for.
- **Expression-valued delays**, and with them the `Retry-After` case — a server saying
  when to come back. This is the frontier, and it
  is **not blocked on retry config**: it needs response headers, string-literal indexing,
  and a seconds→ms conversion, all listed in
  [fetch-http-surface.md](fetch-http-surface.md). Built now it would be inert. When those
  land, `delay` is the slot it goes in.

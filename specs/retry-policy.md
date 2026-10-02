# `retry`: a policy, not a count

Status: **Built, except what "What was deliberately left out" lists.**

An `on_error` rule's `retry` is `{retries, delay, factor, max_delay}`; `retry: 3` is shorthand
for `{retries: 3}`, and every slot also takes a `$:` expression.

## Why a policy

Authors care about duration, not a count, and one fixed curve was wrong at both ends: a
rate-limited API answering 429 with a window got hammered inside it, and a DNS blip that clears
in 200 ms waited seconds anyway. A hand-rolled loop —
[examples/polling-task/poller.genroc.yaml](../examples/polling-task/poller.genroc.yaml), an
`on_error → goto` into a `delay` task with a counter — expresses any policy at the cost of an
extra task, because `delay`'s `for` takes expressions. `retry` was the one timer that could not
reach that grammar.

## The field set

`{initial delay, growth factor, max interval, attempts}` is the industry-standard quartet. The
ceiling is an explicit field: its job is to stop a runaway `factor`, which deriving it from the
base does not do. One ordered `on_error` list both routes and retries per code pattern, rather
than splitting retry-eligibility from the catch into two lists over the same codes.

## The shape

Each decision had a cheaper alternative:

- **An object, not four flat fields** — keeps a rule readable and leaves a home for a later
  setting.
- **A scalar shorthand on a wrapper type nothing embeds**, like `Timeout`; `MarshalJSON` writes
  the object form, so a stored definition is canonical. Never embed `Retry`: its promoted
  `UnmarshalJSON` would eat the outer object.
- **The default curve is 1s, factor 2, ceiling 5m**, `delay` being the wait before the *first*
  retry.
- **The ceiling is absolute, not relative to the base.** A relative ceiling bounds nothing unless
  a wall-clock budget sits behind it, and genroc has none.
- **A default ceiling never truncates an authored base**: `Retry.Resolve` takes
  `max(5m, delay)`, so `delay: 1h` alone is not clamped to 5m. An authored `max_delay` below
  `delay` is refused instead of silently winning.
- **Durations are fixed-unit only** — `RetryDuration`, not `DelaySpec`. The curve multiplies the
  value, and a calendar duration has no length until a zone and an instant fix it; `1d` is
  refused with `24h` as the fix.
- **Unknown keys are refused, never aliased.** A rule-level `retries` gets a hint naming
  `retry` (`ruleFieldHints`), and a typo inside `retry` is an error: a dropped key leaves a rule
  that still matches and routes, and never retries.

## Expression-valued slots

**The classification is syntactic, decided at decode**, as `DelaySpec` does it: a bare number is
the literal, a `$:` leaf is an expression, a `${ }` interpolation is refused by name (it produces
a string), and a quoted number is refused.

- **Bounds move to runtime.** `validateRetry` guards every check on `IsExpr()`, and
  `Retry.Resolve` repeats them all with the same wording. A bound checked in one place only is
  one config can walk past.
- **A policy resolves once per error, before it is consulted, and a resolution failure fails
  the instance** (`engine.expression`). Falling through would turn an unreadable policy into "no
  retries", the author's budget vanishing with nothing reporting it.
- **An expression `retries` counts as retrying for the `only_once` tiers** — the conservative
  reading, which keeps them in force; `isRetryAllowed` gates by code at runtime regardless.
- **It reads `error`** — the failure being retried (per slot, on a child task) — beside `input`,
  `outputs` and `config`: the scope the rule's `case` is matched in (specs/task-scopes.md §The
  error axis). So `delay: "$: error.data.retry_after"` works where a payload carries the value.

## What was deliberately left out

- **`jitter`**, as a strategy or a factor. It is always on, always in the upper half, and
  the integration tests depend on it only ever shortening. Nobody has asked to tune it.
- **A wall-clock budget** (`retry_for: 10m`, or `until:`). The right unit conceptually,
  but it collides with pause/resume (does a 10-minute budget survive a two-day pause?)
  and with the per-attempt `timeout`. Real design work, deferred until asked for.
- **`Retry-After`** — a server saying when to come back. Still the frontier: response
  headers and string-literal indexing shipped ([fetch-http-surface.md](fetch-http-surface.md)),
  but a failed fetch's `error` carries no headers and there is no seconds→ms conversion.
  Expression-valued slots (below) are the syntax it will use; what is missing is the value to put in them.

  This deferral once covered expression-valued slots as a whole, on the reasoning that
  they existed *for* `Retry-After`. That turned out to be wrong: the case that arrived
  first was a curve that has to differ per environment — a Kubernetes cold start is
  minutes, a laptop is seconds — and it needs nothing from the HTTP surface, because
  `config` is already resolved every tick and already in scope.

# internal/delayspec

## Delay syntax: `for` / `until`

A `delay` action takes exactly one of **`for`** (a duration from arm time) or **`until`**
(an instant), plus an optional **`tz`**. The old `ms` slot was removed outright before the
first release — `ms: "30000"` is now `for: 30000`. Full design, decisions and open
questions: [specs/delay-syntax.md](../../specs/delay-syntax.md). The grammars live in this
package, deliberately free of engine and DB dependencies so the calendar edge cases are
table-testable.

The same two slots are a task's `timeout`, which decodes to the same `model.DelaySpec` and
resolves through the same `Engine.resolveSpec` — a timeout is this grammar aimed at a
deadline rather than a wake-up. Everything below applies to both.

What differs is what a *past* instant means, and it splits by whether a truthful code exists
to report: a delay clamps and wakes, an external timeout clamps and raises
`external.timeout`, and only a fetch timeout refuses — an expired context would report
`http.timeout` for a request that was never sent. `resolveSpec` and `resolveTimeout`
deliberately return a past instant untouched so each caller decides. See
[internal/model/CLAUDE.md](../model/CLAUDE.md) for the timeout-only rules (`until` is
external-only; absent ≠ zero).

Three things that break silently if you touch this:

1. **A slot's accepted type is decided syntactically, before inference.** A pure literal
   parses against the delayspec grammar at registration; a `$:` leaf must infer to
   `number`; a `${ }` interpolation is **rejected by name**, because it produces a string
   at runtime. `template.Parse` already makes exactly this distinction — `Static()` is the
   literal branch, `IsExpr()` the `$:` one — so there is no "string if literal, number if
   expression" type to express. The `$:` branch uses a **plain** shape, not an `Expr` one:
   `Expr` shapes carry a bare expression body (as a switch case does), and the delay slot's
   string still has its `$:` marker.
2. **`delayArity` must fail loudly on any slot count other than one.** `CheckDoc` enforces
   exactly-one-of at registration, but the decoder also runs over stored rows that never
   re-validate, and `Action` decodes with no `DisallowUnknownFields`. Neither slot must not
   become a zero-length wait (a row carrying only the removed `ms` decodes to exactly that),
   and **both** slots must not silently prefer one — if `until` held the far-future
   deadline, falling back to `for` waits a fraction of the intended time with nothing
   reporting it. Resolve neither case to a default. (`delayArity` itself lives in
   `internal/engine/action.go`, not here.)
3. **Calendar units are calendar arithmetic in `tz` (UTC when absent), applied before fixed
   units.** `1d` is "the same wall clock tomorrow" — 23h or 25h across a DST boundary — not
   24h. Month arithmetic clamps to the end of the target month (`time.AddDate` would roll
   Jan 31 + 1mo to Mar 3). A nonexistent wall clock normalizes forward; an ambiguous one
   takes the first occurrence.

An `until` that resolves behind now **clamps to now and logs**; it must never fail. Timers
keep running while an instance is paused, so a past target is a legitimate state on resume.

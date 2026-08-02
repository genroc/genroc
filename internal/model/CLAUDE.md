# internal/model

## Validating `on_error` on an `only_once` task

`validateOnError` (`validate.go`) is the registration-time half of the `only_once` rule;
the runtime half and the unknowable set are in
[internal/engine/CLAUDE.md](../engine/CLAUDE.md). Design and rejected alternatives:
[docs/only-once-interrupted.md](../../docs/only-once-interrupted.md).

1. **Retries on an `only_once` task are three tiers, applied per pattern.** A pattern
   matching only `pre.*` is safe alone; anything else needs `not_reached: true` **and must
   name exact codes**, because an assertion about a wildcard is not an assertion; and a
   member of the unknowable set is refused however it is named. Tier 3 is tested first so
   naming `http.timeout` gets the reason it is hopeless rather than advice that leads
   nowhere, and because tier 2 admits only literals the set test is exact membership, not
   wildcard matching. Every message must name the offending pattern *and* the way forward —
   `validate_onlyonce_test.go` asserts that, and asserts every case is accepted on a task
   without `only_once`.
   The tiers key off `Retry.Attempts`, but **D7's rejection on a child task keys off the
   policy naming anything** (`!Retry.IsZero()`): a rule naming only a delay is still an
   author expecting retries, and 0 attempts would let it through in silence. An empty
   policy is not "present" for this purpose — `retry: {}` and `retry: 0` are the absent key.
2. **`on_error` and `switch` reject unknown keys**, naming the list the key belongs to:
   they select with `code` and `case` respectively, and a silently dropped selector turns an
   on_error rule into a catch-all. Safe to do in the decoder because `SaveDefinition` stores
   `json.Marshal` of the decoded struct, so stored definitions are canonical.

## `timeout`: one grammar, two homes

A task's `timeout` is the delay action's slot set pointed at a deadline instead of a
wake-up, so both decode to the same `DelaySpec` (`for` / `until` / `tz`) and share
`delayArity` and the delayspec grammars. Three things break silently:

1. **`DelaySpec` must never gain an `UnmarshalJSON`.** `Action` embeds it — which is what
   keeps `{"type":"delay","for":"1h"}` flat on the wire — so a decoder on `DelaySpec` is
   promoted to `Action` and json hands it the *whole action object*: `type`, `url` and
   every other field decode to nothing, silently. The scalar-or-object shorthand therefore
   lives on the `Timeout` wrapper, which nothing embeds.
   `TestAction_DelaySpecDoesNotHijackDecode` is the regression test.
2. **Absent is not zero.** `timeout_ms: 0` used to spell "wait forever"; a duration slot
   cannot also carry a magic zero, so absence is now the only spelling for it. Anything that
   turns an absent timeout into a zero one — a marshaller dropping `omitzero`, a decoder
   defaulting the struct — makes every fetch task unrunnable, since a zero fetch budget is
   refused (an external clamps it instead; see below).
3. **`until` is confined to `external`, and a timeout is refused on the action types that
   ignore it** (`validateTimeout`). Both rejections exist because the alternative is
   silent: a timeout on a child task is simply never applied, and a fetch whose deadline
   has already passed builds an expired context, which `transport.ClassifyGoError` reports
   as `http.timeout` — an unknowable code, so on an `only_once` task it can never be
   retried, for a request that provably never left.

   The same asymmetry governs a deadline that resolves into the past at runtime, and the
   engine's two callers must keep disagreeing about it: an external **clamps** (it parks
   already due and raises `external.timeout`, the truthful code its `on_error` is written
   against), a fetch **refuses**. Making them agree either invents a lie or turns a
   legitimate late deadline — a re-arm, a resume from a long pause — into an uncatchable
   `engine.expression`.

## `retry`: a policy, not a count

An `on_error` rule's `retry` is `{attempts, delay, factor, max_delay}`, with the scalar
`retry: 3` desugaring to `{attempts: 3}`. Design and the survey behind the field set:
[docs/retry-policy.md](../../docs/retry-policy.md). The curve itself is
[internal/engine/CLAUDE.md](../engine/CLAUDE.md). Four things that break silently:

1. **The pre-policy `retries` key must stay in `ruleFieldHints`.** It is the one rename in
   this rule that is invisible when dropped: unknown keys are refused, but without the hint
   an author reads "unknown field" for a key that worked yesterday. Silently *accepting* it
   would be worse still — the rule keeps its `code` and its `goto`, so it still matches and
   still routes, and only never retries.
2. **`MarshalJSON` always writes the object form.** `SaveDefinition` stores the marshalled
   struct, so this is what canonicalises `retry: 3`; emitting the scalar back would leave
   two stored shapes for every later reader to handle.
3. **`RetryDuration` keeps the literal it parsed.** Marshalling the parsed length instead
   turns an authored `"90s"` into `90000` on read-back. It also refuses calendar units,
   because the curve scales the value and compares it to a ceiling — `"1mo"` has no length
   until a timezone and a start instant fix it.
4. **Nothing may embed `Retry`.** Same trap as `DelaySpec`: an `UnmarshalJSON` on an
   embedded struct is promoted to the outer type and silently swallows the whole object.

## Pointers

- `Action` decoding of `delay` (`for` / `until`, no `DisallowUnknownFields`) —
  [internal/delayspec/CLAUDE.md](../delayspec/CLAUDE.md).
- `Status.Terminal()` and what `paused` does *not* mean —
  [internal/db/CLAUDE.md](../db/CLAUDE.md).

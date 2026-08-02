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
2. **`on_error` and `switch` reject unknown keys**, naming the list the key belongs to:
   they select with `code` and `case` respectively, and a silently dropped selector turns an
   on_error rule into a catch-all. Safe to do in the decoder because `SaveDefinition` stores
   `json.Marshal` of the decoded struct, so stored definitions are canonical.

## Pointers

- `Action` decoding of `delay` (`for` / `until`, no `DisallowUnknownFields`) —
  [internal/delayspec/CLAUDE.md](../delayspec/CLAUDE.md).
- `Status.Terminal()` and what `paused` does *not* mean —
  [internal/db/CLAUDE.md](../db/CLAUDE.md).

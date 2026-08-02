# Path-sensitive process-output inference

**Status: implemented** for the process output boundary. The mid-process case (§5) is
deliberately deferred.

A process that reconverges from several branches writes its output by coalescing across
them:

```yaml
tasks:
  - id: send                      # succeeds → ends
    on_error: [{ code: [http.422], goto: $unsendable }]
    output: { ok: true,  reason: "" }
    switch: end
  - id: unsendable                # the error branch → ends
    output: { ok: false, reason: "$: error.code" }
    switch: end

output:
  ok: "$: outputs.send.ok ?? outputs.unsendable.ok"
```

Exactly one of the two tasks runs. The expression can never be null. Inference used to
type it `boolean|null` anyway, which forced every consumer — a parent's `result_schema`, a
caller reading the field — to declare a null that cannot occur.

## 1. Why it was nullable

The information needed was already computed and then discarded.

`outputTerminals` ([internal/validation/context.go](../internal/validation/context.go))
enumerates the terminal paths, one entry per way of ending, each carrying the set of task
outputs guaranteed present there:

```
terminal @send:        must = {send}
terminal @unsendable:  must = {unsendable}
```

`outputContextSets` then collapses that list into one required/optional pair by
**intersecting** the must-sets. The intersection is empty, so both outputs are merely
"optional", and after that step these two situations are indistinguishable:

- *a is set here, b is set there* — one of them always present
- *neither is ever set* — both genuinely absent

Both come out as "a: optional, b: optional". The inferencer never had a chance: by the time
`??` ran it saw two independently-nullable values, and a union of two nullables is nullable.

## 2. The fix: partition, don't teach the operator

`inferProcessOutput` ([internal/validation/generate.go](../internal/validation/generate.go))
type-checks the output expression **once per terminal** and joins the results. On each
terminal a task output is either its real type or, if that terminal cannot produce it,
exactly `{"type":"null"}`.

| | `outputs.send.ok` | `outputs.unsendable.ok` | `a ?? b` |
|---|---|---|---|
| terminal @send | `boolean` | `null` | left non-null → `boolean` |
| terminal @unsendable | `null` | `boolean` | left null → right → `boolean` |

`Join(boolean, boolean)` = `boolean`.

Nothing was added to `??`. The operator's existing rules — "a null left yields the right",
"a non-null left is a no-op" — already do the work once the environment is precise enough
to distinguish the paths. That is the whole design: **precision comes from the partition,
not from a special case in the operator.**

Two consequences follow for free, and both are the reason this shape was chosen over a
"coverage check" bolted onto `??`:

- **An uncovered terminal keeps it nullable.** A third way to end that sets neither output
  contributes `null ?? null` = `null`, and the join is `boolean|null`. Correct.
- **A genuinely nullable branch keeps it nullable.** If `send.ok` is declared `boolean|null`,
  then on `@send` the expression is `(boolean|null) ?? null` → `boolean|null`. Correct, and
  for the right reason: at runtime a real null in the left operand *does* fall through to an
  absent right operand. Coverage means a value is **present**, never that it is **non-null**.

## 3. What it required elsewhere

**Reading through a null yields null.** Modelling "absent on this terminal" as
`{"type":"null"}` only works if `outputs.gone.v` types as null instead of failing.
`lookupPropertyGuard` ([internal/schema/navigate.go](../internal/schema/navigate.go)) now
returns null for a property of a null.

This aligned inference with two things it already disagreed with: the same function already
returned null when *every* union variant was null, and the **evaluator** has always returned
nil for member access on a missing value — which is precisely why `a.x ?? b.x` works at
runtime. Inference was the odd one out.

The rule is narrow. A property of a string is still an error, an undeclared property of an
object is still an error (a typo must not become a silent null), and the unknown type `{}`
is still refused.

**A reference nothing can produce is still an error.** Absent-as-null applies only to tasks
reachable on *some* terminal. A task output no path produces is left out of every
per-terminal context, so `outputs.nosuch.v` fails as before rather than reading as null.

**`??` canonicalizes its union.** Independent of path sensitivity, and a real bug on its own:
`boolean ?? boolean|null` built `oneOf[{boolean},{boolean|null}]`, and `oneOf` means
*exactly one*. The value `true` matches both arms, so that schema **rejected every value it
described**. Canonicalizing folds it to `{"type":["boolean","null"]}`. A `$ref` arm blocks
the merge (`isSimpleType` requires `Ref == ""`), so a recursive output type stays symbolic
and finite.

**`StripNull` keeps its contract.** It dropped only whole `{"type":"null"}` arms, so a null
inside an arm's *type list* survived — while `HasNull`, which does look inside arms,
reported true. The two disagreed, and no chain of `?? default` could recover non-nullability
once the left had become a union. `stripNull` now recurses into inline arms; `hasNullResolved`
recurses into nested unions (a one-level scan under-reported null, the unsound direction)
with a cycle guard for recursive types.

## 4. Error messages

An expression is now checked several times, so a failure needs to say *where*:

- Fails on **every** terminal — an ordinary type error, reported plainly. Prefixing it with a
  path would be misdirection; the path is not what is wrong.
- Fails on **some** terminal while another type-checks — genuinely path-specific, reported as
  `on the path ending at task "b": …`. Without the prefix the author looks in the wrong place,
  because the expression reads fine against the branch they had in mind.

## 5. Deferred: mid-process task contexts

The same idiom appears inside a task reachable from two branches, and there it is **still
collapsed** — `outputs.a.v ?? outputs.b.v` read from task `c` remains nullable.

The output boundary was cheap because its terminals are already materialised as a list.
Task contexts come from `computeContextSets`, a fixpoint whose lattice element is *one*
set of ids, intersected across predecessors. Making it path-sensitive means carrying a
**set of alternative must-sets** — a DNF — which is exponential in the worst case and needs
a widening rule to terminate. That is a different piece of work with a different risk
profile.

The workaround is a trailing default (`?? false`), which is exactly what an author would
write anyway, and it now behaves correctly thanks to the `StripNull` fix above.

Reopen this if the mid-process case shows up in real definitions often enough to justify
the lattice change. The signal to watch for: definitions carrying a `?? default` whose
default is provably unreachable.

## 6. Rejected alternative: a coverage check inside `??`

Keep the single collapsed context, but give `inferNullCoalesce` access to the terminal sets;
when inferring `a ?? b`, extract the `outputs.<id>` roots each side references and declare
the result non-null if every terminal is covered by one of them.

Rejected. It needs syntactic root extraction, so it works only when the operands are
literally `outputs.X…` paths; it has to separately reason about whether the *property* is
non-null and not just whether the task ran; and it generalises to nothing — the same
reasoning would be wanted for `cond ? outputs.a.v : outputs.b.v`, for `outputs.a.v == null`,
and for every future construct. Case-splitting the environment gets all of those at once,
because it does not know anything about `??` at all.

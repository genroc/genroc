# Path-sensitive process-output inference

Status: **Built, except the mid-process case (§5).**

A process that reconverges from several branches writes its output by coalescing across them —
`send` ends on success, `unsendable` on its error branch, and exactly one of them runs:

```yaml
output:
  ok: "$: outputs.send.ok ?? outputs.unsendable.ok"
```

That expression can never be null, and must not type as `boolean|null`: every consumer (a
parent's `result_schema`, a caller) would have to declare a null that cannot occur.

## 1. Why it was nullable

`outputTerminals` ([internal/validation/context.go](../internal/validation/context.go)) lists one
entry per way of ending, each with the task outputs guaranteed present there. `outputContextSets`
**intersects** those must-sets into one required/optional pair, after which "a is set here, b
there" and "neither is ever set" are indistinguishable. Precision must be taken before that
collapse.

## 2. The fix: partition, don't teach the operator

The output expression is typed **once per terminal** and the results joined. On each terminal a
task output is its real type or, if that terminal cannot produce it, exactly `{"type":"null"}`.

**The partition is the context, not a walk.** `taskScopes.processOutputContext` builds one schema
with an `anyOf` arm per terminal, each arm naming its ending in `description`, and
`Schema.InferNode` ([internal/schema/infer.go](../internal/schema/infer.go)) types an expression
under each arm and joins. Anything handed the context — `genctl schema context`, whatever
generates from it — can therefore reproduce the checker's verdict; a flattened context would omit
what the checker knew.

| | `outputs.send.ok` | `outputs.unsendable.ok` | `a ?? b` |
|---|---|---|---|
| terminal @send | `boolean` | `null` | left non-null → `boolean` |
| terminal @unsendable | `null` | `boolean` | left null → right → `boolean` |

Nothing was added to `??`: its existing rules do the work once the environment distinguishes the
paths. **Precision comes from the partition, not from a special case in the operator.** Two
consequences hold for free:

- **An uncovered terminal keeps it nullable** (`null ?? null` on a third ending).
- **A genuinely nullable branch keeps it nullable**: coverage means a value is **present**, never
  that it is **non-null** — at runtime a real null on the left does fall through.

## 3. What it required elsewhere

**Reading through a null yields null** (`lookupPropertyGuard`,
[internal/schema/navigate.go](../internal/schema/navigate.go)), matching the evaluator, where
member access on a missing value is nil — which is why `a.x ?? b.x` works at runtime. The rule is
narrow: a property of a string, an undeclared property of an object (a typo must not become a
silent null) and a read through `{}` are still errors.

**A reference nothing can produce is still an error.** An output no terminal reaches is left out
of every arm, so `outputs.nosuch.v` fails rather than reading as null.

**`??` canonicalizes its union.** `boolean ?? boolean|null` would otherwise build
`oneOf[{boolean},{boolean|null}]`, and `oneOf` means *exactly one* — `true` matches both arms, so
that schema rejects every value it describes. Canonicalizing folds it to
`{"type":["boolean","null"]}`. A `$ref` arm blocks the merge (`isSimpleType` requires
`Ref == ""`), so a recursive output type stays symbolic and finite.

**`StripNull` and `HasNull` must agree.** `stripNull` recurses into inline arms, so a null inside
an arm's type list is removed; `hasNullResolved` recurses into nested unions (a one-level scan
under-reports null, the unsound direction) with a cycle guard for recursive types. If they
disagree, no chain of `?? default` recovers non-nullability.

## 4. Error messages

The arm's `description` names the path, so the message is available to anyone holding the
context. An expression failing on **every** terminal is reported plainly (the path is not what is
wrong); one failing on **some** terminal while another types is reported as `on the path ending
at task "b": …`.

## 5. Deferred: mid-process task contexts

The same idiom appears inside a task reachable from two branches, and there it is **still
collapsed** — `outputs.a.v ?? outputs.b.v` read from task `c` remains nullable.

The output boundary was cheap because its terminals are already materialised as a list, and
since §2 the *representation* is no longer the obstacle either: a union context is what a
path-sensitive task scope would be, and inference already distributes over one. What is left is
computing the alternatives. Task contexts come from `computeContextSets`, a fixpoint whose
lattice element is *one* set of ids, intersected across predecessors. Making it path-sensitive means carrying a
**set of alternative must-sets** — a DNF — which is exponential in the worst case and needs
a widening rule to terminate. That is a different piece of work with a different risk
profile.

The workaround is a trailing default (`?? false`), which is exactly what an author would
write anyway, and it now behaves correctly thanks to the `StripNull` fix above. Since guard
narrowing there is a second one: route on each branch (`case: outputs.a != null`, then
`case: outputs.b != null`, then a `panic`), and each target reads its branch non-null.

Reopen this if the mid-process case shows up in real definitions often enough to justify
the lattice change. The signal to watch for: definitions carrying a `?? default` whose
default is provably unreachable.

Note that [guard-narrowing.md](guard-narrowing.md) (built 2026-09-15) removes a *different*
slice of the same annoyance and was tractable, because it refines one reference at a time rather
than correlating two. It does not close this section: after `case: outputs.a != null`, the
fallthrough edge knows `outputs.a` is null, not that `outputs.b` is present.


## 6. Rejected alternative: a coverage check inside `??`

Keeping the collapsed context and teaching `inferNullCoalesce` to check that the `outputs.<id>`
roots of its operands cover every terminal. It works only on literal `outputs.X…` operands, must
separately reason about property nullability, and generalises to nothing (`?:`, `== null`, every
future construct). Case-splitting the environment gets all of those at once.

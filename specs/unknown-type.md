# The `unknown` type: opaque results, narrowed at the boundary

Status: **IMPLEMENTED, 2026-07-31** (designed 2026-07-21). Authored as the empty
schema `{}` — there is no keyword; see `examples/polling-task/` for the worked
example. The **Infer** mode described under "Three ways a parent types a child
result" is *not* built — it remains a proposal, and is the one part of this document
that still describes intended work rather than behaviour.

> A more ambitious version of this idea — passing schemas as values for true
> generic processes with call-site specialization — was considered and
> **deliberately dropped** as not worth its complexity. It is preserved for the
> record in the appendix "Not planned: schema-valued generics".

## The idea in one line

Give the type system an `unknown` type: a value a process **handles but does not
inspect**. Anyone who wants to *use* it must validate (narrow) it first — exactly
the way you already have to declare a `result_schema` to read a `fetch` response.

`fetch` is already an "opaque source you must type at the boundary". This lets a
**child process** play the same role. It is not a new concept so much as making
the existing one uniform across `fetch` and processes.

## Motivation

Today `result_schema` must be known statically: it types `self.result` for
downstream expressions, so it has to be a literal on the action. That means a
process cannot hand back data whose shape it doesn't itself know — every result
must be fully typed at the point it is produced.

Often a process legitimately doesn't care what's inside a payload — it fetches it,
carries it, and returns it. Forcing it to declare the shape is both awkward and
sometimes impossible (the shape is the *caller's* concern, not the process's).
`unknown` lets the process stay agnostic and pushes the "what shape is this?"
decision to whoever actually reads the value.

## What `unknown` is — the `{}` top type

`unknown` **is** the engine's empty-node top type `{}`. There is no keyword and no
alias: you write `{}`, and every code path — subset, navigation, inference, the
solver — already treats it correctly. Nothing was added to the schema language.

The empty node already had the three behaviors we need (this is `unknown`,
emphatically **not** `any`):

- **reads rejected** — `self.result.foo` errors (`lookupProperty`,
  `internal/schema/navigate.go`). A black box. The empty node now gets its own
  message there ("the value is unknown (its schema is {})") rather
  than the generic "schema has no properties", since it is the one such case an
  author reaches deliberately and can act on.
- **`{} ⊄ T`** for any typed `T` (`internal/schema/subset.go`) — cannot flow
  into a typed slot without narrowing.
- **`X ⊆ {}`** — assignable into a schema-free slot (e.g. `output`).

So an `unknown` value can be **exported or nested in a known structure**
(`{ data: self.result }`) and nothing else. Runtime `conform` against the real
schema, when narrowing happens, enforces the actual shape.

### Saying you meant it

`{}` has one real weakness: it does not announce intent. `payload: {}` in a long
schema reads like an unfinished stub. Two things answer that, neither of which needs
engine support:

- a **YAML comment** at the authoring site — genroc definitions are authored in YAML,
  and a comment is invisible to every tool;
- a **`description`** — `isEmptyNode` ignores it (along with `secret` and `default`),
  so `{"description": "opaque payload"}` is still exactly the top type. Unlike a
  comment, it survives into the stored definition and the editor, and
  `canonicalizeNode` strips it before type comparison, so it provably cannot perturb
  inference.

The rest of the "no other fields" rule enforces itself: adding any shape keyword makes
the node non-empty, so it simply stops being the top type — there is no rule to write.

### A dedicated `type: unknown` was built, then dropped

Worth recording, because it will be re-proposed. A `type: unknown` keyword (erased at
parse to `{}`) was implemented and reverted. Two reasons:

1. **It would have been genroc's only divergence from JSON Schema.** `type` is
   restricted to the meta-schema's `simpleTypes` enum, so a standard validator rejects
   `type: unknown` on sight. Everything else genroc does is *subsetting* — it accepts
   less (no `allOf`, no boolean `additionalProperties`, no unrecognised keywords) plus
   one inert custom keyword (`secret`), which JSON Schema permits because validators
   ignore keywords they don't know. Staying a pure subset means outside tooling —
   an OpenAPI importer generating schemas, a meta-schema validating the authoring
   surface in an editor, third-party resolvers reading a `result_schema` off the
   external-tasks API — all work unmodified.
2. **The explicitness it bought was illusory.** Because the keyword was erased at
   parse, it never reached the database, the API, or `genctl get`. It existed only in
   the source file — which is YAML, which has comments, and which can carry a
   `description` that *does* survive. The one benefit was confined to the single
   medium that already had two better mechanisms.

A custom keyword (`{"unknown": true}`) would have kept the authored form
meta-schema-valid, since JSON Schema ignores unrecognised keywords, and would even
degrade to `{}` under a standard validator. It was not chosen either: genroc's parser
rejects unrecognised keywords by allowlist, so it does not play by that convention,
and it reads worse than the alternatives above.

### Omission stayed an error

A `result_schema` that is *omitted* still produces a result omitted from the
inference context (`typed=false`, `internal/validation/infer.go`): unreadable and
**unexportable**. That was left alone deliberately. Making omission mean `unknown`
was on the table and rejected: it would erase the difference between "I meant this
to be opaque" and "I forgot to type it", turning a forgotten schema into a value
that fails much later at some consumer's boundary. The distinction is free, since
omission and `{}` are already separate states in the inference context. The error
message for the omitted case names both fixes.

## Narrowing — the one load-bearing rule

An `unknown` enters the typed world only by being **narrowed** with a concrete
schema, and the narrowing is **runtime-checked**. The narrowing point already
existed in the syntax: the **`result_schema` on the action that produced the
value.** For a child, the parent writes `result_schema` on the child action; the
child's actual output is conformed against it at collect time
(`resolveAndValidateChildOutput`, `internal/engine/collect.go`, reading the slot from
the parent's pinned definition), so the narrowing is sound.

The **one rule added** is: allow a `result_schema` to narrow a `{}` result — i.e.
permit `{} → T` **through a `result_schema` only** (runtime-conformed), while
keeping `{} ⊄ T` everywhere else. Concretely: `subsetCtx` gained a `narrow` flag
that flips the single `isEmptyNode(sub) → false` rule, exposed as
`Schema.NarrowsTo`, and `checkChildOutputType` calls that instead of `IsSubset`.
Because the flag is checked inside the recursion, an unknown narrows **at any
depth** — which is what the per-field split below actually needs. Nothing else in
the engine can reach the relaxed relation.

An unknown handed to a typed **input** is still rejected, and the reason is not
symmetry: nothing conforms a child input against the callee's schema on the
parent's behalf, so there would be no check standing behind the claim. The
privilege belongs to `result_schema` precisely because that slot has a runtime
conform.

Everything else in this design was reuse. That rule was the whole build — there is
no syntax to go with it.

## Three ways a parent types a child result

A child action's result-type slot has **three modes**, trading coupling for safety
and ergonomics. Two are built; Infer is not.

| Mode | Syntax | `self.result` | Coupling | On a version bump |
|---|---|---|---|---|
| **Pin** (safeguard) | explicit schema | that schema | decoupled — parent validates without the child | `childOutput` narrows to schema; drift **fails loudly** |
| **Infer** (inherit) — *not built* | `infer` marker *(open)* | the child's computed output | **coupled** — child must be defined | auto-adopts the new output; fails only where a changed field is *used* |
| **Unknown** (opaque) | `{}` | `{}` | decoupled | n/a — opaque; the consumer narrows |

- **Pin** is the pre-existing behaviour, reframed as a *safeguard*
  (`checkChildOutputType`, `internal/validation/validate_children.go`). The parent
  validates standalone, and if the child's output drifts the check fails. This is
  the "explicit type annotation at a public boundary" — you restate the return type
  as a stability gate. It is also the slot that does the narrowing, so pin and
  unknown are not alternatives: pinning a schema onto a child's unknown field *is*
  the narrowing.
- **Infer** would copy the child's *computed* output (`Generate(child)`) into the
  slot — `self.result` becomes the child's output verbatim, no subset check. This is
  "import a function; don't re-state its return type", and it is what would make
  child-processes-as-functions pleasant. It **tightly couples** parent to child: the
  child must be defined at parent-validation time or it is a hard error. Version
  pinning is what makes it safe — a pinned version is immutable, so there is no
  drift *within* a version; bumping the pin re-inherits the new output and re-checks
  the parent's usage.
- **Unknown** (the rest of this document): `{}`, opaque, consumer narrows.

**Infer is a much larger build than the `unknown` core** — which is why the core
shipped alone. It makes output inference **recursive across process boundaries** —
`Generate` today "infers the child's output from its own tasks … no getter, so it
does not recurse across the tree"
(`validate_children.go`). So Infer needs: cross-process output resolution;
cycle handling for mutually-recursive processes (reuse the collapse-or-keep /
productivity machinery, now at process granularity); memoization per pinned
`(process, version)`; and a registration-ordering rule (child before parent, or
forward-reference revalidation). This is the ergonomic linchpin for the
[custom-tasks](custom-tasks.md) vision, and its main cost.

**Infer composes with unknown:** a child whose output is `unknown`, inherited via
Infer, yields `unknown` upward; pinning a concrete schema onto an unknown-output
child is exactly the `{} → T` narrowing above.

## How a value moves

An `unknown` value has exactly two legal moves — mirroring TypeScript's `unknown`:

1. **Into a schema-free / `unknown` slot** directly (`{} ⊆ {}`) — pure forwarding;
   nobody downstream can read it either.
2. **Into a typed input only after narrowing** it with a `result_schema` at the
   producing action.

Passing an `unknown` straight into a typed `input` is correctly **rejected**
(`{} ⊄ T`) — the same refusal TypeScript makes when you hand an `unknown` to a
function expecting a concrete type.

## `unknown` is per-field: forward vs. act-on

The rule is per-field, not per-process: **data a process reads to make its own
decisions must stay typed; only data it forwards untouched can be `unknown`.** Most
real processes are a mix.

The poller (`examples/polling-task/`) is the canonical mix:

- the job's response body — the child never inspects it, just returns it. **This**
  is the `unknown`, validated when the *parent* reads the child result. The poller
  gets away with a fully opaque body because it drives its loop off the HTTP status
  instead (a 202 arrives as the catchable code `http.202`), so it needs nothing from
  the payload.
- `attempts` — the child counted the polls itself, so it knows the type. The
  parent's `result_schema` narrows the first and simply restates this one.

So the poller's only behavioral change: the payload is validated **when the parent
reads the child result**, not right after the fetch. Same runtime guarantee,
different clock. Because narrowing runs the parent's schema over the whole child
output, it also **strips** keys the parent didn't declare — a job may return more
than the caller asked for.

## Consequence: validation is lazy, not eager

Because the check moves from *at the source* to *at consumption*:

- A malformed payload flows through the child and fails at the **parent
  boundary** — later, further from the cause, and **outside the child's own
  `on_error`/retry scope**. Today a `fetch` `result_schema` violation fails the
  fetch task itself, where the child can retry it. That error locality shifts.
- An `unknown` that no one ever narrows is **never validated at all**. Harmless
  (you couldn't read it without narrowing), but lazy-by-design.
- Multiple consumers may each narrow the same `unknown` to **different** schemas,
  each runtime-checked independently. That's a feature.

None of these hurt a stable poller; they're the things to keep in mind before
reaching for `unknown` on data whose shape you don't trust. The last two are pinned
by the example's integration test, which asserts that a payload violating the
parent's narrowing lets the **child complete** and fails the **parent**.

## Ledger

**What was built** — one relation and two error messages. No syntax:
- `Schema.NarrowsTo` — `IsSubset` with the `isEmptyNode(sub)` rule flipped — used
  by `checkChildOutputType` and nowhere else, backed by the collect-time conform.
- Two error messages that name the fix: reading through an unknown, and exporting
  an untyped result.

Also, opportunistically: unrecognised **type names are now rejected at
registration**. They previously parsed and then rejected every value, so
`type: strng` produced an unsatisfiable schema in silence. It is a `CheckDoc`
validity rule rather than a decode rule on purpose: the decoder also runs over
definitions already in the DB, so enforcing it there would turn a legacy bad name
from "fails at runtime" into "undecodable", taking out the whole `ListDefinitions`
page it sits on. `validTypes` is exactly the JSON Schema `simpleTypes` enum.

**Reused as-is:**
- `{}` top type (reads rejected, `{} ⊄ T`, `X ⊆ {}`), and its pass-through
  validation — an empty node returns a value verbatim, which is what makes
  forwarding non-destructive.
- `result_schema` as the narrowing point + its runtime `conform`.
- Everything in the recursion / inference machinery — untouched, as predicted.

**Trap avoided:**
- Do **not** represent `unknown` as a dangling `$ref`. A `$ref` to a missing def
  is a hard error on any touch (`navigate.go`), and an unresolved ref in
  `super` position of `IsSubset` is silently treated as top (`derefSubset`,
  `subset.go`) — unsound. `{}` is the designed top type; use it.

## Open questions (settle later)

- Infer mode's cross-process inference: cycle handling across mutually-recursive
  processes, `(process, version)` memoization, and registration ordering (child
  before parent). *(open — the main cost of Infer, and the only unbuilt mode)*
- Author-time ergonomics beyond the read path: when a typed **input** slot rejects
  an `unknown`, the message is still a bare subset failure and should say why
  narrowing isn't available there. *(open)*
- Harden `derefSubset` to error on an unresolved `super` regardless — a latent
  bug independent of this feature, still unfixed. *(open)*

**Settled while building:** the child-output conform *is* uniform. All three
collectors — `buildSingleChildOutput` / `buildMapChildOutput` /
`buildListChildOutput` — funnel through `resolveAndValidateChildOutput`, each passing
the `result_schema` its own action slot declares, so narrowing is backed by a real
check for `child`, `child_map` and `child_list` alike. (The schema was originally
copied onto every child row at spawn as `_spawn_result_schema`; it is now read from
the parent's task — see version-compatibility.md §5a for why. The argument is
unchanged; only the schema's address is.)

---

## Appendix — Not planned: schema-valued generics

Preserved for the record. This was the more ambitious direction: let a parent pass
a **schema as a value** so a process is generic over its result type
(`fetch<T>(config: { …, result_schema: Schema<T> }): T`), with the parent's result
type *derived* from the schema it passed rather than re-declared.

It was dropped because it buys little over `unknown` + narrow-at-the-boundary
while carrying a large, permanent cost. The pieces it would have required, none of
which the engine has today:

- A first-class **`Schema` builtin type** (a meta-schema over the engine's keyword
  allowlist) plus an **untrusted-schema boundary** (parse a `map[string]any` input
  through the strict path, normalize it *in isolation*, validate it against the
  meta-schema).
- **Call-site specialization**: `Generate(child)` is caller-independent and the
  solver memoizes on def-name only (`internal/schema/solver.go:369-372`), so
  deriving a result type from a caller-supplied schema needs a genuinely new keying
  axis (per-instantiation namespacing by the bound schema's identity).

The recursion machinery itself would *not* have needed changing — a self-contained,
closed, productive injected schema forms its own SCC and rides the existing solver
(termination is structural: finite member set + `maxSolvePasses` + the 64KB
widening cap, with productivity enforced in `CheckDoc`). But the specialization
axis and the untrusted-schema surface are a permanent tax on the engine's hairiest
subsystem, justified by no concrete, recurring use case. If such a use case appears
— genuinely reusable processes callers invoke with differing schemas, where writing
the schema once instead of at each boundary matters — this is where to resume.

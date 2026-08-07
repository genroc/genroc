# The `unknown` type: opaque results, narrowed at the boundary

Status: **implemented 2026-07-31** (designed 2026-07-21). Authored as the empty schema
`{}` — there is no keyword; worked example in `examples/polling-task/`. The **Infer**
mode below is the one part still unbuilt. Invariants live in
[internal/schema/CLAUDE.md](../internal/schema/CLAUDE.md).

## The idea

An `unknown` is a value a process **handles but does not inspect**; whoever wants to
read it must narrow it first — exactly how a `fetch` response already needs a
`result_schema`. This makes a child process able to play the same opaque-source role,
uniformly. Motivation: a forwarding process should not have to declare a shape that is
the *caller's* concern.

## `unknown` IS the `{}` top type

No keyword, no alias — the empty node already had the three needed behaviours (this is
`unknown`, emphatically not `any`): reads through it are rejected (with a dedicated
message, since it is the one such case an author reaches deliberately); `{} ⊄ T` for
any typed T; `X ⊆ {}` — so it can be exported or nested (`{data: self.result}`) and
nothing else.

- **Announcing intent:** a YAML comment, or a `description` — `isEmptyNode` ignores it,
  it survives into storage and the editor, and canonicalization strips it before type
  identity, so it provably cannot perturb inference.
- **A `type: unknown` keyword was built, then dropped**: it would have been genroc's
  only divergence from JSON Schema (everything else is subsetting, so outside tooling
  works unmodified), and the explicitness was illusory — erased at parse, it existed
  only in the one medium that already has comments and descriptions. A custom
  `{"unknown": true}` keyword was rejected too (the parser rejects unknown keywords by
  allowlist, so genroc doesn't play by the ignore-unknown convention).
- **Omission stayed an error**: an omitted `result_schema` still yields an unreadable,
  unexportable result. Making omission mean `unknown` would erase "I meant opaque" vs
  "I forgot", deferring the failure to some distant consumer. The states were already
  distinct; the error message names both fixes.

## Narrowing — the one load-bearing rule

An unknown enters the typed world only through a **runtime-checked** narrowing, and the
narrowing point already existed: the `result_schema` on the producing action, conformed
at collect from the parent's pinned definition. The entire build was one relation:
`Schema.NarrowsTo` = `IsSubset` with the `isEmptyNode(sub)` rule flipped, checked
inside the recursion (so an unknown narrows at any depth), used by
`checkChildOutputType` and nowhere else. A typed **input** still rejects `{}` — not for
symmetry, but because nothing conforms a child input on the parent's behalf; the
privilege belongs exactly where a real check stands behind it.

## Three ways a parent types a child result

| Mode | Syntax | Coupling | On a version bump |
|---|---|---|---|
| **Pin** (built) | explicit schema | decoupled | drift fails loudly — the annotation is a stability gate, and pinning onto an unknown *is* the narrowing |
| **Infer** (not built) | marker TBD | coupled — child must be defined | auto-adopts; fails only where a changed field is used |
| **Unknown** (built) | `{}` | decoupled | n/a — consumer narrows |

Infer is the ergonomic linchpin of [custom-tasks.md](custom-tasks.md) and a much larger
build — it makes output inference recursive across process boundaries: cross-process
resolution, cycle handling at process granularity (reusing collapse-or-keep /
productivity), `(process, version)` memoization, and a registration-ordering rule.
It composes with unknown (an inherited unknown stays unknown; pinning onto it narrows).

## Consequences (deliberate)

Validation moves from source to consumption: a malformed payload fails at the *parent*
boundary, outside the child's own retry scope; an unknown nobody narrows is never
validated at all; multiple consumers may narrow the same value to different schemas,
each checked independently. The narrowing conform also **strips** undeclared keys. All
pinned by the example's integration test (child completes, parent fails).

The per-field rule of thumb: data a process reads to decide must stay typed; only data
it forwards untouched can be unknown. The poller is the canonical mix — opaque body
(drives its loop off the HTTP status instead), typed `attempts`.

## Ledger

Built: `NarrowsTo`, two error messages that name the fix, and — opportunistically —
unrecognised **type names now rejected at registration** (they used to parse into
silently-unsatisfiable schemas; a CheckDoc rule, not a decode rule, so legacy rows stay
decodable). Reused untouched: the `{}` top type and its pass-through validation, the
`result_schema` conform, all recursion machinery. **Trap avoided:** do not represent
unknown as a dangling `$ref` — a missing def is a hard error on touch, and an
unresolved super in `IsSubset` silently reads as top (unsound).

Open: Infer's cross-process machinery; a better message when a typed input rejects an
unknown; hardening `derefSubset` to error on unresolved super (latent, independent,
still unfixed). Settled while building: all three child collectors funnel through
`resolveAndValidateChildOutput`, so narrowing is backed by a real check for
`child`/`child_map`/`child_list` alike (the schema's *address* moved off the spawn row
— version-compatibility.md §3a).

## Appendix — not planned: schema-valued generics

Passing schemas as values for generic processes with call-site specialization was
considered and dropped: it needs a first-class `Schema` type with an untrusted-schema
boundary, and call-site specialization breaks the solver's per-definition memoization
(a genuinely new keying axis) — a permanent tax on the hairiest subsystem for no
recurring use case. The recursion machinery itself would have coped. Resume here if
genuinely reusable schema-parameterised processes ever materialise.

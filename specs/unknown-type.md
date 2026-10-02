# The `unknown` type: opaque results, narrowed at the boundary

Status: **Built, except the Infer result-typing mode.** Authored as `{}`; worked example in
`examples/polling-task/`. Invariants live in [internal/schema/CLAUDE.md](../internal/schema/CLAUDE.md).

## The idea

An `unknown` is a value a process **handles but does not inspect**; whoever reads it must narrow
it first, as a `fetch` body needs a `responses` schema. A forwarding process should not have to
declare a shape that is the *caller's* concern.

## `unknown` IS the `{}` top type

No keyword: the empty node already behaves as `unknown` (emphatically not `any`). Reads through
it are refused (`ErrUnknownValue` — a dedicated message, since it is the one such case an author
reaches deliberately); `{} ⊄ T` for any typed T; `X ⊆ {}`, so it can be exported or nested
(`{data: self.result}`) and nothing else.

- Intent is announced with a YAML comment or a `description`: `isEmptyNode` ignores it and
  canonicalization strips it, so it cannot perturb inference.
- Rejected: a `type: unknown` keyword. It would be genroc's only divergence from JSON Schema
  (everything else is subsetting, so outside tooling works unmodified), and it would be erased at
  parse anyway. A custom `{"unknown": true}` fails the keyword allowlist.
- **Omission stays an error**: an omitted `result_schema` yields an unreadable, unexportable
  result. Omission meaning `unknown` would erase "I meant opaque" vs "I forgot" and defer the
  failure to a distant consumer; the message names both fixes.

## Narrowing — the one load-bearing rule

An unknown enters the typed world only through a **runtime-checked** narrowing: the parent's
`result_schema` (or `raises` entry), conformed at collect against the parent's current task
schema by `resolveAndValidateChildOutput` for `child`, `child_map` and `child_list` alike; a
failure is a catchable `result.invalid`. Statically that is `Schema.NarrowsTo` — `IsSubset` with
the `isEmptyNode(sub)` rule flipped inside the recursion, so an unknown narrows at any depth —
used by `checkChildOutputType` and `checkDeclaredRaises` and nowhere else. A typed **input** still
refuses `{}`: nothing conforms a child input on the parent's behalf, and the privilege belongs
only where a real check stands behind it.

## How a parent types a child result

| Mode | Syntax | Coupling | On a version bump |
|---|---|---|---|
| **Pin** (built) | explicit schema | decoupled | drift fails loudly — the annotation is a stability gate, and pinning onto an unknown *is* the narrowing |
| **Pin, spread** (built) | `<<: "$process: ./child.yaml"` | decoupled on the wire; coupled to a file at author time | drift fails loudly — a Pin written by reference ([source-resolution.md](source-resolution.md) §`$process`) |
| **Infer** (not built) | marker TBD | coupled — child must be defined | auto-adopts; fails only where a changed field is used |
| **Unknown** (built) | `{}` | decoupled | n/a — consumer narrows |

Infer is the ergonomic linchpin of [custom-tasks.md](custom-tasks.md) and a much larger
build — it makes output inference recursive across process boundaries: cross-process
resolution, cycle handling at process granularity (reusing collapse-or-keep /
productivity), `(process, version)` memoization, and a registration-ordering rule.
It composes with unknown (an inherited unknown stays unknown; pinning onto it narrows).

**Most of what Infer is wanted for is reachable at author time instead.** The spread form
resolves another definition's types into the call site and stores the result, so what lands is
a Pin and none of the runtime machinery above is needed. That is the argument for leaving Infer
unscheduled, and the reason the row above is a *variant of Pin* rather than a fourth mode.

It pays for that in exactly one place. The spread graph must be **acyclic**, so a self- or
mutually recursive process cannot type itself by reference, and one edge of the cycle falls back
to a written Pin or to Unknown ([source-resolution.md](source-resolution.md) §Ordering). Of the
four costs above, cycle handling is the one that does not disappear — it moves to the author.

## Consequences (deliberate)

Validation moves from source to consumption: a malformed payload fails at the *parent*
boundary, outside the child's own retry scope; an unknown nobody narrows is never validated;
several consumers may narrow one value to different schemas, each checked independently. The
narrowing conform also **strips** undeclared keys. Pinned by
`tests/integration/examples_polling_test.ts`.

Data a process reads to decide must stay typed; only data it forwards untouched can be unknown.
The poller is the canonical mix — opaque body, typed `attempts`.

## Traps

- Do not represent unknown as a dangling `$ref`: a missing def is a hard error on touch, and an
  unresolved super in `IsSubset` silently reads as top (unsound).
- Unrecognised type names are refused by `CheckDoc` at registration, not at decode, so stored
  rows stay decodable.

## Open

Open: Infer's cross-process machinery; a better message when a typed input rejects an
unknown; hardening `derefSubset` to error on unresolved super (latent, independent,
still unfixed).

## Appendix — not planned: schema-valued generics

Passing schemas as values for generic processes with call-site specialization was
considered and dropped: it needs a first-class `Schema` type with an untrusted-schema
boundary, and call-site specialization breaks the solver's per-definition memoization
(a genuinely new keying axis) — a permanent tax on the hairiest subsystem for no
recurring use case. The recursion machinery itself would have coped. Resume here if
genuinely reusable schema-parameterised processes ever materialise.

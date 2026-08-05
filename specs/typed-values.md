# Typed values: `$:` expressions, `${}` interpolation, structured literals

Status: **implemented — all four phases landed (2026-07-22).** Supersedes the `{{ }}`
grammar outright (prototype, no back-compat). Related: [unknown-type.md](unknown-type.md)
(orthogonal). This file is the design record; the syntax decisions ledger is at the end.

## The idea

At **any node** of a value the author writes the structure literally in YAML
(objects/arrays/scalars, expressions at the leaves) **or** hands the whole subtree to
one typed expression (`$:`). Either way the result is checked by its inferred type
against the slot's schema where one exists. The old `{{ }}` grammar's defect — whether a
value kept its type depended on invisible surrounding whitespace — is removed by making
the two intents two syntaxes:

| you write | meaning | result type |
|---|---|---|
| `"$: EXPR"` | typed expression, whole leaf (quoted string; leading whitespace ok) | inferred type of EXPR |
| `…${ EXPR }…` | interpolate into surrounding text, anywhere | always `string` |
| plain text | literal string | `string` |
| `42` / `true` / `null` / `[…]` / `{…}` | structured literal | the literal's type |

`$:` computes a value, `${}` builds a string — they never fight over type.
Interpolating a structure is a uniform error; concatenation inside a typed leaf uses the
expression language's `+`, never `${}`.

**The YAML footgun, handled by docs not mechanism:** `$:` mirrors YAML's own mapping
syntax, so an unquoted `$:` leaf either errors or silently parses to `{"$": "..."}`.
Rule of thumb: quote every `$:` leaf, and any expression containing `: `. Interpolation
carries no colon, so plain templates need no quoting. (Reserving `$` as an object key
with a did-you-mean hint is the recorded-but-untaken escape hatch if this proves
error-prone.)

**Expression-only positions never take a marker.** Where the required type is a fixed
non-string (switch `case` → boolean, `over` → array, delay `ms` → number), literal text
is never meaningful, so the field is one bare expression — no `$:`, no `${}`. String
positions (`url`, `method`) stay template positions because literal text *is* the common
case there. The dividing line: templates make sense ⇒ template position; they don't ⇒
expression-only, bare.

## Grammar and inference

```
Value = string | "$:" expr | number | bool | null | [Value, …] | {key: Value, …}
```

Literal-vs-expression is structural at authoring time; a whole subtree can always be
replaced by one `$:` expression, which is why `$:` at a slot root gives "the whole
payload as one expression" for free. Inference: template → `string` always (sub-
expressions still inferred for non-null/secret/lazy-load; the type-preserving `single`
path is deleted); `$:` → `expression.Infer`; array literal → `array<join>` with `[]` as
provably-empty `maxItems: 0` (the `?? []` idiom — same lesson as
[map-expressions.md §5](map-expressions.md)); object literal → closed, all required;
scalar → its kind (`json.Number` splits integer from number). `IsSubset` gained the
array arm. Null at a slot root means absent (pointer-nil); nested null is a value.

Two operations, the engine's existing split: `Fits` (author time — inferShape then
IsSubset; untyped into typed refused) and eval + `conform` (runtime — validates and
fills defaults).

## Escaping — `$`-doubling, not backslash

The draft's backslash design was **abandoned**: `\$` is an invalid escape in JSON and
double-quoted YAML, so `\${` breaks in exactly the quoting styles people use. `$` is an
escape char in neither host, so `$$` is collision-free everywhere: `$$` → literal `$`,
`$${` → literal `${`, leaf-leading `$$:` → literal `$:`. A `$` not forming `${` or
leaf-`$:` is already literal (`$5.00`, `$HOME` need nothing). The two-layer rule stands:
the template layer unescapes markers only; inside `${…}`/`$:` bodies the expression
lexer does its own string escapes (`'a\tb'`) — each region unescaped by exactly one
layer, boundary at the marker. Block scalars work (`$:` tolerates leading whitespace, so
residual indentation does not defeat detection).

## Where it applies

- Free projection, no schema (task/process `output`, fetch `body`, external `input`):
  grammar applies, `Fits` skipped.
- Target schema exists (child `input` ⊆ input_schema; `headers` against
  `object<string>`): checked.
- String positions (`url`, `method`): templates, checked non-null against `string`.
- Expression-only (`case`, `over`, `ms`): bare expressions; `ms` also takes a bare
  number.
- **Never expressions, by design:** `id`, `type`, child `name`/`version`,
  `result_schema`, `accepted_status`, fault codes/messages, `on_error` codes —
  downstream analysis needs their concrete values.

Deferred, with this grammar as prerequisite: per-action payload schemas (collapsing
`validateActionRequiredFields` into one `Fits`), the fetch payload pull-out, the
`unknown` result type.

## Editor schema

The generated JSON Schema (`GET /process-schema.json`, yaml-language-server) is kept
live through one transform: `relax(S)` = every node becomes `node | string` (the
expression escape hatch), applied recursively. Decisions that bit:

- **`anyOf`, not `oneOf`** — the string branch overlaps string leaves and
  number/integer overlap each other; `oneOf` spuriously rejects both (the same
  exclusivity trap as the empty-array union).
- **Array items are permissive `{}`**, not `$ref ModelShape`: openapi-typescript emits
  an array-of-indexed-self-reference as an eager cycle tsc rejects (TS2502); object
  recursion — the common structured case — is kept.
- `relax` is implemented over a shared descent combinator (`mapChildren` — the single
  definition of "where sub-schemas live"), so new schema keywords are picked up in one
  place; implemented node-side, which is what lets it label every string position
  without clobbering author descriptions.
- `description` became a first-class schema keyword for this: accepted, preserved,
  exposed — and **stripped by canonicalization** so it never affects type identity or
  `IsSubset`.

## Ledger (as built)

- Phases: (1) grammar+inference widened; (2) `$:` leaf; (3) the breaking `{{ }}`→`${ }`
  retarget + template `single` deleted + `$$` escaping + ~166 TS sites and all fixtures
  migrated with a parse-aware tool; (4) editor schema generated from
  `internal/shape/editor.go` (formerly hand-written literals), guarded by
  `TestProcessSchemaShape`.
- Sigils settled: `$:` and `${}`; `$()` rejected (reads as a call). No trailing-space
  rule.
- Open: author-time strictness on unknown object keys (runtime conform strips; `Fits`
  should probably reject so editors flag typos). Homogeneous arrays only (matches the
  engine's single `items`). Object spread (`...$:`) deferred — order-dependent override
  needs an ordered representation, and both `...` and `$:` need quoting; net-new in the
  expression language too.

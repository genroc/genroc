# Typed values: `$:` expressions, `${}` interpolation, structured literals

Status: **Built.** Related: [unknown-type.md](unknown-type.md) (orthogonal).

## The idea

At any node of a value the author writes the structure literally (objects, arrays, scalars,
expressions at the leaves) or hands the whole subtree to one typed expression. Whether a value
keeps its type must not depend on invisible surrounding whitespace, so the two intents are two
syntaxes:

| you write | meaning | result type |
|---|---|---|
| `"$: EXPR"` | typed expression, whole leaf (quoted string; leading whitespace ok) | inferred type of EXPR |
| `…${ EXPR }…` | interpolate into surrounding text, anywhere | always `string` |
| plain text | literal string | `string` |
| `42` / `true` / `null` / `[…]` / `{…}` | structured literal | the literal's type |

`$:` computes a value, `${}` builds a string — they never fight over type. Interpolating a
structure is an error; concatenation inside a typed leaf uses the expression language's `+`.
`$()` was rejected: it reads as a call.

An unquoted `$:` leaf is YAML mapping syntax, so it errors or parses to `{"$": "..."}`; the docs
say to quote it.

**Expression-only positions never take a marker.** Where the type is a fixed non-string (switch
`case` → boolean, `over` → array), literal text is never meaningful, so the field is one bare
expression. String positions (`url`, `method`) stay templates because literal text is the common
case there.

## Grammar and inference

```
Value = string | "$:" expr | number | bool | null | [Value, …] | {key: Value, …}
```

- template → `string` always (its sub-expressions are still type-checked); `$:` → the
  expression's inferred type.
- array literal → `array<join>`; `[]` is provably empty (`maxItems: 0`), the `?? []` idiom
  ([map-expressions.md](map-expressions.md) §Typing).
- object literal → closed, all keys required; scalar → its kind (a number spelled with no
  fraction or exponent is `integer`, as in an expression).
- null at a slot root means absent (pointer-nil); a nested null is a value.

Author time: `Shape.Check` infers, then requires a subset of the slot's schema (untyped into typed
is refused). Runtime: eval, then `conform` validates and fills defaults.

## Escaping — `$`-doubling, not backslash

`\$` is an invalid escape in JSON and double-quoted YAML, so a backslash escape breaks in exactly
the quoting styles people use. `$` is special in neither host: `$$` → `$`, `$${` → literal `${`,
leaf-leading `$$:` → literal `$:`; a `$` forming neither marker is already literal (`$5.00`).
Each region is unescaped by exactly one layer: the template layer unescapes markers, and inside
`${…}`/`$:` the expression lexer does its own string escapes. `$:` tolerates leading whitespace,
so block scalars work. A `${` ends at the first `}` whose body parses
([map-expressions.md](map-expressions.md) §Templates).

## Where it applies

- Free projection, no schema (task/process `output`, fetch `body`, external `input`):
  grammar applies, `Shape.Check` skipped — unless a declared slot schema is written
  ([declared-slot-schemas.md](declared-slot-schemas.md)).
- Target schema exists (child `input` ⊆ input_schema; `headers` against
  `object<string>`): checked.
- String positions (`url`, `method`): templates, checked non-null against `string`.
- Expression-only (`case`, `over`): bare expressions. Delay `for`/`until` take a literal,
  a bare number or a `$:` leaf ([delay-syntax.md](delay-syntax.md)).
- **Never expressions, by design:** `id`, `type`, child `name`/`version`,
  `result_schema`/`responses`, raise/panic `code`, `on_error` codes —
  downstream analysis needs their concrete values.

Per-action payload schemas, which needed this grammar, are
[declared-slot-schemas.md](declared-slot-schemas.md).

## Editor schema

The generated JSON Schema (`GET /public/process-schema.json`) makes every node `node | string`
(`Schema.Relaxed`, the expression escape hatch), descending through `mapChildren` — the one
definition of where sub-schemas live, so a new keyword is picked up in one place.
`TestProcessSchemaShape` guards it.

- **`anyOf`, not `oneOf`** — the string branch overlaps string leaves and number/integer overlap
  each other; `oneOf` rejects both.
- **Array items are `{}`**, not `$ref ModelShape`: openapi-typescript emits an indexed
  self-reference as an eager cycle tsc rejects (TS2502). Object recursion is kept.
- `description` is a schema keyword, preserved, and **stripped by canonicalization** so it never
  affects type identity or `IsSubset`.

## Open

- Author-time rejection of unknown object keys (runtime conform strips; `Shape.Check` should
  reject so editors flag typos). Trigger: a typo'd key silently stripped in a real definition.
- Heterogeneous arrays — only homogeneous ones, matching the engine's single `items`. Trigger: a
  tuple-shaped slot that cannot be typed.
- Object spread (`...$:`): order-dependent override needs an ordered representation, both `...`
  and `$:` need quoting, and the expression language has none. Trigger: an override pattern
  `??` per key cannot express.

# Typed values: `$:` expressions, `${}` interpolation, and structured literals

Status: **DRAFT / design, updated 2026-07-22.** Supersedes the `{{ }}`-based grammar.
This is a **prototype with no backward-compatibility constraint** — the old
type-preserving `{{ }}` form is removed outright, not deprecated. Engine facts are
cited to `file:line`. Open details are marked *(open)*. Related:
[the `unknown` type](unknown-type.md) — orthogonal and **deferred**; this document is
the near-term work.

## The idea in one line

At **any node** of a value the author can write the structure **literally in YAML**
(objects, arrays, scalars — with expressions at the leaves) **or** hand over a
**single typed expression** (`$:`) that resolves to the whole subtree. Either way the
result is checked, by its *inferred type*, against the slot's schema where one exists.

This slides the static/dynamic boundary anywhere: keep the parts you know as YAML,
express the parts you don't. It generalizes what `input`/`headers`/`output` already do
to every place that takes structured, expression-bearing input.

## Two authoring modes, one sigil

The old grammar overloaded `{{ }}`: a **lone** `"{{ expr }}"` preserved the
expression's type, but the *same* braces inside surrounding text stringified. Whether
a value kept its type therefore depended on invisible surrounding whitespace — the
single defect this redesign removes. The two intents are now two syntaxes, and `$` is
the one expression sigil for both:

| you write | meaning | result type |
|---|---|---|
| `"$: EXPR"` | typed expression, **whole leaf**; a quoted string (leading whitespace tolerated) | the inferred type of `EXPR` |
| `…${ EXPR }…` | interpolate `EXPR` into surrounding text, **anywhere** | always `string` |
| `plain text` (no marker) | literal string | `string` |
| `42` / `true` / `null` / `[…]` / `{…}` | structured literal (YAML/JSON) | the literal's type |

- **`$:` computes a value; `${}` builds a string.** A `${}` template always yields a
  `string` — no result-type inference, no "did surrounding text change my type?"
  ambiguity. Interpolating a structure (`${ input.tags }` where `tags` is an array) is
  a uniform error: you can only splice string/number/bool into text.
- To get the array itself you write `$: input.tags`. To build a header string you
  write `Bearer ${ input.token }`. The two never fight over type.
- `$:` marks the **entire** leaf — everything after `$:` (trimmed) is one expression.
  There is no "mixed" typed leaf; to concatenate inside a typed expression, use the
  expression language's `+`, not `${}`.

### Authoring in YAML: a `$:` expression is a quoted string

A typed expression is just a **string** whose content starts with `$:`, so in YAML it
**must be quoted**:

```yaml
count:   "$: input.n + 1"
body:    "$: map(input.rows, r => {sku: r.code})"
headers: { Authorization: "$: input.token" }
```

The reason to spell this out (it *is* a footgun otherwise): `$:` mirrors YAML's own
`key:` syntax, so an **unquoted** `$:` leaf does not survive parsing —
`body: $: input.x` errors (`mapping values are not allowed`), and on its own indented
line it **silently** parses to the object `{"$": "input.x"}` rather than a string. Quote
it and both problems vanish.

This is not special to `$:` — **any expression containing `: `** (an object literal
`{a: b}`, a ternary `c ? x : y`) must be quoted or written as a block scalar, because a
colon is a YAML mapping indicator. Interpolation (`${ }`) carries no colon, so plain
templates need no quoting (`url: ${config.api}/users`), and an expression-only position
needs quoting only when its expression contains a colon (`over: input.items` is fine
bare; `over: "map(xs, x => {a: x.n})"` must be quoted).

**Rule of thumb: quote every `$:` leaf, and quote any expression with a `:` inside.**

### Expression-only positions never take a marker

The markers exist to disambiguate **expression vs. literal text** — a choice that only
arises where literal text is meaningful, i.e. where the required type is a **string or
free-form data**. A position whose required type is a **fixed non-string** (boolean,
number, array) can never usefully hold literal text or a string template, so there is
nothing to disambiguate: the field is **always one expression**, written bare, with **no
`$:` and no `${}`**.

| position | required type | form |
|---|---|---|
| switch `case` | boolean | `case: self.paid == true` |
| `over` (child_list) | array | `over: input.items` / `over: map(input.rows, r => …)` |
| `ms` (delay) | number | `ms: outputs.x.retry_after` — or a bare literal `ms: 30000` |

Contrast the **string** positions `url` and `method`: literal text *is* the common case
(`method: POST`, `url: https://api/users`), so they stay template positions and use
`${ }` — parsing `POST` as the expression `POST` (an undefined identifier) would be
exactly wrong. That is the dividing line, and it is precisely the rule: **templates make
sense ⇒ template position; templates don't ⇒ expression-only, no marker.**

In an expression-only position a string is parsed directly as an expression; a native
YAML scalar (`30000`, `true`) is taken as that literal. Migration strips the old wrapper
— `"{{ input.items }}"` → `input.items`.

## The value grammar

```
Value = string                 // template: literal text and/or ${ } interpolation → string
      | "$:" expr              // typed expression, whole leaf → inferred type   (NEW path)
      | number | bool | null   // literal scalar                                  (NEW)
      | [ Value, … ]           // literal array                                   (NEW)
      | { key: Value, … }      // literal object                                  (exists)
```

The literal-vs-expression decision is **structural, at authoring time**: a string is a
template (or a `$:` expression); a map/array/scalar is a literal built recursively.
A whole subtree can always be replaced by a single `$:` expression that yields it —
that is the "either YAML or expression" duality, and the reason `$:` at a slot **root**
gives "the whole payload as one expression" for free.

What changes from today (`Shape = string | Record<string, Shape>`,
`internal/model/wire.go:143-151`):

- **Scalars.** `retry: 3` / `enabled: true` are literals today you'd have to spell
  `"$: 3"`. genctl already preserves numeric/bool literals through YAML→JSON
  (`cmd/genctl/input.go:80-141`), so the wire plumbing exists; `checkShape`
  (`wire.go:215-229`) must stop rejecting non-string scalars.
- **Arrays.** New structurally. A literal `[a, b, c]` infers to `array<join of element
  types>`, checked element-wise. The join/kind logic already exists for array literals
  *inside expressions* (`internal/schema/infer.go`, see
  [map-expressions.md §2](map-expressions.md)) — `inferShape` reuses it.
- **`null` vs absent** *(open, must decide before build)*. `Shape.Present()` keys on
  `Raw != nil` (`wire.go:171`), so an authored `null` leaf is indistinguishable from an
  omitted field. `null` inside an array/object is meaningful (`[1, null, 3]`); `null`
  at a slot root is not. Proposal: allow `null` only inside a structure; keep a root
  `null` meaning "absent". *(open)*

## Expression syntax and escaping

### The two markers, precisely

- **`${ … }`** — an interpolation block, valid **anywhere in a leaf string**. Its body
  is a genroc expression; the result is stringified and spliced into the surrounding
  text. Block boundaries are decided by the parser exactly as `{{ }}` are today: at each
  `${`, candidate `}` terminators are tried in order and the first body that **parses**
  wins, so a `}` inside a nested object literal or a string literal does not close the
  block (`parseBlock`, `internal/template/template.go:89-111`). This retargets `{{`→`${`
  and `}}`→`}`; the shortest-match-that-parses rule is unchanged.
- **`$:`** — a typed-expression leaf, valid **only at the leaf's first non-whitespace
  position**. Everything after `$:` (trimmed both sides) is one expression, evaluated
  with its type preserved. Not a `Template` at all — it bypasses the template layer and
  goes straight to `expression.Parse`/`Eval`/`Infer`.

A `$` in any other position is a literal dollar: `$5.00`, `$HOME`, `price: $9` need no
escaping. Only `${` (anywhere) and a leaf-leading `$:` are live.

### YAML multiline

A `$:` (or `${}`) body may span lines via a YAML block scalar; it collapses to one
JSON string on the wire (multiline is an authoring nicety, not a wire concept):

```yaml
body: |
  $: map(input.rows, r => {
       sku: r.code,
       qty: r.count + 1,
     })
```

The leading-whitespace tolerance on `$:` is what lets a block scalar's residual
indentation not defeat marker detection.

### Escaping with `\`

`\` escapes a following `\` or a **marker-forming** `$` — and *only* those. A `\`
before anything else (including a `$` that is **not** forming `${`/leaf-`$:`) is a
literal backslash. That single rule produces the collapse behaviour:

| source | renders as | why |
|---|---|---|
| `\${x}` | literal `${x}` | `\` before marker `$` → escapes it; block is literal text |
| `\$:` at leaf start | literal `$:` | `\` before marker `$` → leaf is a plain string |
| `\\` | `\` | backslash escapes backslash |
| `\$` (bare `$`, no `{`/leaf-`:`) | `\$` | `$` isn't a marker, so `\` stays literal |
| `\\$` | `\$` | `\\`→`\`, then bare `$` literal |
| `\\\$` | `\\$` | `\\`→`\`, then `\`+bare-`$` → both literal |
| `\\${x}` | `\` + interpolation of `x` | `\\`→`\`, then live `${x}` |
| `\\\${x}` | literal `\${x}` | `\\`→`\`, then `\${`→ literal `${` |

Scanning is left-to-right, greedy: on `\`, if the next char is `\` emit one `\` and
advance two; else if the next char is a `$` that begins a live marker at that position,
emit a literal `$` (marker neutralized) and consume the `\`; otherwise emit a literal
`\`. Backslash runs therefore collapse in pairs (`N` backslashes before a bare `$` →
`⌈N/2⌉`), matching the table.

**The two-layer trap.** Escaping is a **template-layer** concern — it applies to
literal text and marker positions only. Inside a `${ … }` or `$: …` body the raw source
is handed to the **expression lexer**, which does its *own* backslash handling for
string literals (`'a\tb'`). Each region is unescaped by exactly one layer; never both,
or `\t` inside an expression string would be mangled. The boundary is the marker.

## Inference (types)

- **template string** → `string`, always. Its interpolated sub-expressions are still
  inferred (to prove they are non-null and stringifiable, for secret taint, and for
  root-ref lazy-loading), but the *result* type is fixed. This deletes `Template.single`
  (`template.go:44`) and both `if t.single` branches (`EvalAny` `:119`, `InferType`
  `:147`): the type-preserving path is gone, so `InferType` always returns `string`
  after checking each `${}` body is a non-null scalar (the existing `stringify` guard,
  `template.go:249-272`, now applies to every template uniformly).
- **`$:` expression** → inferred directly by `expression.Infer` against the context.
- **array literal** → `array<join of elements>`; `[]` types as
  `{"type":"array","maxItems":0}` (provably empty), not an itemless array, so the
  `?? []` idiom keeps working — mirror the expression side, which learned this the hard
  way ([map-expressions.md §5](map-expressions.md)).
- **object literal** → closed object, every key required (matches `inferShape` today).
- **scalar literal** → its kind (`json.Number` distinguishes integer from number).

`IsSubset` needs an **array arm** (element-wise via `items`) — the one genuinely new
inference primitive. Object/subset/scalar arms already exist.

## Two operations (unchanged split)

- **`Fits(value, target)` — author time.** `inferShape(value)` then `IsSubset(target)`.
  Type-level only; does not evaluate or fill defaults. Says "this structure/expression
  can produce something of the required shape." `IsSubset` already refuses an
  untyped/`any` source into a typed target
  (`internal/validation/validate_children.go:206-207`) — the strictness we want.
- **`eval + conform` — runtime.** Evaluate against context, then `conform` against the
  schema — validating concrete values and filling `default:`s
  (`internal/schema/validate.go:105`).

This is the engine's existing static/runtime split; the widened grammar rides it.

## Runtime evaluation

A `Value` leaf evaluates by one of two paths, fixed at parse time:

- **`$:` leaf** → `expression.Eval(body)`, result used as-is (any type).
- **anything else** → the template path, which now **always stringifies** (no `single`
  short-circuit). A structural array/object/scalar node evaluates its children
  recursively.

## Where it applies

The grammar is universal; the *check* depends on whether the slot has a target schema.
(Full slot-by-slot map is in the conversation that produced this doc; summarized here.)

- **Free projection, no schema** — process/task `output`, fetch `body`, external
  `input`. Grammar applies; `Fits` is skipped (nothing to check against). Cheapest tier.
- **Has a target schema** — child `input` (checked `⊆ input_schema` today,
  `validate_children.go:189`) and fetch `headers` (checked only for object-ness today,
  `internal/validation/infer.go:168`; a real `object<string>` `Fits` would tighten it).
- **String positions** — `url`/`method` are template strings (`${ }`), checked non-null
  via `Fits` against `string` (replacing `checkNonNullTemplate`, `infer.go:110-128`).
- **Expression-only positions** — switch `case` (boolean), `over` (array), `ms` (number):
  bare single expressions, no marker, per the rule above. `over`/`ms` shed their `{{ }}`
  wrapper; `ms` additionally accepts a bare-number literal.

**Deferred to later work** (explicitly out of scope for the Shape update): per-action
**payload schemas** + collapsing `validateActionRequiredFields`
(`internal/model/validate.go:113-153`) into one `Fits`; the **fetch-action pull-out**
(one `Value` payload for `{url, method, headers, body}`); and the
[`unknown`](unknown-type.md) result type. The grammar here is the prerequisite for all
three.

**Never expressions, by design:** `id`, `type`, the child family's `name`/`version`,
`result_schema`, `accepted_status`, fault codes/messages, `on_error` codes. Downstream
analysis needs their concrete values, so they opt out of the `Value` grammar.

## Editor support: keep the generated JSON Schema

The definition is authored as `.genroc.yaml`, and a generated **standard JSON Schema**
drives editor hints via yaml-language-server (wired with a
`# yaml-language-server: $schema=…/process-schema.json` modeline or a `yaml.schemas`
glob). That schema already exists and must be **maintained through this change** — it is
`buildProcessDefinitionSchema()` (`internal/api/openapi.go:28-66`), which reflects
`model.ProcessDefinition` into draft 2019-09 and is served at `GET /process-schema.json`
(`internal/api/server.go:54`). Within it, every `Shape` field resolves to the
`ModelShape` def produced by `Shape.JSONSchemaBytes()` (`wire.go:200-211`, a swaggest
`RawExposer`), and the whole action comes from `Action.JSONSchemaBytes()`
(`definition.go:83-176`). Those two hand-written literals are the levers.

### The principle: the required schema, with a string branch at every node

A `Value` slot should be **typed as the schema it requires, but every node may also be a
string** — the string is the `$:`/`${}` expression escape hatch. That is one transform,
`relax(S)`:

```
relax(S) = anyOf: [ S-with-its-children-relaxed , { "type": "string" } ]
```

Applied recursively, an `object<string>` becomes "an object whose values are strings *or*
expression strings, or the whole thing an expression string", so the editor still
completes property names while accepting an expression anywhere.

**`anyOf`, not `oneOf`.** The added string branch overlaps a structure's own string
leaves, and `number`/`integer` overlap each other — under `oneOf` (matches *exactly*
one) a plain string leaf or an integer would match two branches and spuriously fail.
This is the same exclusivity trap the empty-array `oneOf` hit on the engine side
([map-expressions.md §5](map-expressions.md)); use `anyOf` throughout the tooling schema.
(The tooling schema is standard JSON Schema — the engine's own lack of
`additionalProperties:false` does not matter here.)

### Three tiers of hinting

- **Generic `Value` (no static target)** — the widened `ModelShape`:
  `anyOf: [ {type:string}, {type:number}, {type:boolean}, {type:null},
  {type:array, items:{}}, {type:object, additionalProperties:$ref ModelShape} ]`.
  This replaces today's `oneOf:[string, object]` in `Shape.JSONSchemaBytes()` and covers
  `body`, `output`, external `input`, and — for now — child `input`. Basic structure
  hints (nest objects/arrays/scalars) plus the string escape hatch everywhere.
  **Deviation from the original sketch:** array items are permissive (`{}`), NOT
  `$ref ModelShape`. openapi-typescript (which generates the test client from `openapi.json`)
  emits a `$ref` as an indexed access, and an *array of that indexed self-reference* is an
  eager cycle tsc rejects (TS2502); the object index-signature self-reference is lazy and
  fine. So arrays are accepted but not recursed into — the same permissive-for-reflection
  compromise `schema.Schema.JSONSchemaBytes()` already makes. (Object recursion, the common
  structured case — request bodies, inputs — is preserved.)
- **Fixed per-action structure** — `headers` requires `object<string>`, so inline
  `relax(object<string>)` directly in `Action.JSONSchemaBytes()` instead of the generic
  `$ref ModelShape`, giving a tighter hint. The future fetch **payload schema** slots in
  here the same way.
- **Context-dependent (deferred to an LSP)** — child `input`'s *actual* properties are
  chosen by the sibling `name:`, which a static JSON Schema cannot resolve. It stays on
  the generic `ModelShape` until a language server can resolve the referenced child.

### Description hygiene

Every `{{ }}` mention in the schema descriptions must move to the new syntax:
`url`/`method` → `${ }` (they yield strings), `over`/`ms` → `$:` (they yield
array/number; `ms` also gains a bare-number literal), and the `Shape`/`ChildEntry`
payload descriptions → "a literal value with `$:`/`${}` leaves, or a single `$:`
expression". These live in `Action.JSONSchemaBytes()`, `Shape.JSONSchemaBytes()`, and the
struct `description:` tags on `ChildEntry`/`ProcessDefinition` (`definition.go`).

Minor wrinkle: an `anyOf` string branch at every leaf makes yaml-language-server's
completion a little noisy (it can't always narrow which branch you're in). Livable, and
the price of "expressions allowed anywhere".

## Ledger

Done as **one combined change** (grammar + syntax + escaping together), phased so each
step compiles and tests green. **Implementation status (2026-07-22): Phases 1 & 2 landed
and green (Go + TS); Phases 3 & 4 deferred** — the breaking `{{ }}`→`${ }` retarget forces
a judgment-based migration of ~239 sites across 81 TS files, so it is a dedicated future
pass, not blended into the additive work.

**Phase 1 — grammar & inference (pure addition). ✅ DONE.**
- Widened `checkShape`/`Shape` to accept arrays + scalar literals + `null`
  (`wire.go`); `null` handled by pointer-nil = absent (no root-null representation needed),
  nested null accepted.
- Widened `inferShape` to array-join / scalar-kind; added `schema.ArrayLiteral` as the
  shared join helper (also refactored `inferArray` onto it); `IsSubset` array arm already
  existed, added the provably-empty (`maxItems:0`) case. All six Shape-walkers
  (`checkShape`, `Shape.Strings`, `inferShape`, `shapeRefsSelfResult`, `shapeRoots`,
  `evalShape`) descend into arrays. Numeric kind uses a whole-number→integer heuristic on
  `float64` (JSON's only numeric decode).

**Phase 2 — the `$:` typed leaf (additive). ✅ DONE.**
- `template.Parse` detects a leaf-leading `$:` (whitespace-tolerant, `strings.CutPrefix`)
  and parses the body as one expression, reusing the `single` (type-preserving) path so it
  flows through every walker unchanged. `\`-escaping of the marker is deferred to Phase 3.

**Phase 3 — retarget templates & escaping (the breaking step, last). ✅ DONE.** Retarget +
kill-single + full site migration + escaping all landed; the whole suite is green. Escaping
uses **`$`-doubling, not a backslash** — `\` would collide with JSON/YAML string escaping
(`\$` is an *invalid* escape in JSON and double-quoted YAML, so `\${` breaks there; it only
works in YAML plain/single-quoted). `$` is not an escape char in either host, so `$$` is
collision-free in every quoting style. In `scanTemplate`: `$$`→literal `$` (so `$${`→literal
`${`, leaf-leading `$$:`→literal `$:`), staying out of expression bodies (two-layer split);
covered by `template_escape_test.go`. The migration was
mechanical and behaviour-preserving — a lone `"{{ x }}"` (old type-preserving) → `"$: x"`,
a mixed one → `"${ x }"` — done with a parse-aware tool (real expression parser for
block-end detection). ~166 TS sites + Go fixtures + example/bench YAML migrated; the
template package + its tests rewritten; the R2 fault-literal guard retargeted `{{`→`${`.
The one hazard (JS `${}` in TS backtick strings) was a non-issue: the single backtick case
was lone → `$:`, which JS doesn't touch.
- `{{`/`}}` → `${`/`}` in `Parse`/`parseBlock` (`template.go:55-111`).
- Delete `Template.single` and both `if t.single` branches — templates always
  stringify (`EvalAny` `:119-145`, `InferType` `:147-176`).
- Add `\`-escaping in the splitter (marker-`$` and `\` only), keeping it out of
  expression-internal escapes.
- Migrate all example definitions (`examples/`) off `{{ }}` → `${}`/`$:`.

**Phase 4 — editor JSON Schema (ships with the grammar). ✅ DONE.**
- **`description` is now a first-class schema keyword** (`internal/schema`): authors can
  document `input_schema`/`result_schema`/`config_schema` fields (the strict allowlist used to
  reject it). It is a pure annotation — accepted at parse, preserved through `normalize` and
  serialization, exposed via `Schema.Description()` / `WithDescription`, but **stripped by
  `canonicalizeNode`** so it never affects type identity (the recursive-inference fixpoint keys
  off canonical JSON) or `IsSubset`. This is also what lets a relaxed target carry the user's
  own field descriptions alongside the generated expression hints.
- **All Shape editor schemas are generated from `internal/shape/editor.go`, not hand-written.**
  `GenericValueSchema()` produces the `ModelShape` def — relax of an unconstrained target: the
  generic `Value` `anyOf` (string/number/boolean/null/array/object), the single def every free
  Shape slot (`body`, `output`, `input`) resolves to via `$ref`. `Shape.JSONSchemaBytes()`
  (`internal/shape/shape.go`) just delegates to it (the generated bytes are byte-identical to
  the former hand-written literal). **anyOf, not oneOf** (overlap trap). Array items are
  permissive `{}` rather than `$ref ModelShape` — an array-of-indexed-self-reference breaks the
  openapi-typescript client codegen (TS2502); object recursion (the common case) is kept. See
  the method comment and the "Three tiers" deviation note above.
- Implemented the `relax(S)` transform as a **universal schema-node walk over a shared
  descent combinator**, not a keyword-guessing JSON pass. `internal/schema/relax.go` adds
  `mapChildren(n, fn)` — the single definition of "where sub-schemas live" (properties, items,
  additionalProperties, oneOf/anyOf/allOf, $defs), the transform analogue of normalize's
  `walkTree` visitor — so a new sub-schema keyword is picked up in one place. `Schema.Relaxed(
  stringNote)` recurses through it and makes every node `node | string` unless it already
  admits a string, correct for any target (nested objects, arrays, unions, enums, $refs) with
  no per-keyword special-casing. A pure string leaf stays a string (literal == expression); an
  enum is still wrapped so an expression stays allowed. Because `description` is now a
  first-class node field, `Relaxed` labels every string position with `stringNote` **node-side**
  — no JSON post-pass — including nested header **values** (the reported gap), and it never
  clobbers an author's own description. `shape.RelaxedSchema(target)` is now a one-liner:
  `json.Marshal(target.Relaxed(exprLeafDesc))`. `headers` is generated from its object<string>
  target with a property-level description merged on; a typed leaf like `count:integer` becomes
  `integer-or-expression`. Tested in `schematest/relax_test.go` (universality incl.
  object→array→object nesting, unions, enums, the note) and `shape/editor_test.go`.
- `{{ }}` → `$:`/`${}` description sweep landed earlier with Phase 3.
- No new endpoint — `GET /process-schema.json` serves it (`openapi.go`, `server.go`); the
  `TestProcessSchemaShape` guard (`internal/api/shape_schema_test.go`) now asserts the 6-variant
  `anyOf`. `make test` regenerates `openapi.json`→`generated/api.ts` and typechecks it, so the
  client stays in sync automatically.

**Reused as-is:** `expression.Eval`/`Infer`, the parse-attempt block splitter,
`conform` + `default:` filling, the reflected `ProcessDefinition` schema pipeline
(`buildProcessDefinitionSchema`), `stringify` (now universal).

## Test surface ("solid and well tested")

Extend the existing suites (160 Go tests + oracle on the expression side) symmetrically
for the value layer:

- **grammar accept/reject** — arrays, scalars, `null`, nested; `$:` at/after
  whitespace; the full escape table above incl. backslash-run collapse; `${}` block
  boundaries with nested `}`/string literals; `$:`-vs-`${}` type divergence
  (`$: input.n` → integer, `${ input.n }` → string).
- **inference** — `inferShape` for every new form; array join incl. `[]` maxItems:0 and
  `?? []`; `IsSubset` array arm.
- **runtime** — eval+conform per form; the two-layer escape boundary (`'a\tb'` inside
  `$:` survives); stringify guard fires on `${ array }`.
- **round-trip** — YAML(block scalar)→JSON→Shape→eval.

Homes: `internal/template/template_test.go`, `internal/validation/validationtest`,
`internal/schema` inference tests.

## Open questions (settle later)

- **`null` vs absent** at a slot root (proposal: `null` only inside structures). *(open)*
- **Author-time strictness on unknown object keys.** Runtime `conform` silently strips
  undeclared keys; author-time `Fits` should probably **reject** them so the editor
  flags typos. *(open)*
- **Homogeneous arrays only** (`array<join>`), no tuple/positional types — matches the
  engine's single `items`. Proposal: homogeneous only. *(open)*
- **Future — object spread `...$:`.** Merge an expression-produced object into a
  structural literal, explicit keys overriding — prefill a payload (or the whole fetch
  `with:`) and override selectively: `{ "...$": "input.request", headers: {…} }`.
  Deferred, but two traps for whoever picks it up: (1) override is **order-dependent**,
  and a JSON object / Go `map[string]any` does not preserve key order — it would need an
  ordered representation like `SwitchMap` (`internal/model/wire.go:54`); and (2) both
  `...` (YAML's document-end token) and `$:` need quoting, so the surface spelling wants
  care. Also net-new in the expression language, which has object literals but no spread.
  *(future)*
- **Sigil spelling confirmed:** `$:` (typed) and `${}` (interpolation), `\` escape.
  `$()` was considered and rejected (reads as a call; `${}` matches the universal
  "expand a value here"). No trailing-space requirement — `$:` is a plain string prefix.
  **Settled.**
- **The block-form YAML misparse is handled by docs, not a mechanism.** Writing a `$:`
  expression on its own indented line (`body:\n  $: x`) parses to `{"$":"x"}` silently;
  the fix is the "quote every `$:` leaf" rule above. Deliberately *not* enforced in code.
  If it ever proves error-prone, reserving `$` as an object key (a registration error
  with a "did you mean `\"$: …\"`?" hint) would make that one case loud at zero ergonomic
  cost — the escape hatch is recorded, not taken. **Settled (docs-only).**

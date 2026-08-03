# map, lambdas and JSON literals: design

Status: implemented. Grammar in `internal/expression/syntax`, evaluation in
`internal/expression`, typing in `internal/schema/infer.go`, template splitting
in `internal/template`. Implements `ROADMAP.md` → "map function".

> **Syntax note.** Examples below predate the `{{ }}` → `${ }` retarget ([typed-values.md](typed-values.md)): read `{{ x }}` as `${ x }` inside a string, or `"$: x"` where the whole leaf is one expression.

Adds four constructs to the expression language:

| construct | example | result type |
|---|---|---|
| object literal | `{name: input.user.name, n: 1}` | closed object, all keys required |
| array literal | `[input.a, input.b]` | array of the joined element type |
| lambda | `item => item.id` | only as a `map` argument |
| `map` | `map(input.items, x => {id: x.id})` | array of the body type |

The point is reshaping: turn an array of one shape into an array of another, so
a `fetch` body, a `child_list` `over`, or a process output can be built from a
collection without a per-element task.

```
{{ map(input.rows, r => {sku: r.code, qty: r.count + 1}) }}
{{ map(input.items, (x, i) => {pos: i, id: x.id}) }}
```

## 1. Why the parser is ours

The language was previously a subset of expr-lang: expr-lang parsed, and we
walked its AST. That constraint broke twice over this feature, both times
unfixably from outside the parser.

**`{` cannot start a map body.** expr-lang's `parsePredicate`
(`parser/parser.go:668`) reads a leading `{` in predicate position as its
*statement block* form, so `map(xs, {id: .id})` dies on the `:` before we see an
AST. Only `map(xs, ({id: .id}))` parses. genroc has no statements, so that
parenthesis is pure noise here.

**`#` cannot reach an outer element.** expr-lang binds `#` to the innermost
predicate, so `map(a, map(b, #.m))` evaluates to `[[10],[10]]` — the inner
predicate rebinds it and the outer element becomes unreachable. Nested reshaping
is therefore inexpressible with pointer syntax, whatever the spelling.

So the grammar is ours, and named lambda parameters replace `#` entirely. `#`
and the `.field` shorthand are rejected with a message naming the replacement.

**The lexer is still expr-lang's.** `lexer.Lex` handles the four string forms
(`'…'`, `"…"`, backticks, `b'…'`), escapes, and numeric literals; reimplementing
those is exactly the kind of thing that drifts silently on an upgrade. We
consume its tokens and parse them ourselves (~450 lines, precedence climbing).
Byte literals are lexed but rejected — `[]byte` has no JSON counterpart.

Two expr-lang behaviours are replicated deliberately, because existing
definitions depend on them:

- **`??` has precedence 500**, binding tighter than arithmetic, so `a + b ?? c`
  is `a + (b ?? c)`.
- **Mixing `??` with another operator unparenthesized is an error**
  (`a ?? 0 + 1`, `a ?? b == c`). `prevOp` is local to each `parseBinary` frame,
  which is precisely what makes `a + b ?? c` legal while `a ?? b + c` is not.

Owning the parser buys error messages that quote what the author wrote:

```
unexpected ")"
  map(a, x => x.n + )
                    ^
```

### The conformance oracle survives

`expressiontest` ran real expr-lang against our `Eval` and our `Infer`. Diverging
syntax would normally forfeit that for the newest, least-tested construct. It
does not, because `x => body` is exactly expr-lang's `{let x = #; body}` — a
form verified to bind identically, including nested capture and shadowing. So
`map_test.go` pairs each expression with its expr-lang translation and keeps the
three-way check. The `let` rewrite is a *test* device; it never runs in
production, where it would leak into error messages.

## 2. Typing

### Object and array literals

Object keys must be names or quoted strings; duplicates are rejected at parse
time. The result is a closed object with **every key required**, mirroring
`validation.inferShape`. Keys are emitted sorted so generated schemas are
deterministic (`node.Required` is a slice, unlike `Properties`).

A non-empty array literal joins its element types, so `[1, 2]` is
`array<integer>` and `[1, "a"]` is `array<oneOf[integer,string]>`.

`[]` types as `{"type":"array","maxItems":0}` — not an itemless array. Recording
that it *provably holds no element* is what makes the `?? []` idiom work: see
below.

### `map(src, lambda)`

1. Reject `src.HasNull()` — a null source panics at runtime in expr-lang, so it
   must be a registration error: *"map source may be null; use ?? to provide a
   default array"*.
2. Reject a non-array `src`.
3. Element type comes from **`Items()`, not `Index()`**. `Index` is nullable
   because a constant index may be out of bounds; `map` only ever visits real
   elements. Getting this backwards would make every mapped field spuriously
   nullable and force `??` everywhere.
4. A source with no `items` is an error — binding an unconstrained element would
   turn a typo in the body into a runtime null instead of a registration error.
5. The body is inferred with the parameters bound (`elem`, and `integer` for the
   index parameter); the result is `Array(body)`.

**Union sources.** `elementOf` handles a source that is a union — what `??` or a
ternary produces — by joining the variants' element types and **skipping
provably-empty variants**. That is what makes `map(input.opt ?? [], x => x.name)`
keep `string` rather than degrading to an unconstrained array: `input.opt ?? []`
is `oneOf[array<string>, array(maxItems 0)]`, and the empty variant can never
supply an element. Skipping it is sound, not a special case for `??`.

### Scoping

A lambda parameter **shadows a context root**, so `map(xs, input => input.n)`
reads the element, not the process input. This holds identically in three
places, and all three are tested:

- `inferCtx.vars` — inference
- `env.vars` — evaluation
- `bound` in `collectRoots` — reference analysis

`withParams` also **drops guards rooted at a shadowed name**: a narrowing
established outside the lambda says nothing about the parameter that now owns
that name.

### Where this sits in the two-mode split

Per `specs/recursive-type-inference.md` §2, **`map`'s source position is
look-inside** — reading `items` requires resolution. The body is neither mode: it
is inferred in a child scope and may stay symbolic, and a `$ref` surviving into
`Array(...)` sits under `items`, which the productivity rule already counts as
productive. Object and array literals are structural constructors — they neither
resolve operands nor expand refs, so they are pass-stable in either mode.

**Recursive accumulation through `map` is not supported.** Inside a solver
cluster the estimate is null-seeded, so `map(self.previous.list, …)` fails rule 1
and the `?? []` escape yields an empty source. Without array concatenation, `map`
over one's own prior output cannot express an accumulator anyway.

## 3. Two places this could have leaked

**Secret taint.** `walkSecretRefs` resolves a node's dot-path against the root
schema. That cannot see a secret living on the *element* type: `input.items` is
not secret, `input.items[].token` is, and `x.token` has no path from the root. So
the walk threads `inferCtx` and resolves a path rooted at a lambda parameter
against that parameter's schema instead. If inferring the source fails, it taints
— the expression will not type-check anyway, and over-tainting only costs log
verbosity while under-tainting is a leak.

**Root refs.** `collectRoots` drives slot-level lazy loading: `buildEnv`
materializes an externalized `*model.ObjectRef` only if `Roots` says an
expression reads it, and substitutes `nil` otherwise. Had the walkers not
descended into call arguments and lambda bodies,
`map(outputs.fetch.items, x => x.id)` on an externalized output would produce
`nil` at runtime — a wrong answer, not an error, and only for values big enough
to have been externalized.

Fixed in passing on the same path: `shapeRoots` unioned `SelfPrevious` but
dropped `SelfResult`, so an externalized `self.result` read from a shape came
back `nil`.

## 4. Templates: splitting is parsing

`{{ }}` blocks used to be found by scanning for the next `}}`. A nested object
literal breaks that — `{{ map(a, x => {z: {deep: x.n}}) }}` contains `}}` inside
the expression.

Block boundaries are now decided by the parser: at each `{{`, candidate
terminators are tried in order and the first body that **parses** wins.

Brace counting was tried and rejected. It gets six of eight cases wrong: a `}}`
inside any string form ends the block early, and an *unbalanced* brace inside a
string (`{{ "a{b" }}`, which parses today) desynchronizes the counter so it never
terminates at all. Making it sound means tracking all four string forms —
lexer state reimplemented where it can drift. It also saves nothing: the common
case has exactly one `}}`, so the parse-attempt scan performs exactly one parse,
which the `Template` value needs anyway.

Shortest-match is sound: for a longer body to have been intended it must also
parse, which requires the intervening `}}` to sit inside brackets or a string —
and in both cases the shorter candidate fails to parse. When no candidate parses,
the error comes from the **longest** candidate; a truncated one dies with an
uninformative message while the full body carries the real syntax error.

Splitting being parsing made the old shape wasteful — `evalShapeCtx` cost two
`parser.Parse` calls per string leaf per tick, one via `RootRefs` and one via
`EvalAny`. `template.Template` now holds parsed nodes, "single expression" is a
parse-time property rather than a per-call re-derivation, and `template.Get`
memoises by source string. That boundary is right because `GetDefinition` caches
the definition JSON but deliberately re-unmarshals per call, so a parsed field on
`model.Shape` would be rebuilt every advance.

### A pre-existing hole the literals exposed

`stringify` accepts only string/number/bool, so an array or object interpolated
into surrounding text is a guaranteed runtime failure — but `InferType` checked
only nullability, so `x={{ input.tags }}` type-checked as `string` at
registration and then died mid-process. That predates this work (any array-typed
field hit it), but object and array literals make it trivially reachable.

`InferType` now also rejects a mixed interpolation that provably resolves to an
array or object. The check uses `IsType`, which means "resolves *uniformly* to",
so it fires only when the value certainly cannot be interpolated and never on a
type that merely cannot be pinned down — no previously valid definition becomes
invalid. As a whole-template expression (`{{ input.tags }}`) these values remain
fine, since there the type is preserved rather than stringified.

## 5. Bugs the edge-case sweep found

Six real defects, all fixed. Four predated this feature; literals and `map` just
made them reachable.

| bug | consequence |
|---|---|
| `==`/`!=` fell through to Go's `==` | **panicked the whole process** on two arrays or two objects (Go's `==` crashes on identical uncomparable dynamic types). Reachable from a registered definition, since `==` typed as boolean whatever the operands. Now rejected by `inferEquality` at registration and by `equalValues` at runtime — only when *both* sides are structured, so `someArray == null` still works. |
| `[]` unioned as an exclusive `oneOf` | an empty array satisfies both `array<T>` and `array(maxItems 0)`, and `oneOf` means *exactly* one — so `{{ xs ?? [] }}` produced a schema that **rejected its own empty result**, and `over: "{{ xs ?? [] }}"` failed registration because `Items()` is unreachable through a union. `absorbEmptyArray` now discards the provably-empty arm on every path that could build that union (`??`, ternary, array-literal join). |
| `secretAtSub` did not deref for a whole-element copy | `map(creds, c => c.pass)` tainted but `map(creds, c => c)` did not, when `secret` sat on the referenced *definition* — the copy exposes strictly more, so this was the wrong way round and leaked to logs. |
| `checkNonNullTemplate` checked only nullability | an array or object `url`/`method` registered fine and the engine then issued a request against `[a b c]` via `fmt.Sprintf("%v", …)`. The mixed-template path already rejected this; the single-expression path bypassed it. |
| `parseInt` used base 0 | C's leading-zero-octal rule made `017` evaluate to **15**, silently, where expr-lang gives 17 — breaking this package's promise that number rules match expr-lang. `08` was also rejected as "out of range". |
| a lambda parsed in any position | `[x => 1]`, `1 + (x => 1)` and `map(x => x, y => y)` all parsed, so the "unreachable" comments in `eval.go` and `infer.go` were false and the author got a bare error with no source quote or caret. The parser now grants lambda permission only in a builtin's callback slot, consumed by `parsePrimary` so it cannot leak into nested expressions (parentheses stay transparent). |

Output-map errors also never named their task, unlike every other expression
position — fixed by threading the task-scoped label into `inferOutputs`.

Two behaviours were examined and deliberately left as they are, recorded in
tests: a non-boolean or null ternary condition silently takes the else branch
(`mustBool` discards the type-assert failure, unlike `!`/`&&`/`||`), and integer
arithmetic above 2^53 loses precision because it routes through `float64`.

## 6. Test coverage

160 Go tests across six files, plus the e2e suite.

- `syntax/parser_test.go` + `parser_edge_test.go` (30) — grouping and precedence
  tables, the full `??` accept/reject matrix, number-literal *kind* and radix,
  all four string forms, lambda nesting/shadowing/placement, ~50 rejected
  spellings, exact caret offsets, and a prefix sweep asserting no input panics.
- `expressiontest/eval_edge_test.go` (32) — ~200 cases, 44 cross-checked against
  real expr-lang through the `let` translation: `??` against falsy-but-non-null
  values, short-circuit proofs, null-propagation chains, arithmetic typing, and
  map runtime semantics including cross-element binding leakage.
- `expressiontest/map_test.go` (9) + `infer_lambda_test.go` (29) — the oracle,
  `$ref` element types, arrays of arrays, 3-deep capture, shadowing of roots and
  outer params, guard-dropping in `withParams`, narrowing inside bodies, union
  sources, engine-shaped contexts, verbatim error messages, determinism.
- `expressiontest/infer_literal_test.go` (39) — object/array literal typing,
  join semantics, empty-array interactions, `??` interactions, operator
  rejections, and 11 secret-taint vectors.
- `validationtest/map_test.go` (20) — map through `validation.Generate` in
  `over`, body, url, method, headers, task and process outputs, `child_map`
  inputs and `switch` cases, plus task-ID error attribution.
- `template/template_test.go` (14) — split table covering nested literals, `}}`
  inside each string form, the unbalanced-brace case, the longest-candidate rule,
  the stringify guard, and lambda-aware root refs.
- `tests/integration/map_expression_test.ts` — a `child_list` fanning out over a
  reshaped array end to end, plus registration-time rejection of a bad field and
  of a nullable source.

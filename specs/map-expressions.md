# map, lambdas and JSON literals: design

Status: **Built.** Grammar in `internal/expression/syntax`, evaluation in `internal/expression`,
typing in `internal/schema/infer.go`, template splitting in `internal/template`.

Four constructs: object literals (closed, all keys required), array literals (joined element
type), lambdas (only as a `map` argument), and `map` — reshaping a collection without a
per-element task.

## Why the parser is ours

The language was an expr-lang subset, which broke twice, unfixably from outside: `{` cannot
start a predicate body there (the statement-block form eats it), and `#` binds to the innermost
predicate, so nested reshaping is inexpressible. Named lambda parameters replace `#`; `#` and
`.field` are rejected naming the replacement.

**The lexer is still expr-lang's** — string forms, escapes and numeric literals are exactly what
would drift silently if reimplemented. Two behaviours are replicated because stored definitions
depend on them: `??` at precedence 500 (`a + b ?? c` is `a + (b ?? c)`), and mixing `??` with
another operator unparenthesized is an error — `prevOp` being local to each `parseBinary` frame
is what makes `a + b ?? c` legal while `a ?? b + c` is not.

The three-way conformance oracle survives the divergence: `x => body` is exactly expr-lang's
`{let x = #; body}`, so tests translate. The `let` rewrite is a test device only.

## Typing

- Object keys are names or quoted strings, duplicates rejected at parse, emitted sorted so
  generated schemas are deterministic.
- `[]` types as provably empty (`maxItems: 0`); that is what makes `?? []` work. Every
  union-building path runs `absorbEmptyArray`: `[]` matches any array arm and `oneOf` demands
  exactly one, so keeping both arms rejects the empty value the union describes.
- `map(src, λ)` refuses a nullable source (a runtime panic otherwise), a non-array, and an
  itemless array (an unconstrained element turns body typos into runtime nulls). The element type
  is **`Items()`, not `Index()`** — `Index` is nullable for out-of-bounds, and `map` visits only
  real elements. A union source joins its arms' elements, skipping provably-empty arms, which
  keeps `map(xs ?? [], x => x.name)` typed.
- **Shadowing** must hold identically in three places: inference vars, the eval env, and
  `collectRoots`' bound set. `withParams` also drops guards rooted at a shadowed name.
- Solver modes: `map`'s source is look-inside; the body may stay symbolic (a `$ref` under `items`
  is productive). Recursive accumulation through `map` is unsupported.
- Comparing two structured values with `==` is refused at registration and at runtime (Go's `==`
  panics on matching uncomparable types); `x == null` is untouched.
- A lambda parses only in a builtin's callback slot.
- Known limit, pinned (`TestEvalEdge_NonBooleanConditionTakesElseBranch`): a non-boolean ternary
  condition silently takes the else branch.

## Root refs

The root walkers descend into call arguments and lambda bodies. Without that, `map` over an
externalized output evaluates against `nil` — a wrong answer, not an error, and only for values
big enough to have been externalized.

## Templates: splitting is parsing

A `${` ends at the first `}` whose body **parses**. Brace counting is unsound — a `}` inside a
string ends the block early and an unbalanced brace desynchronizes the counter. Shortest match is
sound (a longer intended body puts the inner `}` in brackets or a string, where the shorter
candidate fails to parse); when nothing parses, the error comes from the **longest** candidate.
`template.Get` memoises parsed templates by source.

An interpolation that provably resolves to an array or object is refused at registration
(`IsType` means "resolves uniformly to", so nothing ambiguous is refused).

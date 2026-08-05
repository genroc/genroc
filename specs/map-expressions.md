# map, lambdas and JSON literals: design

Status: **implemented.** Grammar in `internal/expression/syntax`, evaluation in
`internal/expression`, typing in `internal/schema/infer.go`, template splitting in
`internal/template`. (Examples predate the `{{ }}` → `${ }` retarget.)

Four constructs: object literals (closed, all keys required), array literals (joined
element type), lambdas (only as a `map` argument), `map`. The point is reshaping a
collection without a per-element task.

## Why the parser is ours

The language was an expr-lang subset; that broke twice, unfixably from outside:
`{` cannot start a predicate body there (statement-block form eats it), and `#` binds
to the innermost predicate, so nested reshaping is inexpressible with pointer syntax.
Named lambda parameters replace `#` entirely; `#` and `.field` are rejected naming the
replacement. **The lexer is still expr-lang's** (string forms, escapes, numeric
literals — exactly what drifts silently if reimplemented); we parse its tokens
ourselves (~450 lines, precedence climbing). Byte literals lex but are rejected. Two
behaviours replicated deliberately because stored definitions depend on them: `??` at
precedence 500 (so `a + b ?? c` is `a + (b ?? c)`), and mixing `??` with another
operator unparenthesized is an error — `prevOp` being local to each `parseBinary`
frame is precisely what makes `a + b ?? c` legal while `a ?? b + c` is not. Owning the
parser buys errors that quote the author's source with a caret.

**The conformance oracle survives** the syntax divergence: `x => body` is exactly
expr-lang's `{let x = #; body}` (verified to bind identically, nested capture and
shadowing included), so the test suite keeps the three-way check via translation. The
`let` rewrite is a test device only.

## Typing

- Object keys are names or quoted strings, duplicates rejected at parse; keys emitted
  sorted so generated schemas are deterministic.
- `[]` types as provably-empty (`maxItems: 0`) — recording "provably holds no element"
  is what makes `?? []` work.
- `map(src, λ)`: reject nullable src (a null source is a runtime panic in expr-lang,
  so it must be a registration error), reject non-array, element type from
  **`Items()`, not `Index()`** (Index is nullable for out-of-bounds constants; map
  visits real elements — backwards, every mapped field is spuriously nullable), reject
  itemless sources (an unconstrained element turns body typos into runtime nulls).
  Union sources join variants' element types, **skipping provably-empty arms** — what
  keeps `map(xs ?? [], x => x.name)` typed rather than degraded; sound, not a `??`
  special case.
- **Shadowing** holds identically in three places, all tested: inference vars, eval
  env, and `collectRoots`' bound set. `withParams` also drops guards rooted at a
  shadowed name — an outer narrowing says nothing about the parameter that now owns
  the name.
- Solver modes: `map`'s source position is look-inside; the body may stay symbolic (a
  `$ref` under `items` counts as productive). Recursive accumulation through `map` is
  unsupported (null seed fails rule 1; `?? []` yields an empty source) — without array
  concatenation it could not express an accumulator anyway.

## Two places this could have leaked

- **Secret taint:** a secret on the *element* type has no path from the root, so the
  walk resolves lambda-rooted paths against the parameter's schema; a failed source
  inference taints (over-tainting costs log verbosity; under-tainting is a leak).
- **Root refs:** had the walkers not descended into call arguments and lambda bodies,
  `map` over an externalized output would evaluate against `nil` — a wrong answer, not
  an error, and only for values big enough to have been externalized. Fixed in
  passing: `shapeRoots` dropped `SelfResult`.

## Templates: splitting is parsing

Block terminators are decided by trying candidate `}}`s until a body **parses** —
brace counting was tried and rejected (any string form containing `}}` ends the block
early; an unbalanced brace inside a string desynchronizes the counter permanently;
making it sound means reimplementing lexer state). Shortest-match is sound (a longer
intended body puts the inner terminator in brackets or a string, where the shorter
candidate fails to parse); when nothing parses, the error comes from the **longest**
candidate, which carries the real syntax error. Since splitting parses anyway,
`template.Template` now holds parsed nodes and `template.Get` memoises by source —
this halved the two-parses-per-leaf-per-tick cost, and the cache boundary is right
because `GetDefinition` deliberately re-unmarshals per call.

A pre-existing hole the literals exposed: `stringify` rejects arrays/objects at
runtime but `InferType` only checked nullability, so `x={{ input.tags }}`
type-checked and died mid-process. `InferType` now rejects a mixed interpolation that
*provably* resolves to a structure (`IsType` = resolves uniformly, so nothing
previously valid breaks).

## Bugs the edge-case sweep found

Six real defects, four predating the feature: `==` on two structured values
**panicked the whole process** (Go's `==` on matching uncomparable types; now rejected
at registration and runtime, `x == null` untouched); the `[]` union used exclusive
`oneOf`, so `{{ xs ?? [] }}` **rejected its own empty result** (fixed by
`absorbEmptyArray` on every union-building path); whole-element copies
(`map(creds, c => c)`) escaped secret taint while field reads didn't — backwards, a
log leak; single-expression `url`/`method` bypassed the structure check the mixed path
had; `parseInt` base 0 made `017` evaluate to 15; lambdas parsed in any position, so
"unreachable" branches were reachable with bare errors — now grammar-gated to builtin
callback slots. Also fixed: output-map errors now name their task. Left deliberately,
recorded in tests: a non-boolean ternary condition silently takes the else branch, and
(pre-numeric-work) float64 arithmetic precision.

## Coverage

160 Go tests across six files — parser tables (precedence, the full `??` matrix,
caret offsets, a no-panic prefix sweep), the eval oracle (~200 cases, 44 cross-checked
against real expr-lang), lambda/literal inference incl. secret vectors, validation
placement tests, template split tables — plus the e2e fan-out test.

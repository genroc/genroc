# Number precision: blast-radius investigation

Status: implemented. The shared definition of a number lives in
`internal/numeric`; arithmetic is in `internal/expression/ops.go`; the decode
boundary is `numeric.Decode`/`DecodeReader`.

> **Syntax note.** Examples below predate the `{{ }}` → `${ }` retarget ([typed-values.md](typed-values.md)): read `{{ x }}` as `${ x }` inside a string, or `"$: x"` where the whole leaf is one expression.

## The problem is at the door, not in the evaluator

Numbers are corrupted by JSON decode/encode alone, with no expression involved:

| input | after `json.Unmarshal` → `json.Marshal` |
|---|---|
| `9007199254740993` | `9007199254740992` |
| `12345678901234567890` | `12345678901234567000` |

`encoding/json` decodes every number into `float64`. So a definition that merely
*forwards* an order ID already mangles it, and swapping the evaluator to decimals
would fix nothing for that case — the value was destroyed before any expression
ran.

Arithmetic is a second, independent problem: `toFloat64` (`expression/ops.go:170`)
routes everything through `float64`, so `0.1 + 0.2 == 0.3` is false and integer
arithmetic loses precision above 2^53.

Fixing transport fidelity is therefore a prerequisite, not an optional first
step. It is also where most of the value is: workflow payloads are mostly
passed through, not computed on.

## Blast radius: smaller than expected

- **Zero** `.(float64)` type assertions in non-test production code.
- **Seven** `case float64:` sites in type switches.
- The two `.(int64)` assertions are sqlc scalar columns, unrelated to context data.
- The wire contract does not change: `json.Number` marshals as a bare JSON
  number, so the HTTP API and the TypeScript e2e suite see identical bytes.
- `json.Number` does **not** satisfy `.(string)` (verified), so it cannot be
  mistaken for a string by any existing assertion.
- `schema/validate.go` **already** handles `json.Number` in `asFloat` and
  `isIntegral`, and `db/paginate.go` already decodes with `UseNumber()`
  deliberately ("keep kindInt values exact (avoid float64 round-trip)"). The
  precedent exists.

## What actually breaks

A type switch that lists only `float64`/`int`/`int64` falls through to its
default when handed a `json.Number` (verified). Ranked by how the failure
presents:

### Silent — these are the dangerous ones

| site | consequence |
|---|---|
| `engine/collect.go:93` `spawnIndex` | returns `!ok`, so a `child_list`'s children lose their recorded order. `_spawn_index` round-trips through stored engine state, so this is reachable in normal operation and produces wrong output rather than an error. |
| `schema/validate.go:290` `enumContains` | compares *marshalled bytes*. Enum values come from the schema document (decoded as `float64`, so `1`), input arrives as its literal (`json.Number("1.0")` marshals as `1.0`). `{"enum":[1]}` would stop accepting the input `1.0`, which it accepts today. A genuine semantic change, not just a missing case. |

### Loud — these fail with a clear error

| site | consequence |
|---|---|
| `template/template.go:258` `stringify` | mixed templates break: `"n={{ input.n }}"` errors with "cannot stringify". Very common; would be caught instantly. |
| `engine/action.go:224` `durationFromValue` | `delay` with `ms` from a number errors with "ms must evaluate to a number". |
| `expression/ops.go:176` `toFloat64` | all arithmetic and comparison on context numbers stops working. |
| `logview/logview.go:211` `valToString` | log values fall to the default branch; cosmetic. |

### Unaffected

`fmt.Sprintf("%v", …)` on a `json.Number` prints the digits, because it is a
string type — so `url`, `method` and `headers` resolution (`action.go:252/264/289`)
needs no change.

## What was built

`internal/numeric` is the single definition of what a number is at runtime.
Evaluation and validation both compare numbers and must agree, so they share it
rather than each carrying their own rules — the same reasoning that keeps
`Infer` and `Eval` on one grammar.

- **Decode**: `numeric.Decode` / `DecodeReader` wrap `UseNumber`, so every number
  in runtime data keeps its exact literal. Applied at the request, transport,
  object-store and instance-state boundaries. It is a no-op for typed structs —
  `UseNumber` only affects values decoded into `interface{}` — so the risk is
  only ever forgetting a site, never applying one too many.
- **Arithmetic**: `+ - *` run in an unlimited-precision `apd` context and are
  exact; their result length is bounded by the operands. Division uses a separate
  34-digit (decimal128) context, because unlimited precision there would try to
  emit infinitely many digits. Inexact division rounds rather than erroring —
  refusing plain `10 / 3` would be surprising — at a documented, deterministic
  point.
- **Canonical value**: arithmetic yields a `json.Number` holding the exact
  decimal text. It marshals as a bare JSON number and round-trips through storage
  without ever passing through float64. Trailing zeros left by the division
  precision are trimmed (`6/3` computes as `2.000…000`, presents as `2`).
- **Four precision policies, no single global one.** Stated authoritatively in
  the `internal/numeric` package doc; summarised here.

  | | policy |
  |---|---|
  | literals | exact, unbounded — normalised to exact decimal text at parse time |
  | `+ - *` | exact, unlimited precision |
  | `/` | rounds at 34 significant digits (decimal128) — the only rounding point |
  | `%` | sized to the operands, floored at the division precision |

  `+ - *` never round, so they are exact. Within a *single expression* growth is
  linear — result length is bounded by the operands and there is no exponentiation
  operator (`**` and `^` are rejected) — measured at 200 → 400 → 600 → 800 digits.

  **Across loop iterations it is exponential**, which the linear-growth reasoning
  above originally missed. A looping task feeds its own output back as
  `self.previous`, so an output like
  `{{ (self.previous.n ?? input.n) * (self.previous.n ?? input.n) }}` doubles the
  digit count every tick: a 54-digit id reaches ~55,000 digits in ten iterations.
  Unbounded, that ran until apd's own exponent limit tripped with `exponent out of
  range` — *after* the value had been materialised and externalised to the object
  store (observed at 54,382 bytes), with a message that explained nothing.

  `numeric.MaxDigits` (1000) now bounds both arithmetic results and literals.
  Nothing is rounded to fit it: exceeding it is an error naming the cause. 1000 is
  far past any legitimate payload — a monetary amount needs ~20 digits, a 256-bit
  hash rendered as decimal 78 — and it trips at iteration 4 rather than 10, before
  any object-store churn.

  A **single global cap** governing all four was considered and rejected: applied
  to `+ - *` it would silently round arithmetic on a value longer than the cap, so
  an id past 34 digits would quietly lose its tail — the exact corruption class
  this work removed. Applied to literals it would truncate them at parse time,
  reintroducing the literal/data asymmetry.

  The division precision is a **constant rather than a setting** for a separate
  reason: genroc retries tasks and re-runs children, so a precision that varied
  between runs — or between two workers mid-deploy — would make the same
  expression yield different values on replay. If it ever must vary, it belongs
  on the versioned definition, not on the server.

  `%` needs its own sizing because `Rem` computes through an integer quotient:
  sharing the fixed 34-digit context made it fail outright with "division
  impossible" on any operand longer than that, which large ids reach. Precision 0
  is not an option either — apd refuses `Rem` without a finite precision, even for
  `10.5 % 3`.

  Literals are bounded by the same `MaxDigits`, so a definition cannot carry an
  arbitrarily long number either.
- **Comparison, enum and bounds** all compare exactly. `enumContains` gained a
  value-based numeric check: enum entries decode from the schema as float64 while
  data now arrives as its literal, so a byte comparison would have silently
  stopped matching `1` against an input of `1.0`.

The integer/number distinction is untouched: it lives in the type system, not in
the runtime representation. One consequence is that `%` is now gated statically
rather than dynamically — `7 % 2.0` is rejected by inference because `2.0` types
as `number`, while evaluation accepts it because 2.0 is a whole number. The
runtime being the more permissive of the two is the safe direction.

### The CLI was the last lossy hop

The engine preserved exact literals end to end while `genctl` still corrupted
them, in both directions:

- **Upload.** `gopkg.in/yaml.v3` decodes a number too large for int64 into a
  `float64`, tagging it `!!float`. A 54-digit id written as a schema default was
  therefore sent to the server as `1.2374829758395876e+53` — destroyed by the
  client before the request left the machine, with the server faithfully storing
  what it was given. Fixed by decoding into a `yaml.Node` and walking it
  (`cmd/genctl/yamlnum.go`), since the node tree still carries the original text.
  The same walker serves `--set` and stdin input.
- **Display.** Responses were decoded with a plain `json.Unmarshal`, so `genctl
  get` rendered a large id through float64 even when the server held it exactly —
  making the CLI disagree with the stored value.

- **`--set`.** A third path, and the one the first fix missed: `--set k=v` does
  not go through the YAML walker at all. Each value was coerced by `inferScalar`
  via `ParseInt` then `ParseFloat`, so anything past int64 fell to the float
  branch and was rounded. It now keeps the literal when the value is valid JSON
  number syntax, and falls through to a string otherwise.

A YAML spelling JSON cannot express (`0x1F`, `0o17`, `007`) falls back to yaml's
own decoding, since a `json.Number` holding it would not marshal.

Covered end to end in `tests/cli/genctl_precision_test.ts`, which drives the
compiled binary through apply → run → get and asserts on raw stdout — parsing it
would be self-defeating, since JavaScript numbers are float64 too. The fixture
YAML is written as raw text for the same reason: the shared `writeDefs` helper
builds YAML from JS objects and would round the values before genctl saw them.

The lesson generalises: exactness is a property of the whole path, not of the
engine. Every hop that decodes into an `interface{}` is a place to lose it.

### Schema documents carry numbers too

`default` and `enum` are `any`-typed, so they decoded through float64 while
runtime data did not — a schema could disagree with the values it describes.
Three separate decode sites had to change: `node.UnmarshalJSON`, `deepClone`
(schema cloning) and `cloneJSON` (used every time a default is filled). Missing
any one of them silently undid the others.

The enum case was the worst of the three, because an enum is a whitelist rather
than a value: declared for `9007199254740993`, it **rejected that value and
admitted `9007199254740992` instead**. Not a rounding artefact — a permission
check keyed on the wrong id.

`minimum`/`maximum` remain `*float64`, so a bound is only as precise as float64
allows; what is now guaranteed is that a value is compared *exactly* against it
rather than rounded first. No realistic hand-written bound reaches 17 significant
digits, so this is a documented limit rather than an open bug.

### Literals match the data path

`IntNode`/`FloatNode` carry the literal's exact text rather than a Go
`int`/`float64`, so writing a value into an expression and receiving it as data
are equally precise. They used to disagree: a literal past int64 was rejected
outright while the identical value arriving as data was exact.

The text is normalised at parse time, which is what makes it safe to emit as
JSON — a radix prefix (`0x1F` → `31`) and the lexer's bare forms (`.5` → `0.5`,
`1.` → `1`) are all valid input but none is valid JSON. The literal's *spelling*
still decides its static type: a fraction or exponent makes it a `number`,
everything else an `integer`. The value is exact either way, so that only
affects which type inference reports.

Array indices keep the old int range check — an index genuinely has to fit in a
Go `int` because it indexes a slice.

## Original plan

1. **Numeric helpers first.** Add `json.Number` to `toFloat64`, `stringify`,
   `spawnIndex`, `durationFromValue`, `valToString`. Do this *before* switching
   any decoder, so the codebase tolerates both representations and each step is
   independently green.
2. **Decide the enum rule.** Either normalize both sides numerically in
   `enumContains` (preserving today's `1` ≡ `1.0` behaviour) or accept literal
   matching. Normalizing is the compatible choice and needs a decimal compare,
   not a byte compare.
3. **`UseNumber()` at the runtime-data decoders** (~16 sites across `api`,
   `db`, `transport`, `engine`). Definition decoding can stay as-is —
   definitions carry no runtime payload numbers.
4. **Arithmetic on `apd`.** `ops.go` converts operands to `*apd.Decimal`,
   computes in an explicit `apd.Context`, and returns a value that marshals
   back to an exact literal. Division needs a precision: decimal128 (34
   significant digits) with round-half-even is the IEEE default, and apd's
   condition flags let us surface `Inexact` rather than rounding silently.
5. **Schema bounds.** `Minimum`/`Maximum` are `*float64` (`schema.go:96`).
   Comparing an exact decimal against a float64 bound reintroduces error at the
   boundary; they should become decimal-backed too.
6. **Integer vs number typing** is unaffected: `IntNode`/`FloatNode` and the
   `integer`/`number` schema distinction are independent of the runtime
   representation, and `/` keeps typing as `number`.

## Outcome

Every predicted breakage was real and was fixed before the flip: `spawnIndex`,
`stringify`, `durationFromValue`, `valToString`, `toFloat64` and `enumContains`.
Nothing else needed changing — the zero-`.(float64)`-assertions finding held.

Verified end to end in `tests/integration/number_precision_test.ts`, which
asserts on the **raw response bytes**: JavaScript numbers are float64 too, so
`JSON.parse` would corrupt the values under test before the assertion ran.

| case | before | now |
|---|---|---|
| forward `9007199254740993` | `9007199254740992` | unchanged |
| forward `123456789.123456789` | rounded | unchanged |
| `0.1 + 0.2` | `0.30000000000000004` | `0.3` |
| `0.1 + 0.2 == 0.3` | `false` | `true` |
| `9007199254740993 + 1` | `9007199254740992` | `9007199254740994` |

## Risk notes

- Step 3 is the flip point. Until it lands nothing changes behaviourally; after
  it lands every context number is a `json.Number`. Steps 1–2 must be complete
  first or the failures above appear.
- Existing stored instance data keeps whatever precision it already lost; this
  is not retroactive.
- The e2e suite is the real regression net here, since it exercises the whole
  decode → store → evaluate → emit path.

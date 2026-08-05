# Number precision

Status: **implemented.** The definition of a number lives in `internal/numeric` (its
package doc is the authoritative statement of the precision policies); arithmetic in
`internal/expression/ops.go`; the decode boundary is `numeric.Decode`/`DecodeReader`.

## The problem is at the door, not in the evaluator

`encoding/json` decodes every number into float64, so `9007199254740993` corrupts on a
plain decode/encode round trip — a definition that merely *forwards* an order id
mangles it before any expression runs. Arithmetic was a second, independent problem
(`0.1 + 0.2 != 0.3`, integers past 2^53). Transport fidelity was therefore the
prerequisite and where most of the value is: payloads are mostly passed through.

Blast radius was measured before the flip: zero `.(float64)` assertions in production,
seven `case float64:` switches. The dangerous failures were the **silent** two —
`spawnIndex` (a `child_list` quietly loses its recorded order) and `enumContains`
(compared marshalled bytes, so `{"enum":[1]}` would stop accepting `1.0`); the rest
failed loudly (`stringify`, `durationFromValue`, `toFloat64`, `valToString`). All six
were fixed *before* switching any decoder, so each step stayed green. Every predicted
breakage was real; nothing unpredicted broke.

## What was built

- **Decode:** `numeric.Decode`/`DecodeReader` wrap `UseNumber` at every runtime-data
  boundary (request, transport, object store, instance state). No-op for typed structs,
  so the only risk is a forgotten site, never one too many.
- **Arithmetic:** `+ - *` exact in an unlimited-precision context; `/` rounds at 34
  significant digits (decimal128) — the single rounding point; `%` sized to its
  operands (a fixed context failed outright on long operands; apd refuses precision 0).
  Results are `json.Number` exact-decimal text; division's trailing zeros trimmed.
- **Four precision policies, no single global one** — a global cap applied to `+ - *`
  would silently round long ids (the exact corruption class removed), applied to
  literals would truncate at parse. `numeric.MaxDigits` (1000) bounds results and
  literals with an *error naming the cause*, never rounding: growth within one
  expression is linear (no exponentiation operator), but a looping task re-feeding its
  output squares digit counts per tick — unbounded, that ran to apd's exponent limit
  *after* externalizing a 54KB number, with a useless message. 1000 is far past any
  legitimate payload and trips at iteration 4.
- **Division precision is a constant, not a setting:** genroc retries and re-runs, so
  precision varying between runs or workers would make replay non-deterministic. If it
  must ever vary, it belongs on the versioned definition.
- **Comparison/enum/bounds compare exactly.** `enumContains` gained a value-based
  numeric check (the enum whitelist case was the worst pre-fix behaviour: declared for
  `9007199254740993`, it rejected that value and admitted `...992` instead — a
  permission check keyed on the wrong id). `minimum`/`maximum` stay `*float64` —
  documented limit: the bound is float-precise, the comparison against it exact.
- **Schema documents carry numbers too:** `default`/`enum` decode through three sites
  (`node.UnmarshalJSON`, `deepClone`, `cloneJSON`) — missing any one silently undoes
  the others.
- **Literals match the data path:** `IntNode`/`FloatNode` carry exact text, normalised
  at parse (`0x1F` → `31`, `.5` → `0.5`) so it is valid JSON; spelling still decides
  the static type (fraction/exponent ⇒ number). Array indices keep the Go-int check.
- **The CLI was the last lossy hop**, in three places: YAML upload (yaml.v3 floats
  big ints — fixed by walking the `yaml.Node` tree, which keeps the original text),
  display (plain `json.Unmarshal`), and `--set` (ParseInt-then-ParseFloat coercion).
  Non-JSON YAML spellings (`0x1F`, `007`) fall back to yaml's own decoding.

One consequence: `%` is gated statically (`7 % 2.0` rejected by inference since `2.0`
types as `number`) while the runtime accepts whole-numbered floats — the runtime being
the more permissive side is the safe direction.

## Verification

`tests/integration/number_precision_test.ts` and `tests/cli/genctl_precision_test.ts`
assert on **raw bytes** — JavaScript numbers are float64 too, so `JSON.parse` (or
building fixtures from JS objects) would corrupt the values under test before the
assertion ran. The lesson that generalises: exactness is a property of the whole path;
every hop that decodes into `interface{}` is a place to lose it. Stored data keeps
whatever precision it already lost — nothing is retroactive.

# Number precision

Status: **Built.** `internal/numeric` is the one definition of a number (its package doc states
the policies); arithmetic is `internal/expression/ops.go`.

## Transport first

`encoding/json` decodes every number to float64, so `9007199254740993` corrupts on a plain round
trip — a definition that only forwards an order id would mangle it. Payloads are mostly passed
through, so fidelity at every hop matters more than arithmetic. `numeric.Decode`/`DecodeReader`
wrap `UseNumber` at every runtime-data boundary (request, transport, object store, instance
state); a no-op for typed structs, so the only risk is a forgotten site. Every hop that decodes
into `any` is a place to lose exactness.

## Arithmetic

- `+ - *` are exact; `/` rounds at a **constant** 34 significant digits (decimal128), the single
  rounding point; `%` sizes its context to its operands (a fixed one fails on long operands).
  A constant because retries and re-runs must replay to the same value — if it must ever vary, it
  belongs on the versioned definition.
- **No cap that rounds.** On `+ - *` it would round long ids; on literals it would truncate at
  parse. `numeric.MaxDigits` (1000) bounds results and literals with an error instead: a loop
  re-feeding its output doubles its digits per tick.
- `%` is gated statically (`7 % 2.0` refused — `2.0` types as `number`) while the runtime accepts
  whole-numbered floats; the runtime being more permissive is the safe direction.

## Comparison and schemas

- Comparison, `enum` and bounds compare exactly. `enumContains` compares numbers by value, so
  `{"enum":[1]}` accepts `1.0` and an enum of `9007199254740993` does not admit `…992`.
  `minimum`/`maximum` stay `*float64`: the bound is float-precise, the comparison exact.
- Schema `default`/`enum` decode through three sites — `node.UnmarshalJSON`, `deepClone`,
  `cloneJSON` — and missing one silently undoes the others.
- `IntNode`/`FloatNode` carry exact text normalised at parse (`0x1F` → `31`, `.5` → `0.5`) so it
  is valid JSON; spelling decides the static type (fraction/exponent ⇒ `number`).
- A shape literal (`output: {n: 3.0}`) is the `json.Number` `Shape.UnmarshalJSON` decodes, typed by
  the same spelling rule, so `3.0` is `number` written either way.

## The CLI

Three hops that were lossy: YAML upload (`defdoc`'s `scalar` keeps the source text as
`json.Number`; yaml.v3 floats big ints, so non-JSON spellings like `0x1F` fall back to it),
display (`numeric.Decode`, not `json.Unmarshal`), and `--set` (`inferScalar`, not
`ParseInt`/`ParseFloat`).

## Verification

`tests/integration/number_precision_test.ts` and `tests/cli/precision_test.ts` assert on **raw
bytes**: a JS number is float64, so `JSON.parse` would corrupt the value before the assertion
ran. Stored data keeps whatever precision it already lost.

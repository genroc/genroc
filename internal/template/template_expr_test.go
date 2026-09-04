package template

import "testing"

// A $: leaf is one typed expression: it parses to a single EXPR chunk (no literal
// text), evaluates with its type preserved, and infers to the expression's type —
// unlike a ${ } template, which stringifies.

func TestExprMarker_SplitsToSingleExpression(t *testing.T) {
	assertSplit(t, `$: input.n`, `EXPR("input.n")`)
	assertSplit(t, `$:input.n`, `EXPR("input.n")`)       // space after marker optional
	assertSplit(t, `   $:  input.n `, `EXPR("input.n")`) // leading whitespace tolerated
}

func TestExprMarker_EvalPreservesType(t *testing.T) {
	assertEvalAny(t, `$: input.n`, 3)        // integer, not "3"
	assertEvalAny(t, `$: input.name`, "ann") // string
	assertEvalAny(t, `$: input.ok`, true)    // boolean, not "true"
	assertEvalAny(t, `  $: input.n `, 3)     // whitespace-tolerant
}

func TestExprMarker_InferPreservesType(t *testing.T) {
	assertInferType(t, `$: input.n`, "integer")
	assertInferType(t, `$: input.name`, "string")
	// An array/object result is fine as a typed leaf (a ${ } template would reject it
	// as un-stringifiable, but $: preserves the type).
	assertInferType(t, `$: input.tags`, "array")
}

func TestExprMarker_ParseErrorNamesExpression(t *testing.T) {
	assertParseError(t, `$: input.`, "expression")
}

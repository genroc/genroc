package template

import "testing"

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
	// A ${ } template would reject an array as un-stringifiable; $: preserves it.
	assertInferType(t, `$: input.tags`, "array")
}

func TestExprMarker_ParseErrorNamesExpression(t *testing.T) {
	assertParseError(t, `$: input.`, "expression")
}

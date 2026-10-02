package expressiontest

import "testing"

// Equality against a non-nil literal narrows x to the literal's type in the matching branch.
// Schemas are in helpers_test.go.

func TestConditionalLiteralNarrowing_EqStrThenConcat(t *testing.T) {
	testNullableNarrowValid(t, `x == "hi" ? x + "!" : ""`, nullableString)
}

func TestConditionalLiteralNarrowing_NeStrElseConcat(t *testing.T) {
	testNullableNarrowValid(t, `x != "hi" ? "" : x + "!"`, nullableString)
}

func TestConditionalLiteralNarrowing_EqIntThenAdd(t *testing.T) {
	testNullableNarrowValid(t, "x == 5 ? x + 1 : 0", nullableInteger)
}

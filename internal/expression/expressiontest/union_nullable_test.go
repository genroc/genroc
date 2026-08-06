package expressiontest

import "testing"

// Per-operator null compatibility: x declared T|null and nil at runtime. InferType must predict
// which operators accept nil (equality) and which reject it (arithmetic, ordering, logical),
// matching expr-lang. Schemas live in helpers_test.go.

func TestNullableContract_AddInt(t *testing.T) {
	testNullableOperand(t, "x + 1", nullableInteger)
}

func TestNullableContract_SubInt(t *testing.T) {
	testNullableOperand(t, "x - 1", nullableInteger)
}

func TestNullableContract_MulInt(t *testing.T) {
	testNullableOperand(t, "x * 2", nullableInteger)
}

func TestNullableContract_DivInt(t *testing.T) {
	testNullableOperand(t, "x / 2", nullableInteger)
}

func TestNullableContract_UnaryNeg(t *testing.T) {
	testNullableOperand(t, "-x", nullableInteger)
}

func TestNullableContract_StrConcat(t *testing.T) {
	testNullableOperand(t, `x + "s"`, nullableString)
}

func TestNullableContract_Lt(t *testing.T) {
	testNullableOperand(t, "x < 1", nullableInteger)
}

func TestNullableContract_Gt(t *testing.T) {
	testNullableOperand(t, "x > 1", nullableInteger)
}

func TestNullableContract_Le(t *testing.T) {
	testNullableOperand(t, "x <= 1", nullableInteger)
}

func TestNullableContract_Ge(t *testing.T) {
	testNullableOperand(t, "x >= 1", nullableInteger)
}

// Equality comparisons against null are valid for any nullable type — tested via
// InferType only, since expr-lang doesn't treat "null" as a built-in keyword.
func TestNullableContract_Eq(t *testing.T) {
	for _, ns := range nullableInteger {
		t.Run(ns.name, func(t *testing.T) { infer(t, "x == null", ns.schema) })
	}
}

func TestNullableContract_Neq(t *testing.T) {
	for _, ns := range nullableInteger {
		t.Run(ns.name, func(t *testing.T) { infer(t, "x != null", ns.schema) })
	}
}

func TestNullableContract_And(t *testing.T) {
	testNullableOperand(t, "x && true", nullableBoolean)
}

func TestNullableContract_Or(t *testing.T) {
	testNullableOperand(t, "x || false", nullableBoolean)
}

func TestNullableContract_Not(t *testing.T) {
	testNullableOperand(t, "!x", nullableBoolean)
}

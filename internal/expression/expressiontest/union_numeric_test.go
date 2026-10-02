package expressiontest

import "testing"

// An all-numeric union (integer | number) must pass all three layers with both integer and
// float runtime values. Schemas are in helpers_test.go.

func TestNumericUnionContract_Add(t *testing.T) {
	testNumericUnionCase(t, "x + 1", numericUnionSchemas)
}

func TestNumericUnionContract_Sub(t *testing.T) {
	testNumericUnionCase(t, "x - 1", numericUnionSchemas)
}

func TestNumericUnionContract_Mul(t *testing.T) {
	testNumericUnionCase(t, "x * 2", numericUnionSchemas)
}

func TestNumericUnionContract_Div(t *testing.T) {
	testNumericUnionCase(t, "x / 2", numericUnionSchemas)
}

func TestNumericUnionContract_UnaryNeg(t *testing.T) {
	testNumericUnionCase(t, "-x", numericUnionSchemas)
}

func TestNumericUnionContract_Lt(t *testing.T) {
	testNumericUnionCase(t, "x < 100", numericUnionSchemas)
}

func TestNumericUnionContract_Gt(t *testing.T) {
	testNumericUnionCase(t, "x > 100", numericUnionSchemas)
}

func TestNumericUnionContract_Le(t *testing.T) {
	testNumericUnionCase(t, "x <= 100", numericUnionSchemas)
}

func TestNumericUnionContract_Ge(t *testing.T) {
	testNumericUnionCase(t, "x >= 100", numericUnionSchemas)
}

func TestNumericUnionContract_Eq(t *testing.T) {
	testNumericUnionCase(t, "x == 5", numericUnionSchemas)
}

func TestNumericUnionContract_Neq(t *testing.T) {
	testNumericUnionCase(t, "x != 5", numericUnionSchemas)
}

// % requires integer operands — unlike the other arithmetic operators it rejects
// floats at runtime. InferType must also reject integer|number unions for %.
func TestNumericUnionContract_Mod(t *testing.T) {
	infer(t, "x % 2", integerXSchema)
	evalOK(t, "x % 2", map[string]any{"x": 5})
	evalErr(t, "x % 2", map[string]any{"x": 5.5})
}

func TestNumericUnionContract_ModRejectsNumericUnion(t *testing.T) {
	for _, ns := range numericUnionSchemas {
		t.Run(ns.name, func(t *testing.T) {
			inferErr(t, "x % 2", ns.schema, "")
		})
	}
}

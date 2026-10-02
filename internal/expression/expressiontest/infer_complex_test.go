package expressiontest

import (
	"testing"

	"genroc/internal/schema"
)

// complexCtxJSON is a context schema whose fields use anyOf and oneOf.
const complexCtxJSON = `{
	"type": "object",
	"properties": {
		"input": {
			"type": "object",
			"properties": {
				"flexible":      { "anyOf": [{"type":"integer"}, {"type":"string"}] },
				"exactly_one":   { "oneOf": [{"type":"integer"}, {"type":"number"}] },
				"numeric_union": { "anyOf": [{"type":"integer"}, {"type":"number"}] },
				"obj_union": {
					"anyOf": [
						{"type":"object","properties":{"x":{"type":"integer"}},"required":["x"]},
						{"type":"object","properties":{"x":{"type":"string"}},"required":["x"]}
					]
				},
				"obj_union_oneof": {
					"oneOf": [
						{"type":"object","properties":{"x":{"type":"boolean"}},"required":["x"]},
						{"type":"object","properties":{"x":{"type":"number"}},"required":["x"]}
					]
				},
				"obj_union_same": {
					"anyOf": [
						{"type":"object","properties":{"x":{"type":"integer"}},"required":["x"]},
						{"type":"object","properties":{"x":{"type":"integer"}},"required":["x"]}
					]
				},
				"obj_union_nested": {
					"anyOf": [
						{"type":"object","properties":{"inner":{"type":"object","properties":{"z":{"type":"boolean"}},"required":["z"]}},"required":["inner"]},
						{"type":"object","properties":{"inner":{"type":"object","properties":{"z":{"type":"integer"}},"required":["z"]}},"required":["inner"]}
					]
				},
				"obj_union_anyof_in_oneof": {
					"oneOf": [
						{"type":"object","properties":{"x":{"anyOf":[{"type":"number"},{"type":"object","properties":{"y":{"type":"boolean"}},"required":["y"]}]}},"required":["x"]},
						{"type":"object","properties":{"x":{"anyOf":[{"type":"integer"}]}},"required":["x"]}
					]
				}
			},
			"required": ["flexible", "exactly_one", "numeric_union", "obj_union", "obj_union_oneof", "obj_union_same", "obj_union_nested", "obj_union_anyof_in_oneof"]
		}
	},
	"required": ["input"]
}`

// --- field access: complex schemas are returned as-is ---

func TestInfer_AnyOf_FieldReturnsSchema(t *testing.T) {
	c := ctx(t, complexCtxJSON)
	assertSchema(t, infer(t, "input.flexible", c), `{
		"anyOf": [{"type":"integer"}, {"type":"string"}]
	}`)
}

func TestInfer_OneOf_FieldReturnsSchema(t *testing.T) {
	c := ctx(t, complexCtxJSON)
	assertSchema(t, infer(t, "input.exactly_one", c), `{
		"oneOf": [{"type":"integer"}, {"type":"number"}]
	}`)
}

// --- comparison operators always produce boolean regardless of operand schema ---

func TestInfer_AnyOf_EqualityIsBoolean(t *testing.T) {
	c := ctx(t, complexCtxJSON)
	assertSchema(t, infer(t, `input.flexible == "x"`, c), `{"type":"boolean"}`)
}

func TestInfer_AnyOf_InequalityIsBoolean(t *testing.T) {
	c := ctx(t, complexCtxJSON)
	assertSchema(t, infer(t, "input.flexible != 0", c), `{"type":"boolean"}`)
}

func TestInfer_AnyOf_OrderComparisonFails(t *testing.T) {
	c := ctx(t, complexCtxJSON)
	inferErr(t, "input.flexible < 10", c, "unambiguous")
}

func TestInfer_OneOf_EqualityIsBoolean(t *testing.T) {
	c := ctx(t, complexCtxJSON)
	assertSchema(t, infer(t, "input.exactly_one == 0", c), `{"type":"boolean"}`)
}

// --- logical operators always produce boolean ---

func TestInfer_AnyOf_LogicalAndIsBoolean(t *testing.T) {
	c := ctx(t, complexCtxJSON)
	// both operands are comparisons → boolean && boolean → boolean
	assertSchema(t, infer(t, "input.flexible == 1 && input.flexible == 2", c), `{"type":"boolean"}`)
}

func TestInfer_AnyOf_UnaryNotIsBoolean(t *testing.T) {
	c := ctx(t, complexCtxJSON)
	// ! wraps a comparison; the inner comparison is boolean so ! is valid
	assertSchema(t, infer(t, "!(input.flexible == 1)", c), `{"type":"boolean"}`)
}

// --- arithmetic on complex schemas ---

// flexible is anyOf[integer, string] — incompatible types for arithmetic.
func TestInfer_AnyOf_ArithmeticFails(t *testing.T) {
	c := ctx(t, complexCtxJSON)
	inferErr(t, "input.flexible + 1", c, "unambiguous")
}

// exactly_one is oneOf[integer, number] — all numeric variants, so arithmetic is allowed.
func TestInfer_OneOf_AllNumericArithmeticOK(t *testing.T) {
	c := ctx(t, complexCtxJSON)
	// integer widened to number because oneOf contains both integer and number.
	assertSchema(t, infer(t, "input.exactly_one * 2", c), `{"type":"number"}`)
}

// --- nullable types ---

// nullableCtxJSON has fields declared with the {type: [X, "null"]} notation.
const nullableCtxJSON = `{
	"type": "object",
	"properties": {
		"input": {
			"type": "object",
			"properties": {
				"id":      { "type": "integer" },
				"comment": { "type": ["string", "null"] },
				"amount":  { "type": ["number", "null"] }
			},
			"required": ["id", "comment", "amount"]
		}
	},
	"required": ["input"]
}`

// Accessing a nullable field returns its schema as-is.
func TestInfer_Nullable_FieldReturnsSchema(t *testing.T) {
	c := ctx(t, nullableCtxJSON)
	assertSchema(t, infer(t, "input.comment", c), `{"type":["string","null"]}`)
}

// A conditional where one branch is nil produces a nullable type.
func TestInfer_Nullable_ConditionalWithNil_String(t *testing.T) {
	c := ctx(t, nullableCtxJSON)
	assertSchema(t, infer(t, `input.id > 0 ? input.comment : null`, c), `{"type":["string","null"]}`)
}

func TestInfer_Nullable_ConditionalWithNil_Integer(t *testing.T) {
	c := ctx(t, nullableCtxJSON)
	assertSchema(t, infer(t, `true ? input.id : null`, c), `{"type":["integer","null"]}`)
}

// nil on the left branch works the same way.
func TestInfer_Nullable_ConditionalNilFirst(t *testing.T) {
	c := ctx(t, nullableCtxJSON)
	assertSchema(t, infer(t, `false ? null :input.id`, c), `{"type":["integer","null"]}`)
}

// Both branches nil → plain null type (not a type array).
func TestInfer_Nullable_BothBranchesNil(t *testing.T) {
	assertSchema(t, infer(t, `true ? null :null`, schema.Schema{}), `{"type":"null"}`)
}

// Arithmetic on a nullable type fails — the null case is unhandled.
func TestInfer_Nullable_ArithmeticFails(t *testing.T) {
	c := ctx(t, nullableCtxJSON)
	inferErr(t, "input.amount + 1.0", c, "non-nullable")
}

// Comparisons on nullable types still return boolean.
func TestInfer_Nullable_ComparisonIsBoolean(t *testing.T) {
	c := ctx(t, nullableCtxJSON)
	assertSchema(t, infer(t, "input.comment == null", c), `{"type":"boolean"}`)
	assertSchema(t, infer(t, "input.amount != null", c), `{"type":"boolean"}`)
}

// A non-null + complex-type branch still falls back to oneOf.
func TestInfer_Nullable_ComplexBranchFallsBackToOneOf(t *testing.T) {
	c := ctx(t, nullableCtxJSON)
	// anyOf field vs nil — the non-null branch has no simple type string
	assertSchema(t, infer(t, `true ? input.comment : input.id`, c), `{
		"oneOf": [{"type":["string","null"]}, {"type":"integer"}]
	}`)
}

// --- member access through anyOf / oneOf object variants ---

// TestInfer_AnyOf_MemberAccess_DifferentTypes accesses .x on a field that is
// anyOf two object schemas each with a differently typed .x — result is anyOf
// of those two types.
func TestInfer_AnyOf_MemberAccess_DifferentTypes(t *testing.T) {
	c := ctx(t, complexCtxJSON)
	assertSchema(t, infer(t, "input.obj_union.x", c), `{
		"anyOf": [{"type":"integer"}, {"type":"string"}]
	}`)
}

// TestInfer_OneOf_MemberAccess_DifferentTypes same as above but oneOf.
func TestInfer_OneOf_MemberAccess_DifferentTypes(t *testing.T) {
	c := ctx(t, complexCtxJSON)
	assertSchema(t, infer(t, "input.obj_union_oneof.x", c), `{
		"oneOf": [{"type":"boolean"}, {"type":"number"}]
	}`)
}

// TestInfer_AnyOf_MemberAccess_SameType deduplicates identical variant results
// and returns the type directly instead of wrapping in anyOf.
func TestInfer_AnyOf_MemberAccess_SameType(t *testing.T) {
	c := ctx(t, complexCtxJSON)
	assertSchema(t, infer(t, "input.obj_union_same.x", c), `{"type":"integer"}`)
}

// TestInfer_AnyOf_MemberAccess_DeepPath walks through two levels of anyOf.
func TestInfer_AnyOf_MemberAccess_DeepPath(t *testing.T) {
	c := ctx(t, complexCtxJSON)
	assertSchema(t, infer(t, "input.obj_union_nested.inner.z", c), `{
		"anyOf": [{"type":"boolean"}, {"type":"integer"}]
	}`)
}

func TestInfer_OneOf_in_AnyOf_accessValueWhichExistsOnlyInSomeVariants(t *testing.T) {
	c := ctx(t, complexCtxJSON)
	assertSchema(t, infer(t, "input.obj_union_anyof_in_oneof.x.y", c), `{
		"type":["boolean","null"]
	}`)
}

func TestInfer_OneOf_in_AnyOf(t *testing.T) {
	c := ctx(t, complexCtxJSON)
	inferErr(t, "input.obj_union_anyof_in_oneof.x.f", c, `"f" not found in any`)
}

// TestInfer_AnyOf_MemberAccess_MissingInVariant allows access when a property
// exists in at least one variant; variants that lack it contribute null.
func TestInfer_AnyOf_MemberAccess_MissingInVariant(t *testing.T) {
	c := ctx(t, `{
		"type": "object",
		"properties": {
			"input": {
				"type": "object",
				"properties": {
					"partial": {
						"anyOf": [
							{"type":"object","properties":{"x":{"type":"integer"}},"required":["x"]},
							{"type":"object","properties":{"y":{"type":"string"}},"required":["y"]}
						]
					}
				},
				"required": ["partial"]
			}
		},
		"required": ["input"]
	}`)
	assertSchema(t, infer(t, "input.partial.x", c), `{"type":["integer","null"]}`)
}

// TestInfer_AnyOf_MemberAccess_NonObjectVariant errors when no variant has the property.
func TestInfer_AnyOf_MemberAccess_NonObjectVariant(t *testing.T) {
	c := ctx(t, complexCtxJSON)
	// input.flexible is anyOf [integer, string] — neither has properties
	inferErr(t, "input.flexible.x", c, `"x" not found in any anyOf variant`)
}

// The pattern save_order_output → check_fraud.result relies on.
func TestInfer_OneOf_ObjectAndNonObject_PropertyAccess(t *testing.T) {
	c := ctx(t, `{
		"type": "object",
		"properties": {
			"outputs": {
				"type": "object",
				"properties": {
					"save_order": {
						"$ref": "#/$defs/save_order_output"
					}
				},
				"required": ["save_order"]
			}
		},
		"required": ["outputs"],
		"$defs": {
			"save_order_output": {
				"oneOf": [
					{"type":"object","properties":{"valid":{"type":"boolean"}}},
					{"type":"string"}
				]
			}
		}
	}`)
	assertSchema(t, infer(t, "outputs.save_order.valid", c), `{"type":["boolean","null"]}`)
}

// --- member access on all-null variants ---

// Not a type error: the property cannot exist at runtime, but that is a likely bug to warn
// about, not a hard failure.
func TestInfer_AnyOf_AllNullVariants_MemberAccess(t *testing.T) {
	c := ctx(t, `{
		"type": "object",
		"properties": {
			"input": {
				"type": "object",
				"properties": {
					"always_null": {
						"anyOf": [{"type":"null"}, {"type":"null"}]
					}
				},
				"required": ["always_null"]
			}
		},
		"required": ["input"]
	}`)
	assertSchema(t, infer(t, "input.always_null.x", c), `{"type":"null"}`)
}

// TestInfer_OneOf_AllNullVariants_MemberAccess same as above but oneOf.
func TestInfer_OneOf_AllNullVariants_MemberAccess(t *testing.T) {
	c := ctx(t, `{
		"type": "object",
		"properties": {
			"input": {
				"type": "object",
				"properties": {
					"always_null": {
						"oneOf": [{"type":"null"}]
					}
				},
				"required": ["always_null"]
			}
		},
		"required": ["input"]
	}`)
	assertSchema(t, infer(t, "input.always_null.x", c), `{"type":"null"}`)
}

// TestInfer_AnyOf_NullAndObject_MemberAccess checks that mixing null variants
// with object variants still resolves the property, with null added to the result.
func TestInfer_AnyOf_NullAndObject_MemberAccess(t *testing.T) {
	c := ctx(t, `{
		"type": "object",
		"properties": {
			"input": {
				"type": "object",
				"properties": {
					"maybe": {
						"anyOf": [
							{"type":"null"},
							{"type":"object","properties":{"x":{"type":"integer"}},"required":["x"]}
						]
					}
				},
				"required": ["maybe"]
			}
		},
		"required": ["input"]
	}`)
	assertSchema(t, infer(t, "input.maybe.x", c), `{"type":["integer","null"]}`)
}

// --- conditional with complex schemas ---

func TestInfer_Conditional_AnyOfBothBranches(t *testing.T) {
	c := ctx(t, complexCtxJSON)
	// both branches have the same anyOf schema → result is that schema
	assertSchema(t, infer(t, "true ? input.flexible : input.flexible", c), `{
		"anyOf": [{"type":"integer"}, {"type":"string"}]
	}`)
}

func TestInfer_Conditional_AnyOfVsLiteral(t *testing.T) {
	c := ctx(t, complexCtxJSON)
	// branches differ → oneOf wrapping both
	assertSchema(t, infer(t, "true ? input.flexible : 42", c), `{
		"oneOf": [
			{"anyOf": [{"type":"integer"}, {"type":"string"}]},
			{"type":"integer"}
		]
	}`)
}

func TestInfer_Conditional_OneOfVsOneOf(t *testing.T) {
	c := ctx(t, complexCtxJSON)
	// two different oneOf schemas → wrapped in oneOf
	assertSchema(t, infer(t, "true ? input.flexible : input.exactly_one", c), `{
		"oneOf": [
			{"anyOf": [{"type":"integer"}, {"type":"string"}]},
			{"oneOf": [{"type":"integer"}, {"type":"number"}]}
		]
	}`)
}

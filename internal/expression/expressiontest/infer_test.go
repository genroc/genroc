package expressiontest

import (
	"testing"

	"genroc/internal/schema"
)

// --- Literals ---

func TestInfer_IntegerLiteral(t *testing.T) {
	assertSchema(t, infer(t, "42", schema.Schema{}), `{"type":"integer"}`)
}

func TestInfer_FloatLiteral(t *testing.T) {
	assertSchema(t, infer(t, "3.14", schema.Schema{}), `{"type":"number"}`)
}

func TestInfer_StringLiteral(t *testing.T) {
	assertSchema(t, infer(t, `"hello"`, schema.Schema{}), `{"type":"string"}`)
}

func TestInfer_BoolLiteral(t *testing.T) {
	assertSchema(t, infer(t, "true", schema.Schema{}), `{"type":"boolean"}`)
	assertSchema(t, infer(t, "false", schema.Schema{}), `{"type":"boolean"}`)
}

func TestInfer_NullLiteral(t *testing.T) {
	assertSchema(t, infer(t, "null", schema.Schema{}), `{"type":"null"}`)
}

// --- Field access ---

func TestInfer_TopLevelField(t *testing.T) {
	c := ctx(t, richContextJSON)
	assertSchema(t, infer(t, "input", c), `{
		"type": "object",
		"properties": {
			"order_id": {"type":"integer"},
			"amount":   {"type":"number"},
			"label":    {"type":"string"},
			"active":   {"type":"boolean"}
		},
		"required": ["order_id", "amount", "label", "active"]
	}`)
}

func TestInfer_NestedField_Integer(t *testing.T) {
	c := ctx(t, richContextJSON)
	assertSchema(t, infer(t, "input.order_id", c), `{"type":"integer"}`)
}

func TestInfer_NestedField_Number(t *testing.T) {
	c := ctx(t, richContextJSON)
	assertSchema(t, infer(t, "input.amount", c), `{"type":"number"}`)
}

func TestInfer_NestedField_Boolean(t *testing.T) {
	c := ctx(t, richContextJSON)
	assertSchema(t, infer(t, "outputs.charge.charged", c), `{"type":"boolean"}`)
}

func TestInfer_NestedField_DeepPath(t *testing.T) {
	c := ctx(t, richContextJSON)
	assertSchema(t, infer(t, "outputs.charge.fee", c), `{"type":"number"}`)
}

func TestInfer_FieldNotFound(t *testing.T) {
	c := ctx(t, richContextJSON)
	inferErr(t, "input.missing", c, `field "missing" not found in schema`)
}

func TestInfer_FieldOnNonObject(t *testing.T) {
	c := ctx(t, richContextJSON)
	inferErr(t, "input.order_id.x", c, "cannot access .x: schema has no properties")
}

// --- $ref resolution ---

func TestInfer_RefResolution(t *testing.T) {
	c := ctx(t, `{
		"type": "object",
		"properties": {
			"input": { "$ref": "#/$defs/Input" }
		},
		"required": ["input"],
		"$defs": {
			"Input": {
				"type": "object",
				"properties": {
					"order_id": { "type": "integer" }
				},
				"required": ["order_id"]
			}
		}
	}`)
	assertSchema(t, infer(t, "input.order_id", c), `{"type":"integer"}`)
}

func TestInfer_RefMissing(t *testing.T) {
	c := ctx(t, `{
		"type": "object",
		"properties": {
			"input": { "$ref": "#/$defs/Missing" }
		},
		"$defs": {}
	}`)
	inferErr(t, "input.order_id", c, `$ref "#/$defs/Missing" not found in defs`)
}

func TestInfer_RefWithoutDefs(t *testing.T) {
	c := ctx(t, `{
		"type": "object",
		"properties": {
			"input": { "$ref": "#/$defs/Input" }
		}
	}`)
	inferErr(t, "input.order_id", c, "cannot resolve $ref")
}

// --- Arithmetic ---

func TestInfer_IntPlusInt(t *testing.T) {
	c := ctx(t, richContextJSON)
	assertSchema(t, infer(t, "input.order_id + 1", c), `{"type":"integer"}`)
}

func TestInfer_IntPlusFloat(t *testing.T) {
	c := ctx(t, richContextJSON)
	assertSchema(t, infer(t, "input.order_id + input.amount", c), `{"type":"number"}`)
}

func TestInfer_FloatArithmetic(t *testing.T) {
	c := ctx(t, richContextJSON)
	assertSchema(t, infer(t, "input.amount * 2.0", c), `{"type":"number"}`)
}

func TestInfer_IntegerSubtraction(t *testing.T) {
	c := ctx(t, richContextJSON)
	assertSchema(t, infer(t, "input.order_id - 1", c), `{"type":"integer"}`)
}

func TestInfer_StringConcatenation(t *testing.T) {
	c := ctx(t, richContextJSON)
	assertSchema(t, infer(t, `input.label + "_suffix"`, c), `{"type":"string"}`)
}

func TestInfer_ArithmeticOnStrings(t *testing.T) {
	c := ctx(t, richContextJSON)
	inferErr(t, "input.label - 1", c, "operator requires numeric operands")
}

func TestInfer_ArithmeticOnBool(t *testing.T) {
	c := ctx(t, richContextJSON)
	inferErr(t, "input.active + 1", c, "operator requires numeric operands")
}

// --- Unary ---

func TestInfer_UnaryNot(t *testing.T) {
	c := ctx(t, richContextJSON)
	assertSchema(t, infer(t, "!input.active", c), `{"type":"boolean"}`)
}

func TestInfer_UnaryMinus(t *testing.T) {
	c := ctx(t, richContextJSON)
	assertSchema(t, infer(t, "-input.order_id", c), `{"type":"integer"}`)
}

func TestInfer_UnaryMinusOnString(t *testing.T) {
	c := ctx(t, richContextJSON)
	inferErr(t, "-input.label", c, "unary operator requires a numeric operand")
}

// --- Comparison ---

func TestInfer_ComparisonEq(t *testing.T) {
	c := ctx(t, richContextJSON)
	assertSchema(t, infer(t, "input.order_id == 0", c), `{"type":"boolean"}`)
}

func TestInfer_ComparisonLt(t *testing.T) {
	c := ctx(t, richContextJSON)
	assertSchema(t, infer(t, "input.amount < 100.0", c), `{"type":"boolean"}`)
}

func TestInfer_LogicalAnd(t *testing.T) {
	c := ctx(t, richContextJSON)
	assertSchema(t, infer(t, "input.active && outputs.charge.charged", c), `{"type":"boolean"}`)
}

func TestInfer_LogicalOr(t *testing.T) {
	c := ctx(t, richContextJSON)
	assertSchema(t, infer(t, "input.active || false", c), `{"type":"boolean"}`)
}

// --- Conditional ---

func TestInfer_Conditional_SameType(t *testing.T) {
	c := ctx(t, richContextJSON)
	assertSchema(t, infer(t, "input.active ? 1 : 2", c), `{"type":"integer"}`)
}

func TestInfer_Conditional_DifferentTypes(t *testing.T) {
	c := ctx(t, richContextJSON)
	assertSchema(t, infer(t, `input.active ? input.order_id : "unknown"`, c), `{
		"oneOf": [{"type":"integer"}, {"type":"string"}]
	}`)
}

func TestInfer_Conditional_FieldFromContext(t *testing.T) {
	c := ctx(t, richContextJSON)
	assertSchema(t,
		infer(t, "outputs.charge.charged ? input.amount : 0.0", c),
		`{"type":"number"}`)
}

// --- Null coalescing ---

func TestInfer_NullCoalesce_NullableField_SameType(t *testing.T) {
	// optional integer field ?? integer literal → integer (null stripped)
	c := ctx(t, `{
		"type": "object",
		"properties": { "count": {"type": "integer"} },
		"required": []
	}`)
	assertSchema(t, infer(t, "count ?? 0", c), `{"type":"integer"}`)
}

func TestInfer_NullCoalesce_AlwaysNull_ReturnsRight(t *testing.T) {
	assertSchema(t, infer(t, "null ?? 0", schema.Schema{}), `{"type":"integer"}`)
}

func TestInfer_NullCoalesce_NonNullableLeft_ReturnsLeft(t *testing.T) {
	c := ctx(t, richContextJSON)
	assertSchema(t, infer(t, "input.order_id ?? 0", c), `{"type":"integer"}`)
}

func TestInfer_NullCoalesce_NumericWidening(t *testing.T) {
	// nullable integer ?? number literal → number
	c := ctx(t, `{
		"type": "object",
		"properties": { "count": {"type": "integer"} },
		"required": []
	}`)
	assertSchema(t, infer(t, "count ?? 0.5", c), `{"type":"number"}`)
}

func TestInfer_NullCoalesce_DifferentTypes(t *testing.T) {
	// nullable integer ?? string → integer|string, canonicalized. The union is merged
	// rather than left as oneOf[{integer},{string}]: oneOf means EXACTLY one, so
	// un-merged arms that overlap describe a type no value satisfies.
	c := ctx(t, `{
		"type": "object",
		"properties": { "count": {"type": "integer"} },
		"required": []
	}`)
	assertSchema(t, infer(t, `count ?? "n/a"`, c), `{"type":["integer","string"]}`)
}

// --- Unsupported constructs ---

// Unsupported constructs are now rejected by the parser, so these assert on the
// message rather than on ErrUnsupported: the rejection happens earlier and names
// what is wrong instead of reporting an opaque node type.
func TestInfer_FunctionCall_Unsupported(t *testing.T) {
	inferErr(t, `len("hello")`, schema.Schema{}, "unknown function")
}

func TestInfer_InOperator_Unsupported(t *testing.T) {
	c := ctx(t, richContextJSON)
	inferErr(t, "1 in [1, 2, 3]", c, "unexpected")
}

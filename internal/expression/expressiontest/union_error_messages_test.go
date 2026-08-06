package expressiontest

import "testing"

// Schemas local to error-message tests. Each is a minimal single-field schema
// that isolates one type behaviour.
var (
	stringXSchema  = mustSchema(`{"properties": {"x": {"type": "string"}}, "required": ["x"]}`)
	booleanXSchema = mustSchema(`{"properties": {"x": {"type": "boolean"}}, "required": ["x"]}`)

	// Single-notation nullable schemas. Error messages are notation-independent,
	// so one variant is enough here.
	nullableIntegerAnyOf = mustSchema(`{
		"properties": {"x": {"anyOf": [{"type": "integer"}, {"type": "null"}]}},
		"required": ["x"]
	}`)
	nullableBooleanAnyOf = mustSchema(`{
		"properties": {"x": {"anyOf": [{"type": "boolean"}, {"type": "null"}]}},
		"required": ["x"]
	}`)

	// integerOrObjectAnyOf — incompatible union used in the ambiguous-operator tests.
	integerOrObjectAnyOf = mustSchema(`{
		"properties": {"x": {"anyOf": [{"type": "integer"}, {"type": "object"}]}},
		"required": ["x"]
	}`)

	// objectWithFieldSchema — x is an object that has field "n" but not "missing".
	objectWithFieldSchema = mustSchema(`{
		"properties": {
			"x": {
				"type": "object",
				"properties": {"n": {"type": "integer"}},
				"required": ["n"]
			}
		},
		"required": ["x"]
	}`)
)

// ---- arithmetic type mismatches ----
// InferType names the actual types, so the message is actionable without the schema; the
// evalErr call confirms the runtime fails on the same expression.

func TestInferError_AddIntegerToString(t *testing.T) {
	inferErr(t, "x + 1", stringXSchema, `numeric operands, got "string" and "integer"`)
	evalErr(t, "x + 1", map[string]any{"x": "hello"})
}

func TestInferError_AddStringToInteger(t *testing.T) {
	inferErr(t, `x + "hello"`, integerXSchema, `numeric operands, got "integer" and "string"`)
	evalErr(t, `x + "hello"`, map[string]any{"x": 5})
}

func TestInferError_MultiplyBooleanByNumber(t *testing.T) {
	inferErr(t, "x * 2", booleanXSchema, `numeric operands, got "boolean" and "integer"`)
	evalErr(t, "x * 2", map[string]any{"x": true})
}

func TestInferError_UnaryNegOnString(t *testing.T) {
	inferErr(t, "-x", stringXSchema, `numeric operand, got "string"`)
	evalErr(t, "-x", map[string]any{"x": "hello"})
}

// ---- ordering type mismatches ----
//
// expr-lang ordering operators only accept numbers. InferType must catch string
// and boolean operands before they reach the runtime.

func TestInferError_CompareStringLessThanInteger(t *testing.T) {
	inferErr(t, "x < 1", stringXSchema, `numeric operands, got "string" and "integer"`)
	evalErr(t, "x < 1", map[string]any{"x": "hello"})
}

func TestInferError_CompareBooleanGreaterThanInteger(t *testing.T) {
	inferErr(t, "x > 1", booleanXSchema, `numeric operands, got "boolean" and "integer"`)
	evalErr(t, "x > 1", map[string]any{"x": true})
}

// ---- logical type mismatches ----
//
// && and || only accept booleans. An integer or string operand is a static error.

func TestInferError_AndWithInteger(t *testing.T) {
	inferErr(t, "x && true", integerXSchema, `boolean operands, got "integer" and "boolean"`)
	evalErr(t, "x && true", map[string]any{"x": 5})
}

func TestInferError_OrWithString(t *testing.T) {
	inferErr(t, "x || false", stringXSchema, `boolean operands, got "string" and "boolean"`)
	evalErr(t, "x || false", map[string]any{"x": "hello"})
}

func TestInferError_NotOnInteger(t *testing.T) {
	inferErr(t, "!x", integerXSchema, `boolean operand, got "integer"`)
	evalErr(t, "!x", map[string]any{"x": 5})
}

// ---- null operands ----
//
// A nullable operand is caught before the operator even runs, producing a clear
// "non-nullable" message rather than a cryptic runtime panic.

func TestInferError_NullArithmetic(t *testing.T) {
	inferErr(t, "x + 1", nullableIntegerAnyOf, "non-nullable operands")
	evalErr(t, "x + 1", map[string]any{"x": nil})
}

func TestInferError_NullComparison(t *testing.T) {
	inferErr(t, "x < 1", nullableIntegerAnyOf, "non-nullable operands")
	evalErr(t, "x < 1", map[string]any{"x": nil})
}

func TestInferError_NullLogical(t *testing.T) {
	inferErr(t, "x && true", nullableBooleanAnyOf, "non-nullable boolean operands")
	evalErr(t, "x && true", map[string]any{"x": nil})
}

func TestInferError_NullUnaryNeg(t *testing.T) {
	inferErr(t, "-x", nullableIntegerAnyOf, "non-nullable numeric operand")
	evalErr(t, "-x", map[string]any{"x": nil})
}

func TestInferError_NullNot(t *testing.T) {
	inferErr(t, "!x", nullableBooleanAnyOf, "non-nullable boolean operand")
	evalErr(t, "!x", map[string]any{"x": nil})
}

// ---- ambiguous union ----
// A union including an incompatible variant is caught statically. The error says "unambiguous"
// rather than naming types, because any variant could be the runtime value.

func TestInferError_AmbiguousArithmetic(t *testing.T) {
	inferErr(t, "x + 1", integerOrObjectAnyOf, "unambiguous")
	evalErr(t, "x + 1", map[string]any{"x": map[string]any{"v": 1}})
}

func TestInferError_AmbiguousComparison(t *testing.T) {
	schema := mustSchema(`{
		"properties": {"x": {"anyOf": [{"type": "integer"}, {"type": "string"}]}},
		"required": ["x"]
	}`)
	inferErr(t, "x < 1", schema, "unambiguous")
	evalErr(t, "x < 1", map[string]any{"x": "hello"})
}

// ---- field access ----
//
// InferType catches missing or inaccessible fields at schema-resolution time.

func TestInferError_FieldOnPrimitive(t *testing.T) {
	schema := mustSchema(`{
		"properties": {"x": {"anyOf": [{"type": "object", "properties": {"foo": {"type": "string"}}}, {"type": "string"}]}},
		"required": ["x"]
	}`)
	// Inference succeeds: string variant contributes null via optional-chain semantics.
	infer(t, "x.foo", schema)
	// Runtime: non-object access returns nil (matches optional-chain semantics).
	got := evalOK(t, "x.foo", map[string]any{"x": 42})
	if got != nil {
		t.Errorf("Eval(x.foo) with x=42: expected nil, got %v", got)
	}
}

func TestInferError_UndeclaredVariable(t *testing.T) {
	inferErr(t, "y + 1", integerXSchema, `"y" not found in schema`)
	evalErr(t, "y + 1", map[string]any{"x": 5})
}

package expressiontest

import "testing"

// A null-check narrows x to non-null in the safe branch; the unsafe one is still rejected.
// Schemas are in helpers_test.go.

var (
	// memberPathSchema — x is a nullable integer nested under an object, used to
	// verify that guard lookup works for dot-paths like input.x, not just bare identifiers.
	memberPathSchema = mustSchema(`{
		"properties": {
			"input": {
				"type": "object",
				"properties": {
					"x": {"anyOf": [{"type": "integer"}, {"type": "null"}]}
				},
				"required": ["x"]
			}
		},
		"required": ["input"]
	}`)

	// tripleNumericNullSchema — x can be integer, number, or null. Without a null
	// guard the expression must be rejected; after narrowing it must be accepted.
	tripleNumericNullSchema = mustSchema(`{
		"properties": {
			"x": {
				"anyOf": [
					{"type": "integer"},
					{"type": "number"},
					{"type": "null"}
				]
			}
		},
		"required": ["x"]
	}`)
)

func TestConditionalNullNarrowing_EqNilElseGt(t *testing.T) {
	testNullableNarrowValid(t, "x == null ? false : x > 0", nullableInteger)
}

func TestConditionalNullNarrowing_EqNilElseAdd(t *testing.T) {
	testNullableNarrowValid(t, "x == null ? 0 : x + 1", nullableInteger)
}

func TestConditionalNullNarrowing_EqNilElseConcat(t *testing.T) {
	testNullableNarrowValid(t, `x == null ? "" : x + "!"`, nullableString)
}

func TestConditionalNullNarrowing_EqNilElseNot(t *testing.T) {
	testNullableNarrowValid(t, "x == null ? true : !x", nullableBoolean)
}

func TestConditionalNullNarrowing_NeNilThenGt(t *testing.T) {
	testNullableNarrowValid(t, "x != null ? x > 0 : false", nullableInteger)
}

func TestConditionalNullNarrowing_NeNilThenAdd(t *testing.T) {
	testNullableNarrowValid(t, "x != null ? x + 1 : 0", nullableInteger)
}

func TestConditionalNullNarrowing_NilEqElseGt(t *testing.T) {
	testNullableNarrowValid(t, "null == x ? false : x > 0", nullableInteger)
}

func TestConditionalNullNarrowing_NilNeThenGt(t *testing.T) {
	testNullableNarrowValid(t, "null != x ? x > 0 : false", nullableInteger)
}

func TestConditionalNullNarrowing_RejectsEqNilThenAdd(t *testing.T) {
	testNullableNarrowInvalid(t, "x == null ? x + 1 : 0", nullableInteger)
}

func TestConditionalNullNarrowing_RejectsNeNilElseGt(t *testing.T) {
	testNullableNarrowInvalid(t, "x != null ? false : x > 0", nullableInteger)
}

func TestConditionalNullNarrowing_MemberPath(t *testing.T) {
	infer(t, "input.x == null ? 0 : input.x + 1", memberPathSchema)
}

// stripNull on a schema without null returns it unchanged, so the else branch
// still has a concrete integer type — no false positive.
func TestConditionalNullNarrowing_NonNullableX(t *testing.T) {
	infer(t, "x == null ? 0 : x + 1", integerXSchema)
}

// A three-variant union (integer|number|null) must be rejected without a null
// guard and accepted once the null branch is excluded via a null-check.
func TestNullableContract_TripleVariantNumericNullable_Rejected(t *testing.T) {
	inferErr(t, "x + 1", tripleNumericNullSchema, "")
}

func TestConditionalNullNarrowing_TripleVariantNumericNullable(t *testing.T) {
	infer(t, "x != null ? x + 1 : 0", tripleNumericNullSchema)
}

// Narrowing result type — the inferred schema of a null-guarded ternary should
// be the concrete non-null type, not a nullable union.

func TestConditionalNullNarrowingResultType_EqNilBoolElseCmp(t *testing.T) {
	testNullableNarrowResultType(t, "x == null ? false : x > 0", nullableInteger, `{"type":"boolean"}`)
}

func TestConditionalNullNarrowingResultType_EqNilZeroElseX(t *testing.T) {
	testNullableNarrowResultType(t, "x == null ? 0 : x", nullableInteger, `{"type":"integer"}`)
}

func TestConditionalNullNarrowingResultType_NeNilXElseZero(t *testing.T) {
	testNullableNarrowResultType(t, "x != null ? x : 0", nullableInteger, `{"type":"integer"}`)
}

// When the else branch is a null literal, the result must be integer|null, not
// just integer — the then and else types are combined via nullableSchema.
func TestConditionalNullNarrowingResultType_NilElseBranch(t *testing.T) {
	testNullableNarrowResultType(t, "x != null ? x : null", nullableInteger, `{"type":["integer","null"]}`)
}

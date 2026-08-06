package schematest

import (
	"encoding/json"
	"genroc/internal/numeric"
	"reflect"
	"testing"

	"genroc/internal/schema"
)

// mustData decodes the way runtime data does (numbers kept as exact literals), so fixtures and
// expectations share production's representation — otherwise DeepEqual compares float64 against
// json.Number and reports a mismatch between two identical values.
func mustData(t *testing.T, s string) any {
	t.Helper()
	var v any
	if err := numeric.Decode([]byte(s), &v); err != nil {
		t.Fatalf("bad test JSON %q: %v", s, err)
	}
	return v
}

// validated parses a schema, runs data through Validate, and returns the
// normalized value, failing the test on a validation error.
func validated(t *testing.T, schemaJSON, dataJSON string) any {
	t.Helper()
	sc := mustParse(t, schemaJSON)
	norm, err := sc.Validate(mustData(t, dataJSON))
	if err != nil {
		t.Fatalf("Validate(%s) unexpected error: %v", dataJSON, err)
	}
	return norm
}

// assertNormalized checks that data normalizes to wantJSON under schemaJSON.
func assertNormalized(t *testing.T, schemaJSON, dataJSON, wantJSON string) {
	t.Helper()
	got := validated(t, schemaJSON, dataJSON)
	want := mustData(t, wantJSON)
	if !reflect.DeepEqual(got, want) {
		gb, _ := json.MarshalIndent(got, "", "  ")
		wb, _ := json.MarshalIndent(want, "", "  ")
		t.Errorf("normalized mismatch\ngot:\n%s\nwant:\n%s", gb, wb)
	}
}

// assertInvalid checks that data fails validation under schemaJSON.
func assertInvalid(t *testing.T, schemaJSON, dataJSON string) {
	t.Helper()
	sc := mustParse(t, schemaJSON)
	if _, err := sc.Validate(mustData(t, dataJSON)); err == nil {
		t.Errorf("Validate(%s) expected error, got none", dataJSON)
	}
}

func TestValidateStripsUndeclaredProperties(t *testing.T) {
	schemaJSON := `{"type":"object","properties":{"a":{"type":"string"}},"required":["a"]}`
	assertNormalized(t, schemaJSON, `{"a":"x","b":1,"c":true}`, `{"a":"x"}`)
}

func TestValidateFillsDefaults(t *testing.T) {
	schemaJSON := `{
		"type":"object",
		"properties":{
			"a":{"type":"string"},
			"b":{"type":"integer","default":7},
			"c":{"type":"string","default":"hi"}
		},
		"required":["a"]
	}`
	assertNormalized(t, schemaJSON, `{"a":"x"}`, `{"a":"x","b":7,"c":"hi"}`)
	// A present value overrides its own default; other defaults still fill.
	assertNormalized(t, schemaJSON, `{"a":"x","b":3}`, `{"a":"x","b":3,"c":"hi"}`)
}

func TestValidateOmitsAbsentOptionalWithoutDefault(t *testing.T) {
	schemaJSON := `{"type":"object","properties":{"a":{"type":"string"},"b":{"type":"string"}},"required":["a"]}`
	assertNormalized(t, schemaJSON, `{"a":"x"}`, `{"a":"x"}`)
}

func TestValidateRequiredMissing(t *testing.T) {
	schemaJSON := `{"type":"object","properties":{"a":{"type":"string"}},"required":["a"]}`
	assertInvalid(t, schemaJSON, `{}`)
}

func TestValidateNestedObjectsAndArrays(t *testing.T) {
	schemaJSON := `{
		"type":"object",
		"properties":{
			"user":{
				"type":"object",
				"properties":{"name":{"type":"string"},"role":{"type":"string","default":"member"}},
				"required":["name"]
			},
			"tags":{"type":"array","items":{"type":"object","properties":{"v":{"type":"string"}},"required":["v"]}}
		},
		"required":["user"]
	}`
	assertNormalized(t,
		schemaJSON,
		`{"user":{"name":"al","extra":1},"tags":[{"v":"a","junk":9},{"v":"b"}]}`,
		`{"user":{"name":"al","role":"member"},"tags":[{"v":"a"},{"v":"b"}]}`,
	)
}

func TestValidateIntegerAcceptsIntegralFloat(t *testing.T) {
	assertNormalized(t, `{"type":"integer"}`, `5`, `5`)
	assertInvalid(t, `{"type":"integer"}`, `5.5`)
	assertInvalid(t, `{"type":"integer"}`, `"5"`)
}

func TestValidateTypeMismatch(t *testing.T) {
	assertInvalid(t, `{"type":"string"}`, `1`)
	assertInvalid(t, `{"type":"object","properties":{"a":{"type":"string"}}}`, `[]`)
}

func TestValidateNullableType(t *testing.T) {
	schemaJSON := `{"type":"object","properties":{"a":{"type":["string","null"]}},"required":["a"]}`
	assertNormalized(t, schemaJSON, `{"a":null}`, `{"a":null}`)
	assertNormalized(t, schemaJSON, `{"a":"x"}`, `{"a":"x"}`)
	assertInvalid(t, schemaJSON, `{"a":1}`)
}

func TestValidateEnum(t *testing.T) {
	schemaJSON := `{"type":"string","enum":["red","green","blue"]}`
	assertNormalized(t, schemaJSON, `"green"`, `"green"`)
	assertInvalid(t, schemaJSON, `"yellow"`)
}

func TestValidateNumericRange(t *testing.T) {
	schemaJSON := `{"type":"integer","minimum":1,"maximum":10}`
	assertNormalized(t, schemaJSON, `5`, `5`)
	assertInvalid(t, schemaJSON, `0`)
	assertInvalid(t, schemaJSON, `11`)
}

func TestValidateStringLength(t *testing.T) {
	schemaJSON := `{"type":"string","minLength":2,"maxLength":4}`
	assertNormalized(t, schemaJSON, `"abc"`, `"abc"`)
	assertInvalid(t, schemaJSON, `"a"`)
	assertInvalid(t, schemaJSON, `"abcde"`)
}

func TestValidateArrayItemBounds(t *testing.T) {
	schemaJSON := `{"type":"array","items":{"type":"integer"},"minItems":1,"maxItems":2}`
	assertNormalized(t, schemaJSON, `[1,2]`, `[1,2]`)
	assertInvalid(t, schemaJSON, `[]`)
	assertInvalid(t, schemaJSON, `[1,2,3]`)
	assertInvalid(t, schemaJSON, `[1,"x"]`)
}

func TestValidateAnyOfFirstMatchWins(t *testing.T) {
	// Two object variants that both accept the data; anyOf takes the first, so
	// its property set governs which keys survive.
	schemaJSON := `{"anyOf":[
		{"type":"object","properties":{"a":{"type":"string"}}},
		{"type":"object","properties":{"a":{"type":"string"},"b":{"type":"string"}}}
	]}`
	assertNormalized(t, schemaJSON, `{"a":"x","b":"y"}`, `{"a":"x"}`)
}

func TestValidateOneOfExactlyOne(t *testing.T) {
	// Disjoint by type: string or integer.
	schemaJSON := `{"oneOf":[{"type":"string"},{"type":"integer"}]}`
	assertNormalized(t, schemaJSON, `"x"`, `"x"`)
	assertNormalized(t, schemaJSON, `3`, `3`)
	assertInvalid(t, schemaJSON, `true`)
}

func TestValidateRefResolvesAgainstDefs(t *testing.T) {
	schemaJSON := `{
		"type":"object",
		"properties":{"item":{"$ref":"#/$defs/Item"}},
		"required":["item"],
		"$defs":{"Item":{"type":"object","properties":{"id":{"type":"integer"}},"required":["id"]}}
	}`
	assertNormalized(t, schemaJSON, `{"item":{"id":1,"drop":true}}`, `{"item":{"id":1}}`)
	assertInvalid(t, schemaJSON, `{"item":{"id":"nope"}}`)
}

func TestValidateEmptySchemaPassesThrough(t *testing.T) {
	assertNormalized(t, `{}`, `{"anything":[1,2,3]}`, `{"anything":[1,2,3]}`)
}

func TestValidateZeroSchema(t *testing.T) {
	// The zero Schema (no underlying document) accepts anything unchanged.
	var zero schema.Schema
	got, err := zero.Validate(mustData(t, `{"a":1}`))
	if err != nil {
		t.Fatalf("zero schema: %v", err)
	}
	if !reflect.DeepEqual(got, mustData(t, `{"a":1}`)) {
		t.Errorf("zero schema should pass data through, got %v", got)
	}
}

func TestValidateDefaultIsCloned(t *testing.T) {
	// A mutable default must not be shared between two normalizations: mutating
	// one result's default array must not affect the next call's.
	schemaJSON := `{"type":"object","properties":{"xs":{"type":"array","items":{"type":"integer"},"default":[1,2]}}}`
	sc := mustParse(t, schemaJSON)

	first, err := sc.Validate(map[string]any{})
	if err != nil {
		t.Fatal(err)
	}
	firstXs := first.(map[string]any)["xs"].([]any)
	firstXs[0] = 99 // mutate the first result's filled default

	second, err := sc.Validate(map[string]any{})
	if err != nil {
		t.Fatal(err)
	}
	if got := second.(map[string]any)["xs"].([]any)[0]; !numeric.Equal(got, 1) {
		t.Errorf("default leaked between calls: second call sees %v, want 1", got)
	}
}

package schematest

import (
	"encoding/json"
	"testing"

	"genroc/internal/schema"
)

// Schema documents carry numbers too, in `default` and `enum` (both any-typed), which used to
// decode through float64 while runtime data did not. beyondFloat64 is 2^53+1; neighbour is the
// value it collapses to.
const (
	beyondFloat64 = "9007199254740993"
	neighbour     = "9007199254740992"
)

func conformValue(t *testing.T, schemaJSON string, data any) (any, error) {
	t.Helper()
	raw, err := schema.Parse([]byte(schemaJSON))
	if err != nil {
		t.Fatalf("parse schema: %v", err)
	}
	sc, err := raw.Normalize()
	if err != nil {
		t.Fatalf("normalize schema: %v", err)
	}
	return sc.Validate(data)
}

// assertFilledDefault conforms an empty object and checks the value filled for
// "v", compared as raw JSON so the assertion cannot itself round through float64.
func assertFilledDefault(t *testing.T, schemaJSON, wantLiteral string) {
	t.Helper()
	out, err := conformValue(t, schemaJSON, map[string]any{})
	if err != nil {
		t.Fatalf("validate: %v", err)
	}
	got, err := json.Marshal(out)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"v":` + wantLiteral + `}`
	if string(got) != want {
		t.Errorf("filled default = %s, want %s", got, want)
	}
}

// --- defaults ---

// A default past float64's exact range must fill as written. It used to fill as
// its float64 neighbour, so a schema silently rewrote the value it declared.
func TestDefaultBeyondFloat64FillsExactly(t *testing.T) {
	assertFilledDefault(t,
		`{"type":"object","properties":{"v":{"type":"integer","default":`+beyondFloat64+`}}}`,
		beyondFloat64)
}

func TestDefaultBeyondInt64FillsExactly(t *testing.T) {
	assertFilledDefault(t,
		`{"type":"object","properties":{"v":{"type":"integer","default":12345678901234567890}}}`,
		"12345678901234567890")
}

// A fractional default keeps its literal rather than a binary approximation.
func TestDefaultFractionFillsExactly(t *testing.T) {
	assertFilledDefault(t,
		`{"type":"object","properties":{"v":{"type":"number","default":0.1}}}`,
		"0.1")
}

func TestDefaultHighPrecisionFractionFillsExactly(t *testing.T) {
	assertFilledDefault(t,
		`{"type":"object","properties":{"v":{"type":"number","default":123456789.123456789}}}`,
		"123456789.123456789")
}

// Defaults are cloned before being filled (so two documents never alias one
// value). The clone marshals and re-decodes, which is a second place the literal
// could be lost — filling twice must produce the same exact value.
func TestDefaultSurvivesRepeatedFills(t *testing.T) {
	s := `{"type":"object","properties":{"v":{"type":"integer","default":` + beyondFloat64 + `}}}`
	assertFilledDefault(t, s, beyondFloat64)
	assertFilledDefault(t, s, beyondFloat64)
}

// A default nested inside an object default must be exact too — the nested fill
// runs through conform a second time.
func TestNestedDefaultFillsExactly(t *testing.T) {
	out, err := conformValue(t, `{
		"type":"object",
		"properties":{
			"outer":{
				"type":"object",
				"properties":{"inner":{"type":"integer","default":`+beyondFloat64+`}},
				"default":{}
			}
		}
	}`, map[string]any{})
	if err != nil {
		t.Fatalf("validate: %v", err)
	}
	got, _ := json.Marshal(out)
	want := `{"outer":{"inner":` + beyondFloat64 + `}}`
	if string(got) != want {
		t.Errorf("nested default = %s, want %s", got, want)
	}
}

// A supplied value still overrides its default, and keeps its own exactness.
func TestSuppliedValueOverridesDefaultExactly(t *testing.T) {
	out, err := conformValue(t,
		`{"type":"object","properties":{"v":{"type":"integer","default":1}}}`,
		map[string]any{"v": json.Number(beyondFloat64)})
	if err != nil {
		t.Fatalf("validate: %v", err)
	}
	got, _ := json.Marshal(out)
	if string(got) != `{"v":`+beyondFloat64+`}` {
		t.Errorf("supplied value = %s, want the exact literal", got)
	}
}

// --- enum ---
// An enum is a whitelist, so a rounded entry does not merely lose precision — it permits the
// wrong value: a whitelist declared for …993 rejected …993 and accepted …992.

func TestEnumAcceptsItsOwnDeclaredValue(t *testing.T) {
	_, err := conformValue(t,
		`{"type":"object","properties":{"v":{"type":"integer","enum":[`+beyondFloat64+`]}}}`,
		map[string]any{"v": json.Number(beyondFloat64)})
	if err != nil {
		t.Errorf("enum rejected the value it declares: %v", err)
	}
}

func TestEnumRejectsFloat64Neighbour(t *testing.T) {
	_, err := conformValue(t,
		`{"type":"object","properties":{"v":{"type":"integer","enum":[`+beyondFloat64+`]}}}`,
		map[string]any{"v": json.Number(neighbour)})
	if err == nil {
		t.Error("enum admitted a value it does not declare (the float64 neighbour)")
	}
}

// Matching stays by value, not by literal: an enum written 1 must still accept an
// input that arrives as 1.0, which is how it behaved before exact literals and
// what a byte comparison would have broken.
func TestEnumMatchesAcrossEquivalentLiterals(t *testing.T) {
	for _, in := range []string{"1", "1.0", "1.000"} {
		t.Run(in, func(t *testing.T) {
			_, err := conformValue(t,
				`{"type":"object","properties":{"v":{"type":"integer","enum":[1]}}}`,
				map[string]any{"v": json.Number(in)})
			if err != nil {
				t.Errorf("enum [1] rejected input %s: %v", in, err)
			}
		})
	}
}

// --- bounds ---

// minimum/maximum are declared as float64 in the schema struct, so a bound is
// only as precise as float64 allows. What must hold is that a value is compared
// exactly against it rather than being rounded first.
func TestBoundsCompareValueExactly(t *testing.T) {
	s := `{"type":"object","properties":{"v":{"type":"number","minimum":1,"maximum":10}}}`
	for _, c := range []struct {
		name  string
		in    string
		valid bool
	}{
		{"just_above_minimum", "1.0000000000000000001", true},
		{"just_below_minimum", "0.9999999999999999999", false},
		{"just_below_maximum", "9.9999999999999999999", true},
		{"just_above_maximum", "10.000000000000000001", false},
	} {
		t.Run(c.name, func(t *testing.T) {
			_, err := conformValue(t, s, map[string]any{"v": json.Number(c.in)})
			if c.valid && err != nil {
				t.Errorf("%s should be in range: %v", c.in, err)
			}
			if !c.valid && err == nil {
				t.Errorf("%s should be out of range", c.in)
			}
		})
	}
}

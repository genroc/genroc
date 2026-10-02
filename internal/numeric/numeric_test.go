package numeric

import (
	"encoding/json"
	"strings"
	"testing"
)

// --- Decode ---

// exactLiteralsJSON holds values plain json.Unmarshal cannot round trip: an
// integer past float64's exact range, one past int64's, and two fractions whose
// binary expansion is not the literal.
const exactLiteralsJSON = `{"id":9007199254740993,"big":12345678901234567890,"amt":0.1,"neg":-0.000001}`

// Decode is the boundary that makes everything else worthwhile: plain
// json.Unmarshal corrupts these values before any expression runs. This test
// pins that premise — if it ever stops holding, Decode has nothing left to do.
func TestDecodePremise_PlainUnmarshalLosesPrecision(t *testing.T) {
	raw := []byte(exactLiteralsJSON)
	var lossy map[string]any
	if err := json.Unmarshal(raw, &lossy); err != nil {
		t.Fatal(err)
	}
	lossyOut, _ := json.Marshal(lossy)
	if string(lossyOut) == string(raw) {
		t.Fatal("expected plain json.Unmarshal to lose precision; if it no longer does, this test is obsolete")
	}
}

// Decode must round trip each literal byte for byte — comparing decoded Go
// values would compare whatever the lossy conversion produced, so the assertion
// is on the re-marshalled raw text.
func TestDecodePreservesExactLiterals(t *testing.T) {
	raw := []byte(exactLiteralsJSON)
	var exact map[string]any
	if err := Decode(raw, &exact); err != nil {
		t.Fatal(err)
	}
	out, err := json.Marshal(exact)
	if err != nil {
		t.Fatal(err)
	}
	// Marshal orders object keys alphabetically, so compare field by field.
	for _, c := range []struct{ name, literal string }{
		{"beyond_float64_integer", "9007199254740993"},
		{"beyond_int64_integer", "12345678901234567890"},
		{"decimal_fraction", "0.1"},
		{"small_negative_fraction", "-0.000001"},
	} {
		t.Run(c.name, func(t *testing.T) {
			if !strings.Contains(string(out), c.literal) {
				t.Errorf("round trip lost %s\n got: %s\nfrom: %s", c.literal, out, raw)
			}
		})
	}
}

// --- ToDecimal: every representation a value can arrive in at runtime ---

func TestToDecimal_JSONNumberFraction(t *testing.T) {
	assertToDecimal(t, json.Number("1.5"), "1.5")
}

func TestToDecimal_JSONNumberBeyondFloat64(t *testing.T) {
	assertToDecimal(t, json.Number("9007199254740993"), "9007199254740993")
}

func TestToDecimal_Int(t *testing.T) {
	assertToDecimal(t, int(7), "7")
}

func TestToDecimal_Int64(t *testing.T) {
	assertToDecimal(t, int64(-3), "-3")
}

func TestToDecimal_Int32(t *testing.T) {
	assertToDecimal(t, int32(4), "4")
}

// A float64 converts through its shortest round-tripping text, so 0.1 becomes
// decimal 0.1 rather than its binary expansion.
func TestToDecimal_Float64UsesShortestRoundTrip(t *testing.T) {
	assertToDecimal(t, float64(0.1), "0.1")
}

func TestToDecimal_Float32(t *testing.T) {
	assertToDecimal(t, float32(2.5), "2.5")
}

func TestToDecimal_RejectsNonNumeric(t *testing.T) {
	for _, c := range []struct {
		name string
		in   any
	}{
		{"numeric_string", "1"},
		{"bool", true},
		{"nil", nil},
		{"array", []any{1}},
		{"object", map[string]any{}},
	} {
		t.Run(c.name, func(t *testing.T) { assertNotNumeric(t, c.in) })
	}
}

// --- Equal ---
// By value, not literal: an enum declared [1] must accept "1.0".

func TestEqual_JSONNumberAndFloat64(t *testing.T) {
	assertEqual(t, json.Number("1"), float64(1))
}

func TestEqual_TrailingZeroFraction(t *testing.T) {
	assertEqual(t, json.Number("1.0"), json.Number("1"))
}

func TestEqual_TrailingZerosAndInt(t *testing.T) {
	assertEqual(t, json.Number("1.000"), int(1))
}

func TestEqual_Int64AndWholeFloat64(t *testing.T) {
	assertEqual(t, int64(2), float64(2.0))
}

func TestEqual_FractionAcrossTypes(t *testing.T) {
	assertEqual(t, json.Number("0.1"), float64(0.1))
}

func TestEqual_DifferentValues(t *testing.T) {
	assertNotEqual(t, json.Number("1"), float64(1.5))
}

func TestEqual_AdjacentBigIntegers(t *testing.T) {
	assertNotEqual(t, json.Number("9007199254740993"), json.Number("9007199254740992"))
}

func TestEqual_NumberAndString(t *testing.T) {
	assertNotEqual(t, int(1), "1")
}

func TestEqual_NumberAndNil(t *testing.T) {
	assertNotEqual(t, int(1), nil)
}

// The big-integer pair in TestEqual_AdjacentBigIntegers is the point: as float64
// both sides collapse to the same value and would compare equal.
func TestEqualDistinguishesBeyondFloat64Precision(t *testing.T) {
	a, b := json.Number("9007199254740993"), json.Number("9007199254740992")
	af, _ := a.Float64()
	bf, _ := b.Float64()
	if af != bf {
		t.Skip("float64 can represent these distinctly; the test premise no longer holds")
	}
	if Equal(a, b) {
		t.Error("two distinct integers compared equal; the comparison went through float64")
	}
}

// --- IsIntegral ---

func TestIsIntegral(t *testing.T) {
	for _, c := range []struct {
		name string
		in   any
	}{
		{"int", int(3)},
		{"negative_int64", int64(-4)},
		{"json_integer", json.Number("5")},
		{"json_integer_with_zero_fraction", json.Number("5.0")},
		{"whole_float64", float64(2.0)},
		{"json_integer_beyond_float64", json.Number("9007199254740993")},
	} {
		t.Run(c.name, func(t *testing.T) { assertIntegral(t, c.in) })
	}
}

func TestIsIntegral_NonIntegral(t *testing.T) {
	for _, c := range []struct {
		name string
		in   any
	}{
		{"json_fraction", json.Number("5.5")},
		{"float64_fraction", float64(0.1)},
		{"string", "3"},
		{"nil", nil},
		{"bool", true},
	} {
		t.Run(c.name, func(t *testing.T) { assertNotIntegral(t, c.in) })
	}
}

// --- Format ---

func TestFormat_TrimsTrailingZeros(t *testing.T) {
	assertFormat(t, "2.000000000000000000000000000000000", "2")
}

func TestFormat_KeepsSignificantFraction(t *testing.T) {
	assertFormat(t, "0.3", "0.3")
}

func TestFormat_ExpandsExponentNotation(t *testing.T) {
	assertFormat(t, "1E+3", "1000")
}

func TestFormat_Negative(t *testing.T) {
	assertFormat(t, "-0.5", "-0.5")
}

func TestFormat_IntegerBeyondFloat64(t *testing.T) {
	assertFormat(t, "9007199254740993", "9007199254740993")
}

// Infinities and NaN have no JSON form and must be refused rather than emitted
// as a bare word that would corrupt the document.

func TestFormat_RefusesInfinity(t *testing.T) {
	assertFormatRefused(t, "Infinity")
}

func TestFormat_RefusesNegativeInfinity(t *testing.T) {
	assertFormatRefused(t, "-Infinity")
}

func TestFormat_RefusesNaN(t *testing.T) {
	assertFormatRefused(t, "NaN")
}

// Format must not mutate its argument — callers hold the decimal afterwards.
func TestFormatDoesNotMutate(t *testing.T) {
	d := mustDecimal(t, "2.500")
	before := d.Text('f')
	if _, ok := Format(d); !ok {
		t.Fatal("Format failed")
	}
	if after := d.Text('f'); after != before {
		t.Errorf("Format mutated its argument: %s -> %s", before, after)
	}
}

package main

import (
	"encoding/json"
	"testing"
)

// `--set k=v` does not go through the YAML walker (internal/defdoc): each value is coerced by
// inferScalar, which converted through int64 then float64. A value past int64 therefore fell
// to ParseFloat and was rounded — `--set id=<54 digits>` reached the server as
// 1.2374829758395876e+53.

// bigLiteral is 54 digits: past int64, so a float64 round-trip loses it.
const bigLiteral = "123748297583958759399485776859493938587768583992939858"

func assertSetScalar(t *testing.T, in string, want any) {
	t.Helper()
	if got := inferScalar(in); got != want {
		t.Errorf("inferScalar(%q) = %#v (%T), want %#v (%T)", in, got, got, want, want)
	}
}

func TestSetScalarPreservesLargeInteger(t *testing.T) {
	assertSetScalar(t, bigLiteral, json.Number(bigLiteral))
}

func TestSetScalarPreservesIntegerBeyondFloat64(t *testing.T) {
	assertSetScalar(t, "9007199254740993", json.Number("9007199254740993"))
}

func TestSetScalarPreservesHighPrecisionFraction(t *testing.T) {
	assertSetScalar(t, "123456789.123456789", json.Number("123456789.123456789"))
}

func TestSetScalarOrdinaryNumbers(t *testing.T) {
	assertSetScalar(t, "1", json.Number("1"))
	assertSetScalar(t, "-2.5", json.Number("-2.5"))
}

// Non-numeric words must still come through as strings, not as an unparsable
// json.Number.
func TestSetScalarNonNumeric(t *testing.T) {
	assertSetScalar(t, "true", true)
	assertSetScalar(t, "false", false)
	assertSetScalar(t, "null", nil)
	assertSetScalar(t, "hello", "hello")
	assertSetScalar(t, "1.2.3", "1.2.3")
	assertSetScalar(t, "", "")
	// A JSON document that is not a number is not a number.
	assertSetScalar(t, "[1,2]", "[1,2]")
}

// A --set value must marshal to the literal the user typed.
func TestSetScalarMarshalsToLiteral(t *testing.T) {
	b, err := json.Marshal(map[string]any{"id": inferScalar(bigLiteral)})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if string(b) != `{"id":`+bigLiteral+`}` {
		t.Errorf("got %s, want {\"id\":%s}", b, bigLiteral)
	}
}

package model

import (
	"encoding/json"
	"reflect"
	"testing"
)

func TestShape_UnmarshalJSON(t *testing.T) {
	cases := []struct {
		name    string
		in      string
		wantErr bool
	}{
		{"single expression", `"$: self.result"`, false},
		{"plain string literal", `"hello"`, false},
		{"nested object", `{"data": {"flag": "$: self.result.charged"}}`, false},
		{"empty object", `{}`, false},
		// The widened grammar accepts every JSON value structurally; type-checking is
		// deferred to inference, so none of these are rejected at unmarshal.
		{"bare number", `5`, false},
		{"bare bool", `true`, false},
		{"bare null", `null`, false},
		{"array", `["$: a", "$: b"]`, false},
		{"mixed array", `[1, true, "$: a", null]`, false},
		{"nested number", `{"n": 5}`, false},
		{"nested array", `{"tags": ["$: a"]}`, false},
		{"null leaf", `{"x": null}`, false},
		{"deeply nested", `[{"a": [1, "$: b"]}]`, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var s Shape
			err := json.Unmarshal([]byte(c.in), &s)
			if c.wantErr != (err != nil) {
				t.Fatalf("Unmarshal(%s): err=%v, wantErr=%v", c.in, err, c.wantErr)
			}
		})
	}
}

func TestShape_RoundTrip(t *testing.T) {
	in := `{"data":{"flag":"$: self.result.charged"},"id":"$: self.result.id"}`
	var s Shape
	if err := json.Unmarshal([]byte(in), &s); err != nil {
		t.Fatal(err)
	}
	out, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	// Compare structurally (object key order is not stable).
	var a, b any
	json.Unmarshal([]byte(in), &a)
	json.Unmarshal(out, &b)
	if !reflect.DeepEqual(a, b) {
		t.Errorf("round-trip mismatch:\n in:  %s\n out: %s", in, out)
	}
}

func TestShape_Present(t *testing.T) {
	var nilShape *Shape
	if nilShape.Present() {
		t.Error("nil shape should not be Present")
	}

	var s Shape
	if err := json.Unmarshal([]byte(`{"a":"$: x","b":{"c":"$: y"}}`), &s); err != nil {
		t.Fatal(err)
	}
	if !s.Present() {
		t.Error("shape should be Present")
	}
}

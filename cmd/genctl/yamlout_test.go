package main

import (
	"strings"
	"testing"

	"genroc/internal/numeric"
)

// The text view renders a payload as YAML, and YAML has two ways to lie about a value it
// was handed: quote a number, or leave a string looking like one bare. Both round-trip as a
// DIFFERENT value, and nothing downstream would notice.
func TestYamlBlock_RendersEachTypeAsItself(t *testing.T) {
	for _, tc := range []struct{ name, in, want string }{
		{"a large integer keeps every digit and stays a number",
			`{"v":12345678901234567890123}`, "v: 12345678901234567890123"},
		{"a trailing zero is not normalised away",
			`{"v":1.10}`, "v: 1.10"},
		{"exponent notation survives",
			`{"v":1e400}`, "v: 1e400"},
		{"a string of digits stays a string",
			`{"v":"123"}`, `v: "123"`},
		{"a key YAML would read as a boolean is quoted",
			`{"n":1}`, `"n": 1`},
		{"an empty object is not null",
			`{"v":{}}`, "v: {}"},
		{"an empty array is not null",
			`{"v":[]}`, "v: []"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var v any
			if err := numeric.Decode([]byte(tc.in), &v); err != nil {
				t.Fatalf("decode %s: %v", tc.in, err)
			}
			if got := yamlBlock(v); !strings.Contains(got, tc.want) {
				t.Errorf("%s rendered as %q, which does not contain %q — the value changed on the way to the screen", tc.in, got, tc.want)
			}
		})
	}
}

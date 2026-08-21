package main

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// bigLiteral is 54 digits: past int64, so yaml.v3 decodes it as a float64 and
// tags it !!float. Decoding straight into an `any` produced
// 1.2374829758395876e+53, which the CLI then uploaded — corrupting the value
// before the server ever saw it.
const bigLiteral = "123748297583958759399485776859493938587768583992939858"

func convert(t *testing.T, src string) any {
	t.Helper()
	var node yaml.Node
	if err := yaml.Unmarshal([]byte(src), &node); err != nil {
		t.Fatalf("yaml: %v", err)
	}
	got, err := yamlToAny(&node)
	if err != nil {
		t.Fatalf("yamlToAny: %v", err)
	}
	return got
}

// assertRoundTrip checks the JSON the CLI would send for src.
func assertRoundTrip(t *testing.T, src, wantJSON string) {
	t.Helper()
	b, err := json.Marshal(convert(t, src))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if string(b) != wantJSON {
		t.Errorf("yaml %q\n got: %s\nwant: %s", src, b, wantJSON)
	}
}

// The premise: plain yaml decoding loses it. If this ever stops holding,
// yamlToAny has nothing left to do.
func TestYAMLPremise_PlainDecodeLosesLargeInteger(t *testing.T) {
	var doc any
	if err := yaml.Unmarshal([]byte("v: "+bigLiteral+"\n"), &doc); err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(doc)
	if string(b) == `{"v":`+bigLiteral+`}` {
		t.Fatal("plain yaml decoding no longer loses large integers; this test is obsolete")
	}
}

func TestYAMLPreservesLargeInteger(t *testing.T) {
	assertRoundTrip(t, "v: "+bigLiteral+"\n", `{"v":`+bigLiteral+`}`)
}

func TestYAMLPreservesLargeIntegerInSequence(t *testing.T) {
	assertRoundTrip(t, "v: ["+bigLiteral+"]\n", `{"v":[`+bigLiteral+`]}`)
}

// The reported case: a schema default nested two levels down.
func TestYAMLPreservesLargeIntegerInSchemaDefault(t *testing.T) {
	src := "properties:\n  data:\n    type: array\n    default: [" + bigLiteral + "]\n"
	assertRoundTrip(t, src, `{"properties":{"data":{"default":[`+bigLiteral+`],"type":"array"}}}`)
}

func TestYAMLPreservesHighPrecisionFraction(t *testing.T) {
	assertRoundTrip(t, "v: 123456789.123456789\n", `{"v":123456789.123456789}`)
}

func TestYAMLPreservesIntegerBeyondFloat64(t *testing.T) {
	assertRoundTrip(t, "v: 9007199254740993\n", `{"v":9007199254740993}`)
}

// Ordinary values must be untouched.
func TestYAMLOrdinaryScalars(t *testing.T) {
	assertRoundTrip(t, "a: 1\nb: 1.5\nc: hello\nd: true\ne: null\n",
		`{"a":1,"b":1.5,"c":"hello","d":true,"e":null}`)
}

// YAML spellings JSON cannot express fall back to yaml's own decoding rather
// than producing a json.Number that would fail to marshal.
func TestYAMLNonJSONNumericSpellingsFallBack(t *testing.T) {
	for _, c := range []struct{ name, src string }{
		{"hex", "v: 0x1F\n"},
		{"octal", "v: 0o17\n"},
		{"leading_zero", "v: 007\n"},
	} {
		t.Run(c.name, func(t *testing.T) {
			b, err := json.Marshal(convert(t, c.src))
			if err != nil {
				t.Fatalf("value must stay marshalable: %v", err)
			}
			if !json.Valid(b) {
				t.Errorf("produced invalid JSON: %s", b)
			}
		})
	}
}

func TestYAMLNestedStructuresPreserved(t *testing.T) {
	src := "outer:\n  - inner:\n      v: " + bigLiteral + "\n"
	assertRoundTrip(t, src, `{"outer":[{"inner":{"v":`+bigLiteral+`}}]}`)
}

// --- --set scalar coercion ---
//
// `--set k=v` does not go through the YAML walker: each value is coerced by
// inferScalar, which converted through int64 then float64. A value past int64
// therefore fell to ParseFloat and was rounded — `--set id=<54 digits>` reached
// the server as 1.2374829758395876e+53.

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

// ── merge keys ──────────────────────────────────────────────────────────────────

// yamlToAny walks mappings itself, so YAML's `<<` is not free: without handling it the
// alias landed under a literal "<<" field that the server drops as unknown, and a
// definition sharing task shape by anchor silently lost every merged key.

func TestYamlToAny_MergeKeyFoldsTheAliasedMapIn(t *testing.T) {
	got := convert(t, "defaults: &d\n  method: GET\n  timeout: 15\ntask:\n  <<: *d\n")
	task := got.(map[string]any)["task"].(map[string]any)
	if _, leaked := task["<<"]; leaked {
		t.Fatalf("`<<` reached the server as a field: %#v", task)
	}
	if task["method"] != "GET" {
		t.Errorf("merged key missing: task = %#v", task)
	}
}

func TestYamlToAny_AnExplicitKeyBeatsAMergedOne(t *testing.T) {
	// YAML's own precedence. Getting it backwards would make every override a no-op while
	// the definition still applies, which is the failure anchors are reached for.
	got := convert(t, "defaults: &d\n  timeout: 15\ntask:\n  <<: *d\n  timeout: 30\n")
	task := got.(map[string]any)["task"].(map[string]any)
	if fmt.Sprint(task["timeout"]) != "30" {
		t.Errorf("the merged value won: timeout = %#v", task["timeout"])
	}
}

func TestYamlToAny_MergeAboveTheKeyItOverridesStillLoses(t *testing.T) {
	// Order within the mapping must not decide it: merges are applied after the whole
	// mapping is read, so the explicit key wins wherever it sits.
	got := convert(t, "defaults: &d\n  timeout: 15\ntask:\n  timeout: 30\n  <<: *d\n")
	task := got.(map[string]any)["task"].(map[string]any)
	if fmt.Sprint(task["timeout"]) != "30" {
		t.Errorf("position decided precedence: timeout = %#v", task["timeout"])
	}
}

func TestYamlToAny_MergeKeepsNumericLiteralsExact(t *testing.T) {
	// The merged branch must route through yamlToAny, not Decode: a big id arriving via an
	// anchor would otherwise be floated, which is the whole point of this file.
	got := convert(t, "d: &d\n  id: "+bigLiteral+"\ntask:\n  <<: *d\n")
	task := got.(map[string]any)["task"].(map[string]any)
	if fmt.Sprint(task["id"]) != bigLiteral {
		t.Errorf("merged literal corrupted: got %v", task["id"])
	}
}

func TestYamlToAny_ASequenceMergeIsRefusedRatherThanSilentlyReversed(t *testing.T) {
	// YAML 1.1 gives EARLIER entries precedence, so `[*base, *override]` does the opposite
	// of what it reads as. Refusing is the only answer that cannot mislead.
	var node yaml.Node
	src := "a: &a\n  k: 1\nb: &b\n  k: 2\ntask:\n  <<: [*a, *b]\n"
	if err := yaml.Unmarshal([]byte(src), &node); err != nil {
		t.Fatalf("yaml: %v", err)
	}
	_, err := yamlToAny(&node)
	if err == nil {
		t.Fatal("a sequence merge was accepted; its precedence reads backwards")
	}
	if !strings.Contains(err.Error(), "EARLIER") {
		t.Errorf("the error does not say why it is refused: %v", err)
	}
}

func TestYamlToAny_ANonMappingMergeIsRefused(t *testing.T) {
	var node yaml.Node
	if err := yaml.Unmarshal([]byte("task:\n  <<: nope\n"), &node); err != nil {
		t.Fatalf("yaml: %v", err)
	}
	if _, err := yamlToAny(&node); err == nil {
		t.Fatal("`<<: nope` was accepted")
	}
}

package defdoc

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// bigLiteral is 54 digits: past int64, so yaml.v3 decodes it as a float64 and tags it
// !!float. Decoding straight into an `any` produced 1.2374829758395876e+53, which the CLI
// then uploaded -- corrupting the value before the server ever saw it.
const bigLiteral = "123748297583958759399485776859493938587768583992939858"

func convert(t *testing.T, src string) any {
	t.Helper()
	return parse(t, src).Value
}

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

// The premise: plain yaml decoding loses it. If this ever stops holding, the scalar branch
// has nothing left to do.
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

func TestYAMLOrdinaryScalars(t *testing.T) {
	assertRoundTrip(t, "a: 1\nb: 1.5\nc: hello\nd: true\ne: null\n",
		`{"a":1,"b":1.5,"c":"hello","d":true,"e":null}`)
}

// YAML spellings JSON cannot express fall back to yaml's own decoding rather than producing a
// json.Number that would fail to marshal.
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

// The mapping walk is our own, so YAML's `<<` is not free: without handling it the alias
// landed under a literal "<<" field that the server drops as unknown, and a definition
// sharing task shape by anchor silently lost every merged key.

func TestMergeKeyFoldsTheAliasedMapIn(t *testing.T) {
	got := convert(t, "defaults: &d\n  method: GET\n  timeout: 15\ntask:\n  <<: *d\n")
	task := got.(map[string]any)["task"].(map[string]any)
	if _, leaked := task["<<"]; leaked {
		t.Fatalf("`<<` reached the server as a field: %#v", task)
	}
	if task["method"] != "GET" {
		t.Errorf("merged key missing: task = %#v", task)
	}
}

func TestAnExplicitKeyBeatsAMergedOne(t *testing.T) {
	// YAML's own precedence. Getting it backwards would make every override a no-op while
	// the definition still applies, which is the failure anchors are reached for.
	got := convert(t, "defaults: &d\n  timeout: 15\ntask:\n  <<: *d\n  timeout: 30\n")
	task := got.(map[string]any)["task"].(map[string]any)
	if fmt.Sprint(task["timeout"]) != "30" {
		t.Errorf("the merged value won: timeout = %#v", task["timeout"])
	}
}

func TestMergeAboveTheKeyItOverridesStillLoses(t *testing.T) {
	// Order within the mapping must not decide it: merges are applied after the whole
	// mapping is read, so the explicit key wins wherever it sits.
	got := convert(t, "defaults: &d\n  timeout: 15\ntask:\n  timeout: 30\n  <<: *d\n")
	task := got.(map[string]any)["task"].(map[string]any)
	if fmt.Sprint(task["timeout"]) != "30" {
		t.Errorf("position decided precedence: timeout = %#v", task["timeout"])
	}
}

func TestMergeKeepsNumericLiteralsExact(t *testing.T) {
	// The merged branch must route through the same scalar rule, not Decode: a big id
	// arriving via an anchor would otherwise be floated.
	got := convert(t, "d: &d\n  id: "+bigLiteral+"\ntask:\n  <<: *d\n")
	task := got.(map[string]any)["task"].(map[string]any)
	if fmt.Sprint(task["id"]) != bigLiteral {
		t.Errorf("merged literal corrupted: got %v", task["id"])
	}
}

func TestANonMappingMergeIsRefused(t *testing.T) {
	if _, err := Parse([]byte("task:\n  <<: nope\n")); err == nil {
		t.Fatal("`<<: nope` was accepted")
	}
}

func TestAliasedScalarsResolve(t *testing.T) {
	got := convert(t, "a: &v hello\nb: *v\n")
	if got.(map[string]any)["b"] != "hello" {
		t.Errorf("alias did not resolve: %#v", got)
	}
}

func TestObjectKeyMustBeAScalar(t *testing.T) {
	_, err := Parse([]byte("? [a, b]\n: v\n"))
	if err == nil || !strings.Contains(err.Error(), "must be a scalar") {
		t.Fatalf("a complex key has no path spelling and must be refused, got %v", err)
	}
}

package expressiontest

import "testing"

// A $ref to a secret definition, reached through a position that wraps its result
// nullable — a map value, an array element, an optional property. The wrap makes
// the value present as oneOf[{$ref}, null], and a secret check that reads only the
// wrapper's own $ref field finds nothing to follow. The value then goes out
// unredacted, which is why every spelling below is asserted rather than a
// representative one.
const refSecretJSON = `{
	"type": "object",
	"properties": {
		"refMap":   { "type": "object", "additionalProperties": { "$ref": "#/$defs/Cred" } },
		"refList":  { "type": "array",  "items":                { "$ref": "#/$defs/Cred" } },
		"declared": { "type": "object", "properties": { "cred": { "$ref": "#/$defs/Cred" } }, "required": ["cred"] },
		"optional": { "type": "object", "properties": { "cred": { "$ref": "#/$defs/Cred" } } },
		"plainMap": { "type": "object", "additionalProperties": { "$ref": "#/$defs/Plain" } },
		"k": { "type": "string" },
		"i": { "type": "integer" }
	},
	"required": ["refMap","refList","declared","optional","plainMap","k","i"],
	"$defs": {
		"Cred":  { "type": "string", "secret": true },
		"Plain": { "type": "string" }
	}
}`

func TestSecret_RefThroughNullableWrapper(t *testing.T) {
	c := ctx(t, refSecretJSON)
	secretRefAll(t, c, true, []secretCase{
		{"declared_property", `declared.cred`},
		{"optional_property", `optional.cred`},
		{"map_value_static_key", `refMap.literal`},
		{"map_value_quoted_key", `refMap["some-key"]`},
		{"map_value_computed_key", `refMap[k]`},
		{"array_element_literal_index", `refList[0]`},
		{"array_element_computed_index", `refList[i]`},
		{"tainted_value_transformed", `refList[i] ?? "none"`},
		{"inside_a_lambda", `map(refList, c => c)`},
	})
	// The control: an identically shaped map whose definition is not secret.
	secretRefAll(t, c, false, []secretCase{
		{"plain_definition_static", `plainMap.literal`},
		{"plain_definition_computed", `plainMap[k]`},
	})
}

// Redaction is the half that actually withholds the value, so it must agree.
func TestSecret_RefThroughNullableWrapperRedacts(t *testing.T) {
	c := ctx(t, refSecretJSON)
	data := map[string]any{
		"refMap":   map[string]any{"a": "sk-map"},
		"refList":  []any{"sk-list"},
		"declared": map[string]any{"cred": "sk-declared"},
		"optional": map[string]any{"cred": "sk-optional"},
		"plainMap": map[string]any{"a": "not-secret"},
		"k":        "a",
		"i":        1,
	}
	got := c.CollectSecrets(data)
	for _, want := range []string{"sk-map", "sk-list", "sk-declared", "sk-optional"} {
		if !contains(got, want) {
			t.Errorf("CollectSecrets did not gather %q (got %v)", want, got)
		}
	}
	if contains(got, "not-secret") {
		t.Errorf("CollectSecrets gathered a non-secret value (got %v)", got)
	}
}

func contains(hay []string, needle string) bool {
	for _, h := range hay {
		if h == needle {
			return true
		}
	}
	return false
}

package schematest

import (
	"reflect"
	"testing"
)

// The "anywhere" guarantees: defaults fill and undeclared properties are omitted at any depth
// (nested objects, array elements, matched union branches, behind $refs), and secrecy is
// detected along any path — a secret ancestor, array indices, nullable-wrapped optionals.

func TestValidateDefaultBehindRef(t *testing.T) {
	// The property is a lone $ref; its default lives on the *target* definition.
	schemaJSON := `{
		"type":"object",
		"properties":{"retry":{"$ref":"#/$defs/Retry"}},
		"$defs":{"Retry":{"type":"object","properties":{"count":{"type":"integer"}},"default":{"count":3}}}
	}`
	assertNormalized(t, schemaJSON, `{}`, `{"retry":{"count":3}}`)
	// A present value wins over the ref target's default (and is pruned normally).
	assertNormalized(t, schemaJSON, `{"retry":{"count":7,"junk":1}}`, `{"retry":{"count":7}}`)
}

func TestValidateDefaultInArrayElements(t *testing.T) {
	schemaJSON := `{
		"type":"array",
		"items":{"type":"object","properties":{"v":{"type":"integer"},"mode":{"type":"string","default":"auto"}},"required":["v"]}
	}`
	assertNormalized(t, schemaJSON,
		`[{"v":1},{"v":2,"mode":"manual"}]`,
		`[{"v":1,"mode":"auto"},{"v":2,"mode":"manual"}]`)
}

func TestValidateDefaultInUnionBranch(t *testing.T) {
	// The matched anyOf/oneOf branch fills its defaults like any other object.
	anyOfSchema := `{"anyOf":[
		{"type":"object","properties":{"kind":{"type":"string","enum":["a"]},"n":{"type":"integer","default":1}},"required":["kind"]},
		{"type":"string"}
	]}`
	assertNormalized(t, anyOfSchema, `{"kind":"a"}`, `{"kind":"a","n":1}`)

	oneOfSchema := `{"oneOf":[
		{"type":"object","properties":{"kind":{"type":"string","enum":["a"]},"n":{"type":"integer","default":1}},"required":["kind"]},
		{"type":"integer"}
	]}`
	assertNormalized(t, oneOfSchema, `{"kind":"a"}`, `{"kind":"a","n":1}`)
}

func TestValidateDefaultDeeplyNested(t *testing.T) {
	schemaJSON := `{
		"type":"object",
		"properties":{"a":{"type":"object","properties":{"b":{"type":"object","properties":{
			"c":{"type":"string","default":"deep"}
		}}},"required":["b"]}},
		"required":["a"]
	}`
	assertNormalized(t, schemaJSON, `{"a":{"b":{}}}`, `{"a":{"b":{"c":"deep"}}}`)
}

func TestValidateExtraPropsOmittedEverywhere(t *testing.T) {
	// Undeclared properties never error; they are dropped at every depth: the
	// root, a nested object, an array element, a $ref target, and the matched
	// oneOf branch.
	schemaJSON := `{
		"type":"object",
		"properties":{
			"user":{"$ref":"#/$defs/User"},
			"items":{"type":"array","items":{"type":"object","properties":{"v":{"type":"integer"}},"required":["v"]}},
			"choice":{"oneOf":[
				{"type":"object","properties":{"s":{"type":"string"}},"required":["s"]},
				{"type":"integer"}
			]}
		},
		"required":["user"],
		"$defs":{"User":{"type":"object","properties":{"name":{"type":"string"}},"required":["name"]}}
	}`
	assertNormalized(t, schemaJSON,
		`{
			"user":{"name":"al","password":"leak-me-not"},
			"items":[{"v":1,"debug":true}],
			"choice":{"s":"x","extra":[1,2]},
			"rootJunk":{"deep":{"deeper":1}}
		}`,
		`{
			"user":{"name":"al"},
			"items":[{"v":1}],
			"choice":{"s":"x"}
		}`)
}

func TestValidatePreservesDataThroughSecretFields(t *testing.T) {
	// Validation itself must not redact: secret is a logging concern, and the
	// normalized value keeps the real data (only undeclared keys are dropped).
	schemaJSON := `{
		"type":"object",
		"properties":{"token":{"type":"string","secret":true}},
		"required":["token"]
	}`
	got := validated(t, schemaJSON, `{"token":"s3cr3t","junk":1}`)
	want := map[string]any{"token": "s3cr3t"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("normalized = %v, want %v", got, want)
	}
}

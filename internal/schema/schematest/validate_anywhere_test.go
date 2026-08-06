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

func TestSecretAtAncestorObject(t *testing.T) {
	// A whole object marked secret taints every path through it — reading from
	// inside a secret object is itself secret.
	sc := mustParse(t, `{
		"type":"object",
		"properties":{
			"creds":{"type":"object","secret":true,"properties":{"user":{"type":"string"},"pass":{"type":"string"}},"required":["user","pass"]},
			"plain":{"type":"string"}
		},
		"required":["creds"]
	}`)
	for path, want := range map[string]bool{
		"creds":      true,
		"creds.user": true, // through the secret ancestor
		"creds.pass": true,
		"plain":      false,
	} {
		if got := sc.SecretAt(path); got != want {
			t.Errorf("SecretAt(%q) = %v, want %v", path, got, want)
		}
	}
	// Redact replaces the whole secret subtree, not just leaves.
	red := sc.Redact(map[string]any{
		"creds": map[string]any{"user": "u", "pass": "p"},
		"plain": "ok",
	}).(map[string]any)
	if red["creds"] != "***" {
		t.Errorf("secret object not wholly redacted: %v", red["creds"])
	}
	if red["plain"] != "ok" {
		t.Errorf("non-secret sibling altered: %v", red["plain"])
	}
}

func TestSecretAtThroughArrayIndex(t *testing.T) {
	sc := mustParse(t, `{
		"type":"object",
		"properties":{"list":{"type":"array","items":{
			"type":"object","properties":{"token":{"type":"string","secret":true},"ok":{"type":"string"}},"required":["token"]
		}}},
		"required":["list"]
	}`)
	if !sc.SecretAt("list[0].token") {
		t.Error("SecretAt(list[0].token) = false, want true")
	}
	if sc.SecretAt("list[2].ok") {
		t.Error("SecretAt(list[2].ok) = true, want false")
	}
}

func TestSecretAtOptionalProperty(t *testing.T) {
	// An optional property navigates through a nullable wrapper; the secret mark
	// must survive both the simple-type widening and the oneOf-null wrapping.
	sc := mustParse(t, `{
		"type":"object",
		"properties":{
			"apiKey":{"type":"string","secret":true},
			"vault":{"type":"object","secret":true,"properties":{"x":{"type":"string"}}}
		}
	}`)
	if !sc.SecretAt("apiKey") {
		t.Error("optional simple secret lost through nullable widening")
	}
	if !sc.SecretAt("vault") {
		t.Error("optional secret object lost through oneOf-null wrapping")
	}
	if !sc.SecretAt("vault.x") {
		t.Error("path through optional secret object should be secret")
	}
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

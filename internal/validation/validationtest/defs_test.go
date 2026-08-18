package validationtest

import (
	"slices"
	"testing"

	"genroc/internal/validation"
)

// A process-level $defs definition shared by two result_schemas: output maps read
// typed fields through the shared $ref, so inference must resolve it in every
// task's context.
const sharedDefsProcess = `{
	"name": "orders",
	"$defs": {
		"User": {"type":"object","properties":{"name":{"type":"string"},"vip":{"type":"boolean"}},"required":["name","vip"]}
	},
	"tasks": [
		{
			"id": "fetch",
			"action": {
				"type": "fetch", "url": "http://x",
				"responses": { "200": {"type":"object","properties":{"buyer":{"$ref":"#/$defs/User"}},"required":["buyer"]} }
			},
			"output": {"who": "'${ self.result.buyer.name }'"},
			"switch": [{"goto": "next"}]
		},
		{
			"id": "audit",
			"action": {
				"type": "fetch", "url": "http://y",
				"responses": { "200": {"type":"object","properties":{"reviewer":{"$ref":"#/$defs/User"}},"required":["reviewer"]} }
			},
			"output": {"flag": "$: self.result.reviewer.vip"}
		}
	]
}`

func TestGenerate_SharedDefsAcrossResults(t *testing.T) {
	out := runGenerate(t, sharedDefsProcess)

	// Inference resolved the shared def in both task contexts and typed the
	// output-map reads through it.
	assertJSON(t, out.Tasks["fetch"].Output, `{"$ref": "#/$defs/fetch_output"}`)
	fetchOut, ok := out.Defs.Get("fetch_output")
	if !ok {
		t.Fatal("fetch_output def missing")
	}
	assertJSON(t, fetchOut, `{"type":"object","properties":{"who":{"type":"string"}},"required":["who"]}`)
	auditOut, ok := out.Defs.Get("audit_output")
	if !ok {
		t.Fatal("audit_output def missing")
	}
	assertJSON(t, auditOut, `{"type":"object","properties":{"flag":{"type":"boolean"}},"required":["flag"]}`)

	// The shared definition itself is part of the emitted schema vocabulary.
	if !out.Defs.Has("User") {
		t.Errorf("shared def User missing from SchemaFile defs: %v", out.Defs.Names())
	}
}

func TestGenerate_SharedDefUsedByInputAndResults(t *testing.T) {
	// When the input schema uses the same shared def, its baked copy is hoisted
	// under the definition's own name and the pool merge must not duplicate it
	// (no User_1).
	out := runGenerate(t, `{
		"name": "p",
		"$defs": {"User": {"type":"object","properties":{"name":{"type":"string"}},"required":["name"]}},
		"input_schema": {"type":"object","properties":{"u":{"$ref":"#/$defs/User"}},"required":["u"]},
		"tasks": [{
			"id": "s1",
			"action": {
				"type": "fetch", "url": "http://x",
				"responses": { "200": {"type":"object","properties":{"owner":{"$ref":"#/$defs/User"}},"required":["owner"]} }
			},
			"output": {"n": "'${ self.result.owner.name }'"}
		}]
	}`)
	names := out.Defs.Names()
	if !slices.Contains(names, "User") {
		t.Fatalf("User def missing: %v", names)
	}
	for _, n := range names {
		if n == "User_1" {
			t.Errorf("shared def duplicated in generated defs: %v", names)
		}
	}
}

func TestGenerate_GeneratedNamesTakePrecedenceByRenaming(t *testing.T) {
	// A user definition named like a generated schema (here s1_output, colliding
	// with task s1's output def) is renamed with a unique suffix and every $ref
	// to it rewritten — the generated name keeps its meaning, and inference
	// through the renamed definition still types correctly.
	out := runGenerate(t, `{
		"name": "p",
		"$defs": {"s1_output": {"type":"object","properties":{"n":{"type":"integer"}},"required":["n"]}},
		"tasks": [{
			"id": "s1",
			"action": {
				"type": "fetch", "url": "http://x",
				"responses": { "200": {"type":"object","properties":{"d":{"$ref":"#/$defs/s1_output"}},"required":["d"]} }
			},
			"output": {"num": "$: self.result.d.n"},
			"switch": [{"goto": "end"}]
		}]
	}`)

	// The generated name holds the task's inferred output — proving the user's
	// colliding definition did not capture it.
	assertJSON(t, out.Tasks["s1"].Output, `{"$ref": "#/$defs/s1_output"}`)
	gen, ok := out.Defs.Get("s1_output")
	if !ok {
		t.Fatal("generated s1_output def missing")
	}
	assertJSON(t, gen, `{"type":"object","properties":{"num":{"type":"integer"}},"required":["num"]}`)

	// The user's definition survived under a suffixed name, and the inference
	// above (num: integer, read through the renamed ref) proves the embedded
	// result schema's $refs were rewritten to it.
	userDef, ok := out.Defs.Get("s1_output_1")
	if !ok {
		t.Fatalf("renamed user def s1_output_1 missing: %v", out.Defs.Names())
	}
	assertJSON(t, userDef, `{"type":"object","properties":{"n":{"type":"integer"}},"required":["n"]}`)
}

func TestContextSchema_SecretThroughSharedDef(t *testing.T) {
	// A secret marked inside the shared definition is detected through the $ref
	// on any path that reaches it — the reuse case for sensitive shapes.
	out := runGenerate(t, `{
		"name": "p",
		"$defs": {"Cred": {"type":"object","properties":{"token":{"type":"string","secret":true},"host":{"type":"string"}},"required":["token","host"]}},
		"input_schema": {"type":"object","properties":{"cred":{"$ref":"#/$defs/Cred"}},"required":["cred"]},
		"tasks": [{"id":"s1","action":{"type":"fetch","url":"http://x"}}]
	}`)
	ctx := validation.SchemaFileContext(out)
	if !ctx.SecretAt("input.cred.token") {
		t.Error("SecretAt(input.cred.token) = false, want true (secret inside shared def)")
	}
	if ctx.SecretAt("input.cred.host") {
		t.Error("SecretAt(input.cred.host) = true, want false")
	}
}

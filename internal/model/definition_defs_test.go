package model

import (
	"encoding/json"
	"strings"
	"testing"

	"genroc/internal/schema"
)

// mustDefs parses a JSON {"name": schema, ...} object into a Defs pool.
func mustDefs(t *testing.T, src string) schema.Defs {
	t.Helper()
	var d schema.Defs
	if err := json.Unmarshal([]byte(src), &d); err != nil {
		t.Fatalf("parse defs: %v", err)
	}
	return d
}

// defsDef builds a minimal valid definition with a process-level $defs pool whose
// input_schema references the shared "User" definition.
func defsDef(t *testing.T) *ProcessDefinition {
	return &ProcessDefinition{
		Name: "p",
		Defs: mustDefs(t, `{
			"User":{"type":"object","properties":{"name":{"type":"string"},"role":{"type":"string","default":"member"}},"required":["name"]}
		}`),
		InputSchema: mustSchemaPtr(`{"type":"object","properties":{"user":{"$ref":"#/$defs/User"}},"required":["user"]}`),
		Tasks:       []*Task{{ID: "s1", Action: &Action{Type: ActionTypeFetch, Method: "post", URL: "http://x"}, Switch: SwitchMap{{Goto: GotoEnd}}}},
	}
}

func TestDefsNamesCollidingWithGeneratedAreAccepted(t *testing.T) {
	// Names that collide with generated schema names are legal: generation
	// renames the user definition (rewriting its $refs) rather than rejecting it.
	// Validate therefore accepts them all.
	for _, name := range []string{"input", "output", "charge_output", "charge_input"} {
		def := &ProcessDefinition{
			Name:  "p",
			Defs:  mustDefs(t, `{"`+name+`":{"type":"string"}}`),
			Tasks: []*Task{{ID: "charge", Action: &Action{Type: ActionTypeFetch, Method: "post", URL: "http://x"}, Switch: SwitchMap{{Goto: GotoEnd}}}},
		}
		if err := def.Validate(); err != nil {
			t.Errorf("$defs %q: expected acceptance (generation renames), got %v", name, err)
		}
	}
}

func TestDefsValidateChecksPoolDocs(t *testing.T) {
	// A pool def with an unresolvable $ref is rejected; cross-refs within the
	// pool are fine.
	bad := &ProcessDefinition{
		Name:  "p",
		Defs:  mustDefs(t, `{"A":{"$ref":"#/$defs/Nowhere"}}`),
		Tasks: []*Task{{ID: "s1", Action: &Action{Type: ActionTypeFetch, Method: "post", URL: "http://x"}, Switch: SwitchMap{{Goto: GotoEnd}}}},
	}
	if err := bad.Validate(); err == nil {
		t.Error("expected error for pool def with unresolvable $ref")
	}
	good := &ProcessDefinition{
		Name:  "p",
		Defs:  mustDefs(t, `{"A":{"$ref":"#/$defs/B"},"B":{"type":"string"}}`),
		Tasks: []*Task{{ID: "s1", Action: &Action{Type: ActionTypeFetch, Method: "post", URL: "http://x"}, Switch: SwitchMap{{Goto: GotoEnd}}}},
	}
	if err := good.Validate(); err != nil {
		t.Errorf("pool cross-ref rejected: %v", err)
	}
}

func TestDefsValidateAcceptsPoolRefsBeforeNormalize(t *testing.T) {
	// The dry-run validate path runs Validate on unnormalized schemas: a schema
	// referencing a pool definition must pass the doc check via the merged pool.
	def := defsDef(t)
	if err := def.Validate(); err != nil {
		t.Fatalf("Validate before Normalize: %v", err)
	}
}

func TestDefsNormalizeBakesSharedDefs(t *testing.T) {
	def := defsDef(t)
	if err := def.Normalize(); err != nil {
		t.Fatalf("Normalize: %v", err)
	}
	// The input schema is self-contained: runtime validation resolves the shared
	// def (required check, default fill, pruning) without any external context.
	got, err := def.ValidateInput(map[string]any{"user": map[string]any{"name": "al", "junk": 1}})
	if err != nil {
		t.Fatalf("ValidateInput: %v", err)
	}
	user := got.(map[string]any)["user"].(map[string]any)
	if user["name"] != "al" || user["role"] != "member" || len(user) != 2 {
		t.Errorf("normalized user = %v, want name=al role=member and junk pruned", user)
	}
	if _, err := def.ValidateInput(map[string]any{"user": map[string]any{}}); err == nil {
		t.Error("expected error: required User.name missing")
	}
	// Self-containment is visible in the marshaled schema: $defs.User is baked in.
	b, _ := json.Marshal(def.InputSchema)
	if !strings.Contains(string(b), `"$defs"`) || !strings.Contains(string(b), `"User"`) {
		t.Errorf("input_schema not self-contained after Normalize: %s", b)
	}
}

func TestDefsNormalizeIsIdempotent(t *testing.T) {
	def := defsDef(t)
	if err := def.Normalize(); err != nil {
		t.Fatalf("first Normalize: %v", err)
	}
	first, _ := json.Marshal(def)
	// A definition reloaded from the DB is normalized again by Generate; the
	// baked pool copies must not trip the shadow check or duplicate.
	if err := def.Normalize(); err != nil {
		t.Fatalf("second Normalize: %v", err)
	}
	second, _ := json.Marshal(def)
	if string(first) != string(second) {
		t.Errorf("Normalize not idempotent:\n first: %s\nsecond: %s", first, second)
	}
}

func TestDefsLocalDefinitionWinsForItsSchema(t *testing.T) {
	// Nearest-wins scoping: a schema-local root definition shadows the
	// process-level one of the same name for that schema only.
	def := defsDef(t)
	def.InputSchema = mustSchemaPtr(`{
		"type":"object",
		"properties":{"user":{"$ref":"#/$defs/User"}},
		"required":["user"],
		"$defs":{"User":{"type":"integer"}}
	}`)
	if err := def.Normalize(); err != nil {
		t.Fatalf("Normalize: %v", err)
	}
	// The input schema validates against ITS local User (integer), not the pool's.
	if _, err := def.ValidateInput(map[string]any{"user": 5}); err != nil {
		t.Errorf("local integer User rejected: %v", err)
	}
	if _, err := def.ValidateInput(map[string]any{"user": map[string]any{"name": "al"}}); err == nil {
		t.Error("pool-shaped user accepted although the local definition shadows it")
	}
}

func TestDefsResultSchemaValidatesThroughSharedDef(t *testing.T) {
	def := defsDef(t)
	def.Tasks[0].Action.ResultSchema = mustSchemaPtr(`{
		"type":"object","properties":{"owner":{"$ref":"#/$defs/User"}},"required":["owner"]
	}`)
	if err := def.Normalize(); err != nil {
		t.Fatalf("Normalize: %v", err)
	}
	got, err := def.Tasks[0].Action.ValidateOutput(map[string]any{"owner": map[string]any{"name": "bo"}})
	if err != nil {
		t.Fatalf("ValidateOutput: %v", err)
	}
	owner := got.(map[string]any)["owner"].(map[string]any)
	if owner["role"] != "member" {
		t.Errorf("shared default not applied through result_schema ref: %v", owner)
	}
}

func TestDefsJSONRoundTrip(t *testing.T) {
	src := `{
		"name":"p",
		"$defs":{"User":{"type":"object","properties":{"name":{"type":"string"}},"required":["name"]}},
		"input_schema":{"type":"object","properties":{"u":{"$ref":"#/$defs/User"}},"required":["u"]},
		"tasks":[{"id":"s1","action":{"type":"fetch","method":"post","url":"http://x"}}]
	}`
	var def ProcessDefinition
	if err := json.Unmarshal([]byte(src), &def); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if def.Defs.Len() != 1 || !def.Defs.Has("User") {
		t.Fatalf("defs not parsed: %v", def.Defs.Names())
	}
	b, err := json.Marshal(&def)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(b), `"$defs"`) {
		t.Errorf("marshal dropped $defs: %s", b)
	}
	// A definition without $defs omits the field entirely.
	plain := ProcessDefinition{Name: "p", Tasks: []*Task{{ID: "s1", Action: &Action{Type: ActionTypeFetch, Method: "post", URL: "http://x"}, Switch: SwitchMap{{Goto: GotoEnd}}}}}
	pb, _ := json.Marshal(&plain)
	if strings.Contains(string(pb), `"$defs"`) {
		t.Errorf("empty $defs not omitted: %s", pb)
	}
}

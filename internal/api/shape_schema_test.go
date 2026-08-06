package api

import (
	"encoding/json"
	"strings"
	"testing"
)

// Guards the served process-schema.json wiring for the recursive Shape type: the generic
// Value anyOf, the self $ref, and the task-output/action-input references. Breaking it
// silently breaks editor autocomplete.
func TestProcessSchemaShape(t *testing.T) {
	b := buildProcessDefinitionSchema()
	var root map[string]any
	if err := json.Unmarshal(b, &root); err != nil {
		t.Fatal(err)
	}
	defs, _ := root["$defs"].(map[string]any)
	shape, ok := defs["ModelShape"].(map[string]any)
	if !ok {
		t.Fatalf("no ModelShape def; defs=%v", keysOf(defs))
	}
	// anyOf (not oneOf): the string branch overlaps nested string leaves, so oneOf's
	// exactly-one rule would spuriously reject them.
	variants, ok := shape["anyOf"].([]any)
	if !ok || len(variants) != 6 {
		t.Fatalf("ModelShape should be anyOf with 6 variants (string/number/boolean/null/array/object), got %v", shape["anyOf"])
	}
	// The array and object branches must recurse back into the Shape def.
	raw, _ := json.Marshal(shape)
	if !strings.Contains(string(raw), `"$ref":"#/$defs/ModelShape"`) {
		t.Errorf("ModelShape must recurse via #/$defs/ModelShape; got %s", raw)
	}
	// Task.output must reference the Shape def.
	task, _ := defs["ModelTask"].(map[string]any)
	props, _ := task["properties"].(map[string]any)
	if ob, _ := json.Marshal(props["output"]); !strings.Contains(string(ob), "ModelShape") {
		t.Errorf("Task.output should reference ModelShape, got %s", ob)
	}
	// The action's input (now on the action union, shared by fetch/child)
	// must reference the Shape def.
	action, _ := defs["ModelAction"].(map[string]any)
	ab, _ := json.Marshal(action)
	if !strings.Contains(string(ab), "ModelShape") {
		t.Errorf("Action.input should reference ModelShape, got %s", ab)
	}
}

func keysOf(m map[string]any) []string {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	return ks
}

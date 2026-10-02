package validationtest

// An upgrade conforms the stored state through the ONE compat layer of the task the instance
// sits on. specs/version-compatibility.md s1.

import (
	"encoding/json"
	"reflect"
	"testing"

	"genroc/internal/model"
	"genroc/internal/validation"
)

// At `work`, `first` has produced an output and `later` has not, so the layer chosen matters.
// noteRequired toggles the null-versus-missing gap a migration must close.
func twoTaskDef(noteRequired bool) string {
	req := ``
	if noteRequired {
		req = `,"required":["note"]`
	}
	return `{"name":"noted","tasks":[
		{"id":"first","output":{"v":"$: 1"},"switch":[{"goto":"$work"}]},
		{"id":"work","switch":[{"goto":"$later"}]},
		{"id":"later","output":{"w":"$: 2"},"switch":"end"}],
		"input_schema":{"type":"object","properties":{"note":{"type":["string","null"]}}` + req + `}}`
}

func stateAtWork() map[string]any {
	return map[string]any{
		"input":   map[string]any{},
		"outputs": map[string]any{"first": map[string]any{"v": float64(1)}},
	}
}

func TestMigrateState_ClosesTheNullGap(t *testing.T) {
	// Permitting the gap is not enough: the written state must satisfy the version it is
	// written for.
	to := defFrom(t, twoTaskDef(true))

	got, err := validation.MigrateState(to, "work", stateAtWork(), nil)
	if err != nil {
		t.Fatalf("MigrateState: %v", err)
	}
	in, ok := got["input"].(map[string]any)
	if !ok {
		t.Fatalf("migrated state lost its input: %#v", got)
	}
	v, present := in["note"]
	if !present || v != nil {
		t.Fatalf("input.note = %#v (present=%v), want an inserted null", v, present)
	}
}

// Inside `outputs` a task the schema does not name is pruned; at the top, the engine's
// bookkeeping is none of the layer's business.
func TestMigrateState_PrunesDeadOutputsAndKeepsBookkeeping(t *testing.T) {
	to := defFrom(t, twoTaskDef(false))
	state := stateAtWork()
	state["outputs"].(map[string]any)["gone"] = map[string]any{"x": float64(9)}
	state["_children"] = map[string]any{"work": "01a03d00-0000-7000-8000-000000000000"}
	state["_spawn_index"] = float64(2)

	got, err := validation.MigrateState(to, "work", state, nil)
	if err != nil {
		t.Fatalf("MigrateState: %v", err)
	}
	outs, _ := got["outputs"].(map[string]any)
	if _, kept := outs["gone"]; kept {
		t.Errorf("the output of a task the target does not declare survived: %#v", outs)
	}
	if _, kept := outs["first"]; !kept {
		t.Errorf("a live task's output was stripped with the dead one: %#v", outs)
	}
	for _, k := range []string{"_children", "_spawn_index"} {
		if _, kept := got[k]; !kept {
			t.Errorf("%s is the engine's, not the layer's, and must survive the migration", k)
		}
	}
}

func TestMigrateState_UsesTheLayerForThisTaskOnly(t *testing.T) {
	// At `work`, `later` has not run: the same state migrates there and is judged differently
	// against `later`'s own layer. One shared schema would not discriminate.
	to := defFrom(t, twoTaskDef(false))

	if _, err := validation.MigrateState(to, "work", stateAtWork(), nil); err != nil {
		t.Fatalf("state at `work` should fit `work`'s layer: %v", err)
	}
	if _, err := validation.MigrateState(to, "nonexistent", stateAtWork(), nil); err == nil {
		t.Fatal("migrated against a task the version does not have; there is no layer to conform through")
	}
}

func TestMigrateState_RefusesWhatCannotBeReconciled(t *testing.T) {
	// A stored string where the target requires a number is not a gap a migration can
	// close, and the validator saying so IS the refusal.
	to := defFrom(t, `{"name":"noted","tasks":[{"id":"work","switch":"end"}],
		"input_schema":{"type":"object","properties":{"note":{"type":"number"}},"required":["note"]}}`)

	_, err := validation.MigrateState(to, "work", map[string]any{
		"input": map[string]any{"note": "text"}, "outputs": map[string]any{},
	}, nil)
	if err == nil {
		t.Fatal("migrated an instance whose stored input cannot be reconciled with the target schema")
	}
}

func TestMigrateState_CarriesEngineBookkeepingThrough(t *testing.T) {
	// Losing external_input unparks an instance still waiting. Carry-through is "the layer does
	// not describe this key", not "starts with _": safe only while no layer declares one.
	to := defFrom(t, twoTaskDef(false))
	state := stateAtWork()
	state[model.StateExternalInput] = map[string]any{"n": float64(1)}
	state["_spawn_child_key"] = "out"

	got, err := validation.MigrateState(to, "work", state, nil)
	if err != nil {
		t.Fatalf("MigrateState: %v", err)
	}
	if got["_spawn_child_key"] != "out" {
		t.Errorf("_spawn_child_key came back %#v; the slot a child occupies is what its upgrade reads", got["_spawn_child_key"])
	}
	ext, ok := got[model.StateExternalInput].(map[string]any)
	if !ok || ext["n"] != float64(1) {
		t.Errorf("external_input came back %#v; a parked instance would be unparked by its own upgrade", got[model.StateExternalInput])
	}
}

// The conform cannot normalize inside a MARKER it has not loaded; unresolved, the refusal blames
// the type ("got *model.ObjectRef") instead of the reason.
func TestMigrateState_MovesAnInstanceHoldingAnExternalizedValue(t *testing.T) {
	var def model.ProcessDefinition
	if err := json.Unmarshal([]byte(`{"name":"acc","tasks":[
		{"id":"t","action":{"type":"delay","for":"1s"},
		 "output":{"items":"$: [1,2,3]"},
		 "switch":[{"case":"self.output.items != null","goto":"$t"},{"goto":"end"}]}
	]}`), &def); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if _, err := validation.Generate(&def); err != nil {
		t.Fatalf("Generate: %v", err)
	}

	content := []any{1.0, 2.0, 3.0}
	loads := 0
	load := func(string) (any, error) { loads++; return content, nil }

	state := map[string]any{"outputs": map[string]any{"t": map[string]any{
		"items": &model.ObjectRef{Ref: "deadbeef", Size: 4096},
	}}}

	moved, err := validation.MigrateState(&def, "t", state, load)
	if err != nil {
		t.Fatalf("an instance whose value outgrew the row must still move: %v", err)
	}
	if loads == 0 {
		t.Error("the marker was never resolved, so the conform judged a reference rather than the value")
	}
	// The value itself comes through, not the marker: the write that follows re-cuts it, and
	// identical content hashes to the same object.
	outs, _ := moved["outputs"].(map[string]any)
	task, _ := outs["t"].(map[string]any)
	if got := task["items"]; !reflect.DeepEqual(got, content) {
		t.Errorf("migrated state holds %#v, want the resolved value %#v", got, content)
	}
}

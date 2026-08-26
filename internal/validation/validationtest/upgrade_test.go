package validationtest

// Moving an instance uses ONE of compat's per-task layers -- the one for the task it is
// sitting on -- and conforms the stored state through it. The validator's answer is the
// verdict. specs/version-compatibility.md s1.

import (
	"testing"

	"genroc/internal/validation"
)

// twoTaskDef has a task before and after `work`, so the layer chosen actually matters:
// `first` has produced an output by the time an instance reaches `work`, and `later` has
// not. noteRequired flips the input's `note` between optional-and-nullable and required,
// which is the null-versus-missing gap a migration has to close.
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
	// `note` is absent, required on the target, and admits null: the migration writes the
	// null in. Permitting the gap is not enough -- the state that gets written has to
	// satisfy the version it is written for.
	to := defFrom(t, twoTaskDef(true))

	got, err := validation.MigrateState(to, "work", stateAtWork())
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

// The layer is PARTIAL at the top and complete below it, and the migration has to treat those
// two halves oppositely. Inside `outputs` the schema names every task the target has, so a task
// it does not name is gone: nothing on the new version can read that output -- an expression
// naming it is refused at registration -- and keeping it stores weight that only grows and pins
// whatever it references. At the top the layer names only a definition's own slots, so the
// engine's bookkeeping is not undeclared-and-dead, it is simply none of the layer's business.
func TestMigrateState_PrunesDeadOutputsAndKeepsBookkeeping(t *testing.T) {
	to := defFrom(t, twoTaskDef(false))
	state := stateAtWork()
	state["outputs"].(map[string]any)["gone"] = map[string]any{"x": float64(9)}
	state["_children"] = map[string]any{"work": "01a03d00-0000-7000-8000-000000000000"}
	state["_spawn_index"] = float64(2)

	got, err := validation.MigrateState(to, "work", state)
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
	// The layer is why one instance can move where another cannot. At `work`, `first` has
	// run and `later` has not -- so a state carrying no `later` output migrates cleanly,
	// and the same state is judged against `later`'s own layer differently. If every task
	// shared one schema this would not discriminate.
	to := defFrom(t, twoTaskDef(false))

	if _, err := validation.MigrateState(to, "work", stateAtWork()); err != nil {
		t.Fatalf("state at `work` should fit `work`'s layer: %v", err)
	}
	if _, err := validation.MigrateState(to, "nonexistent", stateAtWork()); err == nil {
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
	})
	if err == nil {
		t.Fatal("migrated an instance whose stored input cannot be reconciled with the target schema")
	}
}

func TestMigrateState_CarriesEngineBookkeepingThrough(t *testing.T) {
	// A real state holds more than input/outputs: _external for a parked task, and the spawn
	// discriminants for a live child. compat's layers describe none of it -- they are about the
	// data a definition can see -- so the migration must carry it through untouched. Losing
	// _external unparks an instance from a task it is still waiting on.
	to := defFrom(t, twoTaskDef(false))
	state := stateAtWork()
	state["_external"] = map[string]any{"task_id": "work", "input": map[string]any{"n": float64(1)}}
	state["_spawn_child_key"] = "out"

	got, err := validation.MigrateState(to, "work", state)
	if err != nil {
		t.Fatalf("MigrateState: %v", err)
	}
	if got["_spawn_child_key"] != "out" {
		t.Errorf("_spawn_child_key came back %#v; the slot a child occupies is what its upgrade reads", got["_spawn_child_key"])
	}
	ext, ok := got["_external"].(map[string]any)
	if !ok || ext["task_id"] != "work" {
		t.Errorf("_external came back %#v; a parked instance would be unparked by its own upgrade", got["_external"])
	}
}

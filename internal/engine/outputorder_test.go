package engine

import (
	"testing"

	"genroc/internal/model"
)

// output_order is PERSISTED (db_instances.go's outputsColumn), so a duplicate is not a
// display quirk: a child task in a loop spawns on every pass and appended each time, which
// grew the stored list without bound and made the outputs object serialise one key
// repeatedly. `outputs` holds a single value per task, so a second position could never be
// filled — uniqueness is the invariant, not a preference.

func orderOf(t *testing.T, inst *model.ProcessInstance) []string {
	t.Helper()
	order, ok := inst.State["output_order"].([]string)
	if !ok {
		t.Fatalf("output_order is %T, want []string", inst.State["output_order"])
	}
	return order
}

func TestAppendOutputOrder_RepeatedAppendsKeepOnePosition(t *testing.T) {
	inst := &model.ProcessInstance{State: map[string]any{}}
	for i := 0; i < 5; i++ {
		appendOutputOrder(inst, "call")
	}
	if got := orderOf(t, inst); len(got) != 1 || got[0] != "call" {
		t.Errorf("five spawns of one task recorded %v", got)
	}
}

func TestAppendOutputOrder_KeepsFirstCompletionOrder(t *testing.T) {
	inst := &model.ProcessInstance{State: map[string]any{}}
	for _, id := range []string{"a", "b", "a", "c", "b"} {
		appendOutputOrder(inst, id)
	}
	got := orderOf(t, inst)
	want := []string{"a", "b", "c"}
	if len(got) != len(want) {
		t.Fatalf("order = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("order = %v, want %v", got, want)
		}
	}
}

func TestAppendOutputOrder_RepairsARowThatAlreadyHasDuplicates(t *testing.T) {
	// Rows written before uniqueness held keep their repeats forever unless the rebuild
	// drops them — so the next append is what heals them, rather than a migration.
	inst := &model.ProcessInstance{State: map[string]any{
		"output_order": []any{"a", "call", "call", "call"},
	}}
	appendOutputOrder(inst, "b")
	got := orderOf(t, inst)
	want := []string{"a", "call", "b"}
	if len(got) != len(want) {
		t.Fatalf("order = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("order = %v, want %v", got, want)
		}
	}
}

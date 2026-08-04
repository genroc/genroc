package dbtest

import (
	"testing"

	dbpkg "genroc/internal/db"
	"genroc/internal/model"
)

// ApplyDefinitions commits a whole planned batch or none of it. The API layer validates
// every definition before calling it, so what these cover is the other half: that a write
// failing partway leaves nothing behind, and that a batch's own cross-references land
// together.

func def(name string, taskID string) *model.ProcessDefinition {
	return &model.ProcessDefinition{Name: name, Tasks: []*model.Task{{ID: taskID}}}
}

func TestApplyDefinitions_CommitsWholeBatch(t *testing.T) {
	for _, b := range testBackends(t) {
		t.Run(b.name, func(t *testing.T) {
			writes := []dbpkg.DefinitionWrite{
				{Def: def("kid", "s1"), Name: "kid", Version: 1, Hash: "kid-1", Channels: []string{"latest"}},
				{
					Def: def("mum", "spawn"), Name: "mum", Version: 1, Hash: "mum-1",
					Channels: []string{"latest", "stable"},
					Deps: []dbpkg.DependencyRow{{
						ParentName: "mum", ParentVersion: 1, TaskID: "spawn",
						ChildName: "kid", ChildVersion: 1,
					}},
				},
			}
			if err := b.db.ApplyDefinitions(writes); err != nil {
				t.Fatalf("ApplyDefinitions: %v", err)
			}

			for _, name := range []string{"kid", "mum"} {
				if _, err := b.db.GetDefinition(name, 1); err != nil {
					t.Errorf("%s@v1 not stored: %v", name, err)
				}
			}
			// Every listed channel is pointed, not just the first.
			for _, ch := range []string{"latest", "stable"} {
				if v, err := b.db.GetChannel("mum", ch); err != nil || v != 1 {
					t.Errorf("mum@%s = (%d, %v), want v1", ch, v, err)
				}
			}
			// The dependency row landed in the same commit as the definition that owns it,
			// so a parent is never stored referencing a child row that does not exist.
			childV, err := b.db.GetDependencyVersion("mum", 1, "spawn", "")
			if err != nil {
				t.Fatalf("GetDependencyVersion: %v", err)
			}
			if childV != 1 {
				t.Errorf("mum's spawn task resolves kid@v%d, want v1", childV)
			}
		})
	}
}

// A write that fails partway must take the whole batch with it — the entries ahead of it
// are the ones a save-as-you-go loop would have left behind.
func TestApplyDefinitions_RollsBackOnFailure(t *testing.T) {
	for _, b := range testBackends(t) {
		t.Run(b.name, func(t *testing.T) {
			// The second entry names a version the first already claimed with different
			// content; the dependency insert below it references a parent version that was
			// never inserted, which the foreign key rejects.
			writes := []dbpkg.DefinitionWrite{
				{Def: def("first", "s1"), Name: "first", Version: 1, Hash: "first-1", Channels: []string{"latest"}},
				{
					Def: def("second", "s1"), Name: "second", Version: 1, Hash: "second-1",
					Channels: []string{"latest"},
					Deps: []dbpkg.DependencyRow{{
						ParentName: "second", ParentVersion: 99, // no such version
						TaskID: "spawn", ChildName: "nowhere", ChildVersion: 7,
					}},
				},
			}
			if err := b.db.ApplyDefinitions(writes); err == nil {
				t.Fatal("ApplyDefinitions succeeded, want the bad dependency to fail it")
			}

			// Neither the failing entry nor the one before it survives.
			for _, name := range []string{"first", "second"} {
				if _, err := b.db.GetDefinition(name, 1); err == nil {
					t.Errorf("%s@v1 was stored despite the batch failing", name)
				}
				if _, err := b.db.GetChannel(name, "latest"); err == nil {
					t.Errorf("%s kept a channel pointer despite the batch failing", name)
				}
			}
		})
	}
}

func TestApplyDefinitions_EmptyBatchIsANoOp(t *testing.T) {
	for _, b := range testBackends(t) {
		t.Run(b.name, func(t *testing.T) {
			if err := b.db.ApplyDefinitions(nil); err != nil {
				t.Fatalf("ApplyDefinitions(nil): %v", err)
			}
		})
	}
}

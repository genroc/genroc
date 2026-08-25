package dbtest

// The upgrade write. Its predicates are the safety story: the migrated state was conformed
// against ONE version at ONE task, so anything that moved since makes it stale, and a stale
// migration must not land. specs/version-compatibility.md s4.

import (
	"context"
	"errors"
	"testing"

	dbpkg "genroc/internal/db"
	"genroc/internal/model"
)

func upgradable(t *testing.T, db *dbpkg.DB, id string, status model.Status) *model.ProcessInstance {
	t.Helper()
	inst := &model.ProcessInstance{
		ID: id, ProcessName: "test", ProcessVersion: 1, Task: "step1",
		ContextData: map[string]any{"input": map[string]any{"a": float64(1)}},
		Status:      status,
	}
	if err := db.SaveInstance(inst); err != nil {
		t.Fatalf("SaveInstance: %v", err)
	}
	return inst
}

func TestUpgradeInstances_WritesVersionAndStateTogether(t *testing.T) {
	for _, b := range testBackends(t) {
		t.Run(b.name, func(t *testing.T) {
			inst := upgradable(t, b.db, "up-ok", model.StatusPaused)

			err := b.db.UpgradeInstances(context.Background(), []dbpkg.InstanceUpgrade{{
				Instance: inst, ToVersion: 2,
				NewContext: map[string]any{"input": map[string]any{"a": float64(1), "b": nil}},
			}})
			if err != nil {
				t.Fatalf("UpgradeInstances: %v", err)
			}

			got, err := b.db.GetInstance("up-ok")
			if err != nil {
				t.Fatalf("GetInstance: %v", err)
			}
			if got.ProcessVersion != 2 {
				t.Errorf("process_version = %d, want 2", got.ProcessVersion)
			}
			in, _ := got.ContextData["input"].(map[string]any)
			v, present := in["b"]
			if !present || v != nil {
				t.Errorf("migrated state was not written: input = %#v; the version is the lens the row is read through, so the two cannot land apart", in)
			}
		})
	}
}

func TestUpgradeInstances_RefusesWhatMovedUnderIt(t *testing.T) {
	for _, b := range testBackends(t) {
		t.Run(b.name, func(t *testing.T) {
			ctx := context.Background()
			base := func(id string, status model.Status) *model.ProcessInstance {
				return upgradable(t, b.db, id, status)
			}
			newState := map[string]any{"input": map[string]any{"a": float64(1)}}

			// Version moved: the state was conformed against a version the row has left.
			bumped := base("up-ver", model.StatusPaused)
			stale := *bumped
			stale.ProcessVersion = 99
			err := b.db.UpgradeInstances(ctx, []dbpkg.InstanceUpgrade{{Instance: &stale, ToVersion: 2, NewContext: newState}})
			if !errors.Is(err, dbpkg.ErrUpgradeStale) {
				t.Errorf("stale version: err=%v, want ErrUpgradeStale", err)
			}

			// Task moved: the state was conformed against ONE task's layer.
			moved := base("up-task", model.StatusPaused)
			elsewhere := *moved
			elsewhere.Task = "somewhere-else"
			err = b.db.UpgradeInstances(ctx, []dbpkg.InstanceUpgrade{{Instance: &elsewhere, ToVersion: 2, NewContext: newState}})
			if !errors.Is(err, dbpkg.ErrUpgradeStale) {
				t.Errorf("stale task: err=%v, want ErrUpgradeStale", err)
			}

			// Running: it can be claimed and advanced between the read and this write.
			running := base("up-running", model.StatusRunning)
			err = b.db.UpgradeInstances(ctx, []dbpkg.InstanceUpgrade{{Instance: running, ToVersion: 2, NewContext: newState}})
			if !errors.Is(err, dbpkg.ErrUpgradeStale) {
				t.Errorf("running: err=%v, want ErrUpgradeStale", err)
			}

		})
	}
}

func TestUpgradeInstances_AllOrNothing(t *testing.T) {
	// A cluster moves as a unit: a tree whose parent and children disagree about which
	// version describes their data is exactly what a partial write produces.
	for _, b := range testBackends(t) {
		t.Run(b.name, func(t *testing.T) {
			good := upgradable(t, b.db, "up-good", model.StatusPaused)
			bad := upgradable(t, b.db, "up-bad", model.StatusRunning) // refused: not settled
			state := map[string]any{"input": map[string]any{"a": float64(1)}}

			err := b.db.UpgradeInstances(context.Background(), []dbpkg.InstanceUpgrade{
				{Instance: good, ToVersion: 2, NewContext: state},
				{Instance: bad, ToVersion: 2, NewContext: state},
			})
			if !errors.Is(err, dbpkg.ErrUpgradeStale) {
				t.Fatalf("err=%v, want ErrUpgradeStale", err)
			}
			got, err := b.db.GetInstance("up-good")
			if err != nil {
				t.Fatalf("GetInstance: %v", err)
			}
			if got.ProcessVersion != 1 {
				t.Fatalf("the movable member of a refused cluster moved anyway (version %d); the batch has to roll back whole", got.ProcessVersion)
			}
		})
	}
}

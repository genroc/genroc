package dbtest

// Planning a tree's move. Only the root's target version is chosen; every child's is
// DERIVED from the definition its parent moves to. specs/version-compatibility.md s3c.

import (
	"context"
	"fmt"
	"testing"
	"time"

	dbpkg "genroc/internal/db"
	"genroc/internal/model"
	"genroc/internal/validation"
)

// upgradeTree composes what the operation's owner does: PLAN (db), MIGRATE (validation), WRITE
// (db). It lives here because what the new state IS is not internal/db's question.
func upgradeTree(t *testing.T, db *dbpkg.DB, rootID string, from, to int) error {
	t.Helper()
	root, err := db.GetInstance(rootID)
	if err != nil {
		return err
	}
	if root.ProcessVersion != from {
		return fmt.Errorf("%q is on version %d, not %d: %w", rootID, root.ProcessVersion, from, dbpkg.ErrUpgradeStale)
	}
	plan, err := db.PlanUpgrade(context.Background(), rootID, to)
	if err != nil {
		return err
	}
	var ups []dbpkg.InstanceUpgrade
	for _, m := range plan {
		if m.ToVersion == m.Instance.ProcessVersion {
			continue // already there: nothing to migrate and nothing to write
		}
		def, err := db.GetDefinition(m.Instance.ProcessName, m.ToVersion)
		if err != nil {
			return err
		}
		// The same loader the API passes: a migration conforms the value, so it resolves the
		// row's references first.
		state, err := validation.MigrateState(def, m.Instance.Task, m.Instance.State, db.ObjectLoader())
		if err != nil {
			return fmt.Errorf("%q: %w", m.Instance.ID, err)
		}
		ups = append(ups, dbpkg.InstanceUpgrade{Instance: m.Instance, ToVersion: m.ToVersion, NewContext: state})
	}
	return db.UpgradeInstances(context.Background(), ups)
}

// parentPinning registers parent@version whose task `fan` spawns `kid` at the declared
// version (0 = leave it to the dependency row / latest).
func parentPinning(t *testing.T, db *dbpkg.DB, version, kidVersion int) {
	t.Helper()
	def := &model.ProcessDefinition{Name: "par", Tasks: []*model.Task{{
		ID:     "fan",
		Action: &model.Action{Type: model.ActionTypeChild, Name: "kid", Version: kidVersion},
		Switch: model.SwitchMap{{Goto: model.GotoEnd}},
	}}}
	storeDef(t, db, def, version)
}

func saveKidDef(t *testing.T, db *dbpkg.DB, version int) {
	t.Helper()
	def := &model.ProcessDefinition{Name: "kid", Tasks: []*model.Task{
		{ID: "run", Switch: model.SwitchMap{{Goto: model.GotoEnd}}},
	}}
	storeDef(t, db, def, version)
}

func TestPlanUpgrade_ChildVersionComesFromTheParentsTargetDefinition(t *testing.T) {
	for _, b := range testBackends(t) {
		t.Run(b.name, func(t *testing.T) {
			saveKidDef(t, b.db, 1)
			saveKidDef(t, b.db, 7)
			parentPinning(t, b.db, 1, 1) // par@1 spawns kid@1
			parentPinning(t, b.db, 2, 7) // par@2 spawns kid@7

			parent := &model.ProcessInstance{
				ID: "p", ProcessName: "par", ProcessVersion: 1, Task: "fan",
				State: map[string]any{"outputs": map[string]any{}}, Status: model.StatusPaused,
				Phase: model.PhaseChildren,
			}
			if err := b.db.SaveInstance(parent); err != nil {
				t.Fatalf("SaveInstance parent: %v", err)
			}
			kid := &model.ProcessInstance{
				ID: "k", ProcessName: "kid", ProcessVersion: 1, Task: "run",
				State: map[string]any{"outputs": map[string]any{}}, Status: model.StatusPaused,
				ParentID: "p", SpawnTaskID: "fan",
			}
			if err := b.db.SaveInstance(kid); err != nil {
				t.Fatalf("SaveInstance kid: %v", err)
			}

			plan, err := b.db.PlanUpgrade(context.Background(), "p", 2)
			if err != nil {
				t.Fatalf("PlanUpgrade: %v", err)
			}
			got := map[string]int{}
			for _, m := range plan {
				got[m.Instance.ID] = m.ToVersion
			}
			if got["p"] != 2 {
				t.Errorf("root planned to version %d, want 2", got["p"])
			}
			// The point: nobody asked for kid@7. par@2's definition names it, so moving the
			// parent without moving the child leaves it running a version par@2 never mentions.
			if got["k"] != 7 {
				t.Errorf("child planned to version %d, want 7 — a child's target is derived from the parent's TARGET definition, not chosen", got["k"])
			}
		})
	}
}

func TestPlanUpgrade_SettledDescendantsStayPut(t *testing.T) {
	// A completed, raised or cancelled child is never run again — a raised slot is re-spawned
	// whole, at the version the parent's target names — so it is not part of the unit that moves.
	for _, settled := range []model.Status{model.StatusCompleted, model.StatusRaised, model.StatusCancelled} {
		for _, b := range testBackends(t) {
			t.Run(b.name+"/"+string(settled), func(t *testing.T) {
				saveKidDef(t, b.db, 1)
				saveKidDef(t, b.db, 7)
				parentPinning(t, b.db, 1, 1)
				parentPinning(t, b.db, 2, 7)

				if err := b.db.SaveInstance(&model.ProcessInstance{
					ID: "p2", ProcessName: "par", ProcessVersion: 1, Task: "fan",
					State: map[string]any{"outputs": map[string]any{}}, Status: model.StatusPaused,
				}); err != nil {
					t.Fatalf("SaveInstance: %v", err)
				}
				if err := b.db.SaveInstance(&model.ProcessInstance{
					ID: "k2", ProcessName: "kid", ProcessVersion: 1, Task: "run",
					State: map[string]any{"outputs": map[string]any{}}, Status: settled,
					ParentID: "p2", SpawnTaskID: "fan",
				}); err != nil {
					t.Fatalf("SaveInstance: %v", err)
				}

				plan, err := b.db.PlanUpgrade(context.Background(), "p2", 2)
				if err != nil {
					t.Fatalf("PlanUpgrade: %v", err)
				}
				for _, m := range plan {
					if m.Instance.ID == "k2" {
						t.Fatalf("a %s child was planned into the move; nothing runs it again", settled)
					}
				}
				if len(plan) != 1 {
					t.Fatalf("planned %d instances, want just the root", len(plan))
				}
			})
		}
	}
}

func TestUpgradeComposition_MovesParentAndChildTogether(t *testing.T) {
	// The whole operation from one call: name the root and the version it goes to, and the
	// tree follows. specs/version-compatibility.md s3c.
	for _, b := range testBackends(t) {
		t.Run(b.name, func(t *testing.T) {
			saveKidDef(t, b.db, 1)
			saveKidDef(t, b.db, 7)
			parentPinning(t, b.db, 1, 1) // par@1 spawns kid@1
			parentPinning(t, b.db, 2, 7) // par@2 spawns kid@7

			for _, inst := range []*model.ProcessInstance{
				{ID: "tp", ProcessName: "par", ProcessVersion: 1, Task: "fan",
					State: map[string]any{"outputs": map[string]any{}}, Status: model.StatusPaused, Phase: model.PhaseChildren},
				{ID: "tk", ProcessName: "kid", ProcessVersion: 1, Task: "run",
					State: map[string]any{"outputs": map[string]any{}}, Status: model.StatusPaused, ParentID: "tp", SpawnTaskID: "fan"},
			} {
				if err := b.db.SaveInstance(inst); err != nil {
					t.Fatalf("SaveInstance %s: %v", inst.ID, err)
				}
			}

			if err := upgradeTree(t, b.db, "tp", 1, 2); err != nil {
				t.Fatalf("UpgradeTree: %v", err)
			}
			parent, _ := b.db.GetInstance("tp")
			kid, _ := b.db.GetInstance("tk")
			if parent.ProcessVersion != 2 {
				t.Errorf("parent on version %d, want 2", parent.ProcessVersion)
			}
			if kid.ProcessVersion != 7 {
				t.Errorf("child on version %d, want 7 — the version par@2 names, which nobody passed in", kid.ProcessVersion)
			}
		})
	}
}

func TestUpgradeComposition_RefusesAStaleFromVersion(t *testing.T) {
	// fromVersion is asserted, not read: a tree that moved since the caller looked at it is
	// refused rather than half-migrated against a plan made for a version it has left.
	for _, b := range testBackends(t) {
		t.Run(b.name, func(t *testing.T) {
			saveKidDef(t, b.db, 1)
			parentPinning(t, b.db, 1, 1)
			parentPinning(t, b.db, 2, 1)
			if err := b.db.SaveInstance(&model.ProcessInstance{
				ID: "sp", ProcessName: "par", ProcessVersion: 1, Task: "fan",
				State: map[string]any{"outputs": map[string]any{}}, Status: model.StatusPaused,
			}); err != nil {
				t.Fatalf("SaveInstance: %v", err)
			}
			if err := upgradeTree(t, b.db, "sp", 99, 2); err == nil {
				t.Fatal("moved a tree whose root is not on the version the caller asserted")
			}
		})
	}
}

func TestUpgradeComposition_IsIdempotent(t *testing.T) {
	// A repeated run repairs a partial one, so a bulk operation interrupted halfway is
	// recoverable by running it again.
	for _, b := range testBackends(t) {
		t.Run(b.name, func(t *testing.T) {
			saveKidDef(t, b.db, 1)
			parentPinning(t, b.db, 1, 1)
			parentPinning(t, b.db, 2, 1)
			if err := b.db.SaveInstance(&model.ProcessInstance{
				ID: "ip", ProcessName: "par", ProcessVersion: 1, Task: "fan",
				State: map[string]any{"outputs": map[string]any{}}, Status: model.StatusPaused,
			}); err != nil {
				t.Fatalf("SaveInstance: %v", err)
			}
			if err := upgradeTree(t, b.db, "ip", 1, 2); err != nil {
				t.Fatalf("first move: %v", err)
			}
			after, err := b.db.GetInstance("ip")
			if err != nil {
				t.Fatalf("GetInstance: %v", err)
			}

			// Re-running must succeed AND touch nothing: every member is already on target,
			// so it is skipped before the migration rather than rewritten as a no-op.
			dbpkg.AdvanceClock(time.Second)
			if err := upgradeTree(t, b.db, "ip", 2, 2); err != nil {
				t.Fatalf("re-running an already-applied move failed: %v", err)
			}
			again, err := b.db.GetInstance("ip")
			if err != nil {
				t.Fatalf("GetInstance: %v", err)
			}
			if !again.UpdatedAt.Equal(after.UpdatedAt) {
				t.Errorf("re-running rewrote a row that had not moved (updated_at %v -> %v)", after.UpdatedAt, again.UpdatedAt)
			}
		})
	}
}

func TestPlanUpgrade_AFailedTreeMovesWithItsFailedChild(t *testing.T) {
	// `failed` is a state an upgrade is FOR: move it, then retry it on the new version. Retry
	// revives a failed child in place, so it moves too — left behind, the retry re-runs the very
	// version the fix was published to replace.
	for _, b := range testBackends(t) {
		t.Run(b.name, func(t *testing.T) {
			saveKidDef(t, b.db, 1)
			saveKidDef(t, b.db, 7)
			parentPinning(t, b.db, 1, 1) // par@1 spawns kid@1
			parentPinning(t, b.db, 2, 7) // par@2 spawns kid@7: the fix

			if err := b.db.SaveInstance(&model.ProcessInstance{
				ID: "fp", ProcessName: "par", ProcessVersion: 1, Task: "fan",
				State:  map[string]any{"outputs": map[string]any{}},
				Status: model.StatusFailed, ErrorMessage: "boom",
			}); err != nil {
				t.Fatalf("SaveInstance: %v", err)
			}
			if err := b.db.SaveInstance(&model.ProcessInstance{
				ID: "fk", ProcessName: "kid", ProcessVersion: 1, Task: "run",
				State:  map[string]any{"outputs": map[string]any{}},
				Status: model.StatusFailed, ParentID: "fp", SpawnTaskID: "fan",
			}); err != nil {
				t.Fatalf("SaveInstance: %v", err)
			}

			plan, err := b.db.PlanUpgrade(context.Background(), "fp", 2)
			if err != nil {
				t.Fatalf("PlanUpgrade on a failed root: %v", err)
			}
			got := map[string]int{}
			for _, m := range plan {
				got[m.Instance.ID] = m.ToVersion
			}
			if got["fp"] != 2 || got["fk"] != 7 {
				t.Fatalf("plan = %v, want fp→2 and fk→7: a failed child's target is the version par@2 names", got)
			}

			if err := upgradeTree(t, b.db, "fp", 1, 2); err != nil {
				t.Fatalf("upgradeTree on a failed tree: %v", err)
			}
			for id, want := range map[string]int{"fp": 2, "fk": 7} {
				inst, _ := b.db.GetInstance(id)
				if inst.ProcessVersion != want {
					t.Errorf("%s on version %d, want %d", id, inst.ProcessVersion, want)
				}
				if inst.Status != model.StatusFailed {
					t.Errorf("%s: status %q, want failed — an upgrade moves the version, not the state machine", id, inst.Status)
				}
			}

			// The retry that follows revives the child on the version it was moved to.
			if _, err := b.db.RetryProcess(context.Background(), "fp", false, ""); err != nil {
				t.Fatalf("RetryProcess: %v", err)
			}
			kid, _ := b.db.GetInstance("fk")
			if kid.Status != model.StatusRunning || kid.ProcessVersion != 7 {
				t.Errorf("revived child is %s on version %d, want running on 7", kid.Status, kid.ProcessVersion)
			}
		})
	}
}

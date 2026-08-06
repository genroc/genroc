package engine

import (
	"context"
	"fmt"
	"testing"
	"time"

	"genroc/internal/db"
	"genroc/internal/model"
)

// spawnFixture registers a one-task child process and a parent whose only task spawns it,
// then creates the parent instance and returns its id.
func spawnFixture(t *testing.T, database *db.DB, name string) string {
	t.Helper()
	child := fmt.Sprintf("%s-child-%d", name, time.Now().UnixNano())
	if err := database.SaveDefinition(&model.ProcessDefinition{
		Name:  child,
		Tasks: []*model.Task{{ID: "work", Switch: model.SwitchMap{{Goto: model.GotoEnd}}}},
	}, 1, nil, child+"-hash", ""); err != nil {
		t.Fatalf("SaveDefinition (child): %v", err)
	}

	parent := fmt.Sprintf("%s-parent-%d", name, time.Now().UnixNano())
	if err := database.SaveDefinition(&model.ProcessDefinition{
		Name: parent,
		Tasks: []*model.Task{{
			ID:     "fan",
			Action: &model.Action{Type: model.ActionTypeChild, Name: child},
			Switch: model.SwitchMap{{Goto: model.GotoEnd}},
		}},
	}, 1, nil, parent+"-hash", ""); err != nil {
		t.Fatalf("SaveDefinition (parent): %v", err)
	}

	id := fmt.Sprintf("%s-i-%d", name, time.Now().UnixNano())
	if err := database.SaveInstance(&model.ProcessInstance{
		ID: id, ProcessName: parent, ProcessVersion: 1,
		Task: "fan", ContextData: map[string]any{}, Status: model.StatusRunning,
	}); err != nil {
		t.Fatalf("SaveInstance: %v", err)
	}
	return id
}

// claimOne claims exactly the named instance for eng's worker and returns it.
func claimOne(t *testing.T, database *db.DB, eng *Engine, id string) *model.ProcessInstance {
	t.Helper()
	claimed, err := database.ClaimInstances(eng.WorkerID(), time.Minute, 10, db.AllowTakeover())
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	for _, c := range claimed {
		if c.ID == id {
			return c
		}
	}
	t.Fatalf("instance %s was not claimable", id)
	return nil
}

// The rule the outcome type exists to enforce: advance decides, persist writes. Spawn is the
// case that broke it — it committed mid-advance, releasing the lease while runAdvance still
// had the instance marked, and a claim in that window read as re-claiming live work.
func TestAdvance_SpawnWritesNothingUntilPersist(t *testing.T) {
	database := openTestDB(t)
	eng := tickEngine(t, database)
	id := spawnFixture(t, database, "spawn-pure")
	inst := claimOne(t, database, eng, id)

	outcome := eng.advanceGuarded(context.Background(), inst)
	if outcome.kind != outcomeSpawn {
		t.Fatalf("expected a spawn outcome, got kind %d", outcome.kind)
	}
	if len(outcome.children) != 1 {
		t.Fatalf("expected 1 child in the outcome, got %d", len(outcome.children))
	}

	// Nothing may exist yet: not the child, not the parent's park, not the released lease.
	if _, err := database.GetInstance(outcome.children[0].ID); err == nil {
		t.Error("advance created the child row itself; the batch must travel to persist")
	}
	before, err := database.GetInstance(id)
	if err != nil {
		t.Fatalf("GetInstance (pre-persist): %v", err)
	}
	if before.WaitState != model.WaitStateNone {
		t.Errorf("advance parked the parent itself (wait_state %q)", before.WaitState)
	}
	if before.WorkerID == nil {
		t.Error("advance released the lease itself; only persist may hand the instance on")
	}

	if err := eng.persist(context.Background(), inst, outcome); err != nil {
		t.Fatalf("persist: %v", err)
	}

	if _, err := database.GetInstance(outcome.children[0].ID); err != nil {
		t.Errorf("persist did not create the child: %v", err)
	}
	after, err := database.GetInstance(id)
	if err != nil {
		t.Fatalf("GetInstance (post-persist): %v", err)
	}
	if after.WaitState != model.WaitStateWaiting {
		t.Errorf("parent wait_state = %q, want %q", after.WaitState, model.WaitStateWaiting)
	}
	if after.WorkerID != nil {
		t.Errorf("persist left the lease held by %q", *after.WorkerID)
	}
}

// TestAdvance_ExternalArmWritesNothingUntilPersist is the same rule for the other path that
// used to write for itself — and the one where it mattered, because a parked external
// instance is claimable the moment it lands (a past-due timeout, or a signal that arrives
// straight after), unlike a spawn's parent which is parked out of the claim predicate.
func TestAdvance_ExternalArmWritesNothingUntilPersist(t *testing.T) {
	database := openTestDB(t)
	eng := tickEngine(t, database)

	process := fmt.Sprintf("arm-pure-%d", time.Now().UnixNano())
	if err := database.SaveDefinition(&model.ProcessDefinition{
		Name: process,
		Tasks: []*model.Task{{
			ID:     "wait",
			Action: &model.Action{Type: model.ActionTypeExternal},
			Switch: model.SwitchMap{{Goto: model.GotoEnd}},
		}},
	}, 1, nil, process+"-hash", ""); err != nil {
		t.Fatalf("SaveDefinition: %v", err)
	}
	id := fmt.Sprintf("arm-pure-i-%d", time.Now().UnixNano())
	if err := database.SaveInstance(&model.ProcessInstance{
		ID: id, ProcessName: process, ProcessVersion: 1,
		Task: "wait", ContextData: map[string]any{}, Status: model.StatusRunning,
	}); err != nil {
		t.Fatalf("SaveInstance: %v", err)
	}
	inst := claimOne(t, database, eng, id)

	outcome := eng.advanceGuarded(context.Background(), inst)
	if outcome.kind != outcomeArm {
		t.Fatalf("expected an arm outcome, got kind %d", outcome.kind)
	}
	if outcome.arm == nil || outcome.arm.taskID != "wait" {
		t.Fatalf("arm intent missing or wrong: %+v", outcome.arm)
	}

	before, err := database.GetInstance(id)
	if err != nil {
		t.Fatalf("GetInstance (pre-persist): %v", err)
	}
	if before.WaitState != model.WaitStateNone {
		t.Errorf("advance armed the wait itself (wait_state %q)", before.WaitState)
	}
	if before.WorkerID == nil {
		t.Error("advance released the lease itself; only persist may hand the instance on")
	}

	if err := eng.persist(context.Background(), inst, outcome); err != nil {
		t.Fatalf("persist: %v", err)
	}
	after, err := database.GetInstance(id)
	if err != nil {
		t.Fatalf("GetInstance (post-persist): %v", err)
	}
	if after.WaitState != model.WaitStateExternal {
		t.Errorf("wait_state = %q, want %q", after.WaitState, model.WaitStateExternal)
	}
	if after.WorkerID != nil {
		t.Errorf("persist left the lease held by %q", *after.WorkerID)
	}
}

// The verdict that moved out of advance with the write: spawning is part of the step, so a
// refused spawn is the instance's failure, not the worker's. Re-advancing an already-parked
// parent is exactly what SpawnChildrenAndWait refuses — what a double advance looks like.
func TestRunAdvance_SpawnFailureFailsTheInstance(t *testing.T) {
	database := openTestDB(t)
	eng := tickEngine(t, database)
	id := spawnFixture(t, database, "spawn-conflict")
	inst := claimOne(t, database, eng, id)

	if err := eng.runAdvance(context.Background(), inst); err != nil {
		t.Fatalf("first advance: %v", err)
	}

	// The same in-memory instance still reads as unparked, so advancing it again reaches
	// the spawn a second time — and the parent row is now 'waiting'.
	if err := eng.runAdvance(context.Background(), inst); err != nil {
		t.Fatalf("second advance returned the write error to the worker instead of failing the instance: %v", err)
	}

	got, err := database.GetInstance(id)
	if err != nil {
		t.Fatalf("GetInstance: %v", err)
	}
	if got.Status != model.StatusFailed {
		t.Fatalf("status = %q, want %q (a refused spawn is the instance's failure)", got.Status, model.StatusFailed)
	}
	if got.ErrorCode != "engine.spawn" {
		t.Errorf("error_code = %q, want engine.spawn", got.ErrorCode)
	}
	if got.WorkerID != nil {
		t.Errorf("the failure write left the lease held by %q", *got.WorkerID)
	}
}

// A signal that beat the process to the task is consumed by the same transaction that would
// have parked the instance — that atomicity is why a racing signal cannot be lost. The consume
// is an ordinary checkpoint: result durable, lease released, next claim reads it.
func TestPersistArm_ConsumeStoresResultAndReleases(t *testing.T) {
	database := openTestDB(t)
	eng := tickEngine(t, database)
	ctx := context.Background()

	process := fmt.Sprintf("arm-buffered-%d", time.Now().UnixNano())
	if err := database.SaveDefinition(&model.ProcessDefinition{
		Name: process,
		Tasks: []*model.Task{{
			ID:     "wait",
			Action: &model.Action{Type: model.ActionTypeExternal},
			Switch: model.SwitchMap{{Goto: model.GotoEnd}},
		}},
	}, 1, nil, process+"-hash", ""); err != nil {
		t.Fatalf("SaveDefinition: %v", err)
	}
	id := fmt.Sprintf("arm-buffered-i-%d", time.Now().UnixNano())
	if err := database.SaveInstance(&model.ProcessInstance{
		ID: id, ProcessName: process, ProcessVersion: 1,
		Task: "wait", ContextData: map[string]any{}, Status: model.StatusRunning,
	}); err != nil {
		t.Fatalf("SaveInstance: %v", err)
	}

	// The signal arrives first: nothing is armed, so it buffers.
	if delivered, err := database.DeliverSignal(ctx, id, "wait", "sig-1", map[string]any{"ok": true}); err != nil {
		t.Fatalf("DeliverSignal: %v", err)
	} else if delivered {
		t.Fatal("the signal was delivered to an unarmed task; it should have buffered")
	}

	inst := claimOne(t, database, eng, id)
	outcome := eng.advanceGuarded(ctx, inst)
	if outcome.kind != outcomeArm {
		t.Fatalf("expected an arm outcome, got kind %d", outcome.kind)
	}

	if err := eng.persist(ctx, inst, outcome); err != nil {
		t.Fatalf("persist: %v", err)
	}
	after, err := database.GetInstance(id)
	if err != nil {
		t.Fatalf("GetInstance: %v", err)
	}
	if _, ok := after.ContextData[model.CtxExternalResult]; !ok {
		t.Fatal("the consumed result is not on the row; a crash here would lose the signal")
	}
	if after.WorkerID != nil {
		t.Errorf("consuming kept the lease held by %q; every persist ends the work session", *after.WorkerID)
	}
	if after.WaitState != model.WaitStateNone {
		t.Errorf("consuming parked the instance (wait_state %q); it must stay claimable", after.WaitState)
	}

	// The next claim — a fresh session, not a continuation — reads the stored result via
	// runExternal phase 2 and finishes the process.
	next := claimOne(t, database, eng, id)
	if next.ReclaimedExpired {
		t.Fatal("the post-consume claim read as a takeover; the consume must release the lease cleanly")
	}
	if err := eng.runAdvance(ctx, next); err != nil {
		t.Fatalf("second session: %v", err)
	}
	got, err := database.GetInstance(id)
	if err != nil {
		t.Fatalf("GetInstance: %v", err)
	}
	if got.Status != model.StatusCompleted {
		t.Fatalf("status = %q, want %q (%s: %s)", got.Status, model.StatusCompleted, got.ErrorCode, got.Error)
	}
}

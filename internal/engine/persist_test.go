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

// TestAdvance_SpawnWritesNothingUntilPersist is the rule the outcome type exists to enforce:
// advance decides, persist writes. A child spawn is the case that used to break it — it
// committed its own transaction mid-advance, which released the lease while runAdvance still
// had the instance marked in flight, and a claim landing in that window read as this worker
// re-claiming live work and took it down.
func TestAdvance_SpawnWritesNothingUntilPersist(t *testing.T) {
	database := openTestDB(t)
	eng := tickEngine(t, database)
	id := spawnFixture(t, database, "spawn-pure")
	inst := claimOne(t, database, eng, id)

	outcome := eng.advanceGuarded(context.Background(), inst, false)
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

	if _, err := eng.persist(context.Background(), inst, outcome); err != nil {
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

	outcome := eng.advanceGuarded(context.Background(), inst, false)
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

	again, err := eng.persist(context.Background(), inst, outcome)
	if err != nil {
		t.Fatalf("persist: %v", err)
	}
	if again {
		t.Error("persist asked for another pass with no buffered signal to consume")
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

// TestRunAdvance_SpawnFailureFailsTheInstance covers the verdict that moved out of advance
// with the write: spawning is part of the step, so a spawn the database refuses is the
// instance's failure and not the worker's. Re-advancing a parent that is already parked is
// exactly what SpawnChildrenAndWait refuses, and it is reachable — it is what a double
// advance of one instance looks like.
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

// TestPersistArm_ConsumesBufferedSignalAndAsksForAnotherPass covers the one outcome that
// keeps the lease. A signal that reached the task before the process did is consumed by the
// same transaction that would otherwise have parked the instance — that atomicity is what
// makes a signal racing the arm impossible to lose — so the result comes back in memory for
// a second advance pass rather than through a second claim.
func TestPersistArm_ConsumesBufferedSignalAndAsksForAnotherPass(t *testing.T) {
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
	outcome := eng.advanceGuarded(ctx, inst, false)
	if outcome.kind != outcomeArm {
		t.Fatalf("expected an arm outcome, got kind %d", outcome.kind)
	}

	again, err := eng.persist(ctx, inst, outcome)
	if err != nil {
		t.Fatalf("persist: %v", err)
	}
	if !again {
		t.Fatal("persist consumed the buffered signal but did not ask for the pass that uses it")
	}
	if _, ok := inst.ContextData[model.CtxExternalResult]; !ok {
		t.Fatal("the consumed result was not handed back in memory; the next pass has nothing to read")
	}
	if got, err := database.GetInstance(id); err != nil {
		t.Fatalf("GetInstance: %v", err)
	} else if got.WorkerID == nil {
		t.Error("consuming released the lease; the instance is still being advanced here")
	}

	// The second pass is what runAdvance would run, and it must finish the process.
	if _, err := eng.persist(ctx, inst, eng.advanceGuarded(ctx, inst, true)); err != nil {
		t.Fatalf("persist (second pass): %v", err)
	}
	got, err := database.GetInstance(id)
	if err != nil {
		t.Fatalf("GetInstance: %v", err)
	}
	if got.Status != model.StatusCompleted {
		t.Fatalf("status = %q, want %q (%s: %s)", got.Status, model.StatusCompleted, got.ErrorCode, got.Error)
	}
}

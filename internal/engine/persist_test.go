package engine

import (
	"context"
	"fmt"
	"testing"
	"time"

	"genroc/internal/db"
	"genroc/internal/idgen"
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

	// A real UUID, not a readable string: idgen.ChildBase falls back to a random v7 for
	// anything unparseable, and one test needs the child id to be derivable in advance.
	id := idgen.New()
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
// refused spawn is the instance's failure, not the worker's. SpawnChildrenAndWait reads
// wait_state from the ROW inside its transaction, so parking the row on 'external' (with an
// expired deadline, which keeps it claimable) and clearing the in-memory copy makes the
// spawn refuse. The refusal rolls its transaction back, which is what leaves the lease
// held — and holding it is now what lets the failure write land at all.
func TestRunAdvance_SpawnFailureFailsTheInstance(t *testing.T) {
	database := openTestDB(t)
	eng := tickEngine(t, database)
	id := spawnFixture(t, database, "spawn-conflict")

	parked, err := database.GetInstance(id)
	if err != nil {
		t.Fatalf("GetInstance: %v", err)
	}
	past := db.Now().Add(-time.Minute)
	parked.WaitState = model.WaitStateExternal
	parked.WakeAt = &past
	if err := database.UpdateInstance(parked); err != nil {
		t.Fatalf("park the row: %v", err)
	}

	inst := claimOne(t, database, eng, id)
	inst.WaitState = model.WaitStateNone // the row is parked; this worker's copy is not
	if err := eng.runAdvance(context.Background(), inst); err != nil {
		t.Fatalf("advance returned the write error to the worker instead of failing the instance: %v", err)
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

// The other half of the same branch: once the advance has persisted, the lease is
// released, and a second advance off the same in-memory instance holds no grant. Its
// spawn is refused, and the failure write that would follow is refused too — so the
// instance stays running for its next claim rather than being failed by a doubled
// advance. Before worker_id joined the fence this wrote through, because releasing a
// lease does not move the epoch. specs/durability-levels.md §7.
func TestRunAdvance_DoubledAdvanceCannotFailTheInstance(t *testing.T) {
	database := openTestDB(t)
	eng := tickEngine(t, database)
	id := spawnFixture(t, database, "spawn-doubled")
	inst := claimOne(t, database, eng, id)

	if err := eng.runAdvance(context.Background(), inst); err != nil {
		t.Fatalf("first advance: %v", err)
	}
	// The same in-memory instance still reads as unparked, so advancing it again reaches
	// the spawn a second time — and the parent row is now 'waiting' and unleased.
	if err := eng.runAdvance(context.Background(), inst); err != nil {
		t.Fatalf("second advance returned an error to the worker: %v", err)
	}

	got, err := database.GetInstance(id)
	if err != nil {
		t.Fatalf("GetInstance: %v", err)
	}
	if got.Status != model.StatusRunning {
		t.Fatalf("status = %q, want %q — a doubled advance holds no grant and must not fail the row", got.Status, model.StatusRunning)
	}
	if got.ErrorCode != "" {
		t.Errorf("error_code = %q, want empty", got.ErrorCode)
	}
}

// A signal that beat the process to the task is consumed on ARRIVAL at the task, in one advance:
// runExternal checks the buffer before it arms, so the instance never parks and no second claim
// is needed. The old shape consumed it in the arm and yielded, costing a claim cycle.
//
// The arm's own not-parking branch still exists and is not this: it is the race where a signal
// lands between this check and the park write, and it is covered at the DB level
// (TestSignals_BufferThenConsumeFIFO). specs/external-outcome-as-signal.md.
func TestExternal_BufferedAnswerIsConsumedWithoutParking(t *testing.T) {
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
	if delivered, err := database.DeliverSignal(ctx, id, "wait", "sig-1", model.ExternalOutcome{Result: map[string]any{"ok": true}}); err != nil {
		t.Fatalf("DeliverSignal: %v", err)
	} else if delivered {
		t.Fatal("the signal was delivered to an unarmed task; it should have buffered")
	}

	// ONE advance: the buffered answer is consumed and the process finishes. Assert the outcome
	// kind, not just the end state — a consume-then-yield implementation reaches `completed` too,
	// one claim cycle later, and that cost is the whole reason this is written this way.
	inst := claimOne(t, database, eng, id)
	outcome := eng.advanceGuarded(ctx, inst)
	if outcome.kind == outcomeArm {
		t.Fatal("the instance parked with an answer already buffered; that costs a claim cycle on every external task")
	}
	if err := eng.persist(ctx, inst, outcome); err != nil {
		t.Fatalf("persist: %v", err)
	}
	got, err := database.GetInstance(id)
	if err != nil {
		t.Fatalf("GetInstance: %v", err)
	}
	if got.Status != model.StatusCompleted {
		t.Fatalf("status = %q, want %q (%s: %s)", got.Status, model.StatusCompleted, got.ErrorCode, got.Error)
	}
	if n, err := database.CountBufferedSignals(id, "wait"); err != nil || n != 0 {
		t.Fatalf("buffered = %d (err=%v), want 0 — the consuming write must delete it", n, err)
	}
}

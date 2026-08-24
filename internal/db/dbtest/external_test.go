package dbtest

import (
	"context"
	"testing"
	"time"

	dbpkg "genroc/internal/db"
	"genroc/internal/model"
)

// insertExternalParked saves an instance parked on an external task: status=running,
// wait_state='external', with the _external {task_id, input} snapshot. The occurrence a
// resolve must match is task_epoch on the row, not anything inside _external.
// wakeAt is the (optional) timeout deadline.
func insertExternalParked(t *testing.T, db *dbpkg.DB, id string, epoch int64, wakeAt *time.Time) {
	t.Helper()
	inst := &model.ProcessInstance{
		ID:             id,
		ProcessName:    "test",
		ProcessVersion: 1,
		Task:           "approval",
		ContextData: map[string]any{
			model.CtxExternal: map[string]any{
				"task_id": "approval",
				"input":   map[string]any{"order_id": float64(42)},
			},
		},
		Status:    model.StatusRunning,
		WaitState: model.WaitStateExternal,
		WakeAt:    wakeAt,
		TaskEpoch: epoch,
	}
	if err := db.SaveInstance(inst); err != nil {
		t.Fatalf("SaveInstance: %v", err)
	}
}

// TestResolveExternalTask covers the exact-occurrence epoch check, the successful
// resolve (result stored + un-parked), and double-submit rejection, on both engines.
func TestResolveExternalTask(t *testing.T) {
	for _, b := range testBackends(t) {
		t.Run(b.name, func(t *testing.T) {
			ctx := context.Background()
			const epoch = int64(3)
			insertExternalParked(t, b.db, "inst-ext", epoch, nil)

			// A result submitted against a DIFFERENT arming is rejected; the task stays parked.
			if err := b.db.ResolveExternalTask(ctx, "inst-ext", epoch-1, dbpkg.Unclaimed, model.ExternalOutcome{Result: map[string]any{"approved": true}}); err == nil {
				t.Fatal("expected a prior-occurrence resolve to fail")
			}
			if got, _ := b.db.GetInstance("inst-ext"); got.WaitState != model.WaitStateExternal {
				t.Fatalf("a prior-occurrence resolve should leave it parked, got wait_state %q", got.WaitState)
			}

			// The current occurrence resolves: result stored, instance un-parked.
			if err := b.db.ResolveExternalTask(ctx, "inst-ext", epoch, dbpkg.Unclaimed, model.ExternalOutcome{Result: map[string]any{"approved": true}}); err != nil {
				t.Fatalf("ResolveExternalTask: %v", err)
			}
			got, err := b.db.GetInstance("inst-ext")
			if err != nil {
				t.Fatalf("GetInstance: %v", err)
			}
			if got.WaitState != model.WaitStateNone {
				t.Fatalf("expected wait_state cleared, got %q", got.WaitState)
			}
			if got.WakeAt != nil {
				t.Fatalf("expected wake_at cleared, got %v", got.WakeAt)
			}
			res, ok := got.ContextData[model.CtxExternalResult].(map[string]any)
			if !ok || res["approved"] != true {
				t.Fatalf("expected _external_result {approved:true}, got %#v", got.ContextData[model.CtxExternalResult])
			}

			// A second submit is rejected: the task is no longer waiting.
			if err := b.db.ResolveExternalTask(ctx, "inst-ext", epoch, dbpkg.Unclaimed, model.ExternalOutcome{Result: map[string]any{"approved": false}}); err == nil {
				t.Fatal("expected double resolve to fail")
			}
		})
	}
}

// TestResolveExternalTask_RejectsWhenLeased verifies the resolve loses to a timeout
// claim already in flight: once a worker has leased the (due) external instance, a
// submit racing the timeout's advance is rejected rather than overwriting it.
func TestResolveExternalTask_RejectsWhenLeased(t *testing.T) {
	for _, b := range testBackends(t) {
		t.Run(b.name, func(t *testing.T) {
			ctx := context.Background()
			const epoch = int64(1)
			past := time.Now().Add(-time.Minute)
			insertExternalParked(t, b.db, "inst-to", epoch, &past) // timeout already due

			// A worker claims it (the timeout firing) -> a live lease.
			claimed, err := b.db.ClaimInstances("worker-timeout", 30*time.Second, 10, dbpkg.AllowTakeover())
			if err != nil {
				t.Fatalf("ClaimInstances: %v", err)
			}
			if len(claimed) != 1 {
				t.Fatalf("expected the due external instance to be claimable, got %d", len(claimed))
			}

			// Resolve now races the in-flight timeout claim and must lose.
			if err := b.db.ResolveExternalTask(ctx, "inst-to", epoch, dbpkg.Unclaimed, model.ExternalOutcome{Result: map[string]any{"approved": true}}); err == nil {
				t.Fatal("expected resolve to be rejected while the instance is leased")
			}
		})
	}
}

// TestClaim_ExternalNoTimeoutNotClaimable verifies a no-timeout external wait (wake_at
// NULL) is never returned by the claim, while a due-timeout one is.
func TestClaim_ExternalNoTimeoutNotClaimable(t *testing.T) {
	for _, b := range testBackends(t) {
		t.Run(b.name, func(t *testing.T) {
			insertExternalParked(t, b.db, "inst-wait", 0, nil) // no timeout
			past := time.Now().Add(-time.Minute)
			insertExternalParked(t, b.db, "inst-due", 0, &past) // timeout due

			claimed, err := b.db.ClaimInstances("worker-x", 10*time.Second, 10, dbpkg.AllowTakeover())
			if err != nil {
				t.Fatalf("ClaimInstances: %v", err)
			}
			if len(claimed) != 1 || claimed[0].ID != "inst-due" {
				ids := make([]string, len(claimed))
				for i, c := range claimed {
					ids[i] = c.ID
				}
				t.Fatalf("expected only the due-timeout external instance claimable, got %v", ids)
			}
		})
	}
}

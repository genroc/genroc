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
		State: map[string]any{
			model.StateExternal: map[string]any{
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

			// The current occurrence resolves: the outcome is BUFFERED and the instance un-parked.
			// It never lands on the row -- the engine pops it under lease and writes it through
			// the ordinary context encode. specs/external-outcome-as-signal.md.
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
			if c, _ := b.db.CountBufferedSignals("inst-ext", "approval"); c != 1 {
				t.Fatalf("expected the outcome buffered, got %d", c)
			}
			_, outcome, ok, err := b.db.PeekSignal("inst-ext", "approval")
			if err != nil || !ok {
				t.Fatalf("peek: ok=%v err=%v", ok, err)
			}
			res, _ := outcome.Result.(map[string]any)
			if res["approved"] != true {
				t.Fatalf("expected the buffered result {approved:true}, got %#v", outcome.Result)
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

// A large submitted outcome arrives CUT: externalized, declared in the instance's objects and
// claimed, exactly like a value the process produced itself.
//
// It could not be, before. The resolve API holds only the instance row lock and has no reference
// set to reconcile, so it wrote the outcome onto the row inline whatever its size -- no cut, no
// `objects` entry, no claim -- and it stayed that way until some later full write tidied it.
// Routed through the buffer, the engine pops it under lease and writes it through the ordinary
// context encode, which is the only path that can do any of that.
// specs/external-outcome-as-signal.md.
func TestResolveExternalTask_LargeOutcomeIsCutWhenConsumed(t *testing.T) {
	for _, b := range testBackends(t) {
		t.Run(b.name, func(t *testing.T) {
			ctx := context.Background()
			big := bigString("submitted")
			insertExternalParked(t, b.db, "inst-big", 0, nil)

			if err := b.db.ResolveExternalTask(ctx, "inst-big", 0, dbpkg.Unclaimed,
				model.ExternalOutcome{Result: map[string]any{"payload": big}}); err != nil {
				t.Fatalf("ResolveExternalTask: %v", err)
			}

			// Still only in the buffer: the row carries nothing yet, which is the point.
			row, err := b.db.GetInstance("inst-big")
			if err != nil {
				t.Fatalf("GetInstance: %v", err)
			}
			for _, ref := range row.LoadedObjectHashes {
				t.Fatalf("the resolve wrote an object onto the instance: %v", ref)
			}

			// The engine consumes it: peek, act, and let the write carry the delete.
			id, outcome, ok, err := b.db.PeekSignal("inst-big", "approval")
			if err != nil || !ok {
				t.Fatalf("peek: ok=%v err=%v", ok, err)
			}
			res, _ := outcome.Result.(map[string]any)
			row.State["outputs"] = map[string]any{"wait": res}
			row.ConsumedSignalID = id
			row.WaitState = model.WaitStateNone
			if err := b.db.UpdateInstanceProgress(row); err != nil {
				t.Fatalf("consume write: %v", err)
			}

			// Cut, declared and claimed -- and still the value it was submitted as.
			after, err := b.db.GetInstance("inst-big")
			if err != nil {
				t.Fatalf("GetInstance: %v", err)
			}
			out := after.State["outputs"].(map[string]any)["wait"].(map[string]any)
			ref, isRef := out["payload"].(*model.ObjectRef)
			if !isRef {
				t.Fatalf("the consumed outcome was stored inline (%T); a submitted result is cut like any other value", out["payload"])
			}
			if n, err := b.db.CountObjectRefs(ref.Ref); err != nil || n != 1 {
				t.Fatalf("claims on the outcome's object = %d (err=%v), want 1", n, err)
			}
			v, err := b.db.ResolveObject(ctx, ref)
			if err != nil || v != big {
				t.Fatalf("the outcome's content did not survive the cut: err=%v", err)
			}
			if c, _ := b.db.CountBufferedSignals("inst-big", "approval"); c != 0 {
				t.Fatalf("the consuming write did not delete the signal: %d buffered", c)
			}
		})
	}
}

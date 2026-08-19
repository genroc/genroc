package dbtest

import (
	"context"
	"testing"

	dbpkg "genroc/internal/db"
	"genroc/internal/model"
)

// insertExternalRunning saves a running instance sitting at (but not yet armed on) an
// external task — wait_state empty, no _external snapshot. A signal delivered now buffers.
func insertExternalRunning(t *testing.T, db *dbpkg.DB, id string) {
	t.Helper()
	inst := &model.ProcessInstance{
		ID:             id,
		ProcessName:    "test",
		ProcessVersion: 1,
		Task:           "approval",
		ContextData:    map[string]any{},
		Status:         model.StatusRunning,
	}
	if err := db.SaveInstance(inst); err != nil {
		t.Fatalf("SaveInstance: %v", err)
	}
}

func n(v any) float64 {
	m, _ := v.(map[string]any)
	f, _ := m["n"].(float64)
	return f
}

// TestSignals_BufferThenConsumeFIFO covers the push/early case: signals delivered before
// the task arms are buffered and consumed in FIFO order, one per arming, then the task
// parks once the buffer drains. Runs on both engines.
func TestSignals_BufferThenConsumeFIFO(t *testing.T) {
	for _, b := range testBackends(t) {
		t.Run(b.name, func(t *testing.T) {
			ctx := context.Background()
			insertExternalRunning(t, b.db, "inst-sig")

			// Two signals arrive before the task is armed -> both buffered.
			d1, err := b.db.DeliverSignal(ctx, "inst-sig", "approval", "s1", map[string]any{"n": 1})
			if err != nil || d1 {
				t.Fatalf("deliver s1: delivered=%v err=%v (want buffered)", d1, err)
			}
			d2, _ := b.db.DeliverSignal(ctx, "inst-sig", "approval", "s2", map[string]any{"n": 2})
			if d2 {
				t.Fatal("deliver s2 should buffer, not deliver")
			}
			if c, _ := b.db.CountBufferedSignals("inst-sig", "approval"); c != 2 {
				t.Fatalf("expected 2 buffered, got %d", c)
			}

			inst, err := b.db.GetInstance("inst-sig")
			if err != nil {
				t.Fatalf("GetInstance: %v", err)
			}

			// First arming consumes the oldest (FIFO).
			consumed, result, err := b.db.ArmExternalOrConsumeSignal(ctx, inst, "approval", map[string]any{}, nil)
			if err != nil || !consumed {
				t.Fatalf("arm 1: consumed=%v err=%v (want consumed)", consumed, err)
			}
			if n(result) != 1 {
				t.Fatalf("FIFO: expected first signal n=1, got %v", result)
			}

			// Second arming consumes the next.
			consumed, result, err = b.db.ArmExternalOrConsumeSignal(ctx, inst, "approval", map[string]any{}, nil)
			if err != nil || !consumed || n(result) != 2 {
				t.Fatalf("arm 2: consumed=%v result=%v err=%v (want consumed n=2)", consumed, result, err)
			}

			// Buffer drained -> the next arming parks.
			consumed, _, err = b.db.ArmExternalOrConsumeSignal(ctx, inst, "approval", map[string]any{"in": "x"}, nil)
			if err != nil || consumed {
				t.Fatalf("arm 3: consumed=%v err=%v (want parked)", consumed, err)
			}
			got, _ := b.db.GetInstance("inst-sig")
			if got.WaitState != model.WaitStateExternal {
				t.Fatalf("expected parked (wait_state external), got %q", got.WaitState)
			}
			if c, _ := b.db.CountBufferedSignals("inst-sig", "approval"); c != 0 {
				t.Fatalf("expected 0 buffered after draining, got %d", c)
			}
		})
	}
}

// TestSignals_ResolveWhenArmed covers the case where the task is already parked: the
// signal resolves it directly instead of buffering.
func TestSignals_ResolveWhenArmed(t *testing.T) {
	for _, b := range testBackends(t) {
		t.Run(b.name, func(t *testing.T) {
			ctx := context.Background()
			insertExternalParked(t, b.db, "inst-armed", 0, nil)

			delivered, err := b.db.DeliverSignal(ctx, "inst-armed", "approval", "s1", map[string]any{"approved": true})
			if err != nil || !delivered {
				t.Fatalf("deliver to armed task: delivered=%v err=%v (want delivered)", delivered, err)
			}
			if c, _ := b.db.CountBufferedSignals("inst-armed", "approval"); c != 0 {
				t.Fatalf("an armed delivery should not buffer, got %d buffered", c)
			}
			got, _ := b.db.GetInstance("inst-armed")
			if got.WaitState != model.WaitStateNone {
				t.Fatalf("expected un-parked, got wait_state %q", got.WaitState)
			}
			res, ok := got.ContextData[model.CtxExternalResult].(map[string]any)
			if !ok || res["approved"] != true {
				t.Fatalf("expected _external_result {approved:true}, got %#v", got.ContextData[model.CtxExternalResult])
			}
		})
	}
}

// TestSignals_RejectsNonRunning verifies a signal to a terminal instance is refused.
func TestSignals_RejectsNonRunning(t *testing.T) {
	for _, b := range testBackends(t) {
		t.Run(b.name, func(t *testing.T) {
			ctx := context.Background()
			insertExternalRunning(t, b.db, "inst-done")
			// Drive it terminal via cancel path: simplest is to mark it completed directly.
			inst, _ := b.db.GetInstance("inst-done")
			inst.Status = model.StatusCompleted
			if err := b.db.UpdateInstance(inst); err != nil {
				t.Fatalf("UpdateInstance: %v", err)
			}
			if _, err := b.db.DeliverSignal(ctx, "inst-done", "approval", "s1", map[string]any{"n": 1}); err == nil {
				t.Fatal("expected signal to a completed instance to be rejected")
			}
		})
	}
}

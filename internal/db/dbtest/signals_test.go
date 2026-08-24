package dbtest

import (
	"context"
	"encoding/json"
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

// n reads the marker off a consumed outcome. json.Number, not float64: buffered signals now
// decode through internal/numeric, so an integer keeps its exact literal instead of being
// collapsed — the corruption that decoder exists to prevent, on a path that used to skip it.
func n(o model.ExternalOutcome) float64 {
	m, _ := o.Result.(map[string]any)
	num, ok := m["n"].(json.Number)
	if !ok {
		return -1
	}
	f, err := num.Float64()
	if err != nil {
		return -1
	}
	return f
}

// TestSignals_BufferThenConsumeFIFO covers the push/early case: signals delivered before the task
// arms are buffered, the arm declines to park while any is waiting, and they are consumed in FIFO
// order -- one per advance, each delete riding the write that acted on it. Runs on both engines.
func TestSignals_BufferThenConsumeFIFO(t *testing.T) {
	for _, b := range testBackends(t) {
		t.Run(b.name, func(t *testing.T) {
			ctx := context.Background()
			insertExternalRunning(t, b.db, "inst-sig")

			// Two signals arrive before the task is armed -> both buffered.
			d1, err := b.db.DeliverSignal(ctx, "inst-sig", "approval", "s1", model.ExternalOutcome{Result: map[string]any{"n": 1}})
			if err != nil || d1 {
				t.Fatalf("deliver s1: delivered=%v err=%v (want buffered)", d1, err)
			}
			d2, _ := b.db.DeliverSignal(ctx, "inst-sig", "approval", "s2", model.ExternalOutcome{Result: map[string]any{"n": 2}})
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

			// The arm no longer consumes: with an answer already buffered it declines to park
			// and leaves the row claimable, so the engine's phase 2 pops it.
			// specs/external-outcome-as-signal.md.
			armed, err := b.db.ArmExternalUnlessSignalled(ctx, inst, "approval", map[string]any{}, nil)
			if err != nil || armed {
				t.Fatalf("arm 1: armed=%v err=%v (want not parked -- an answer is waiting)", armed, err)
			}
			if c, _ := b.db.CountBufferedSignals("inst-sig", "approval"); c != 2 {
				t.Fatalf("the arm consumed a signal: %d buffered, want 2", c)
			}
			if got, _ := b.db.GetInstance("inst-sig"); got.WaitState == model.WaitStateExternal {
				t.Fatal("the arm parked despite a buffered answer -- nothing would wake it")
			}

			// Consuming is peek-then-write, and the delete rides the write: FIFO order, one per
			// advance.
			consume := func(want float64) {
				t.Helper()
				id, outcome, ok, err := b.db.PeekSignal("inst-sig", "approval")
				if err != nil || !ok {
					t.Fatalf("peek: ok=%v err=%v", ok, err)
				}
				if n(outcome) != want {
					t.Fatalf("FIFO: peeked n=%v, want %v", n(outcome), want)
				}
				cur, err := b.db.GetInstance("inst-sig")
				if err != nil {
					t.Fatalf("GetInstance: %v", err)
				}
				cur.ConsumedSignalID = id
				if err := b.db.UpdateInstanceProgress(cur); err != nil {
					t.Fatalf("consume write: %v", err)
				}
			}
			consume(1)
			if c, _ := b.db.CountBufferedSignals("inst-sig", "approval"); c != 1 {
				t.Fatalf("the consuming write did not delete the signal: %d buffered, want 1", c)
			}
			consume(2)

			// Buffer drained -> the next arming parks.
			inst, _ = b.db.GetInstance("inst-sig")
			armed, err = b.db.ArmExternalUnlessSignalled(ctx, inst, "approval", map[string]any{"in": "x"}, nil)
			if err != nil || !armed {
				t.Fatalf("arm 3: armed=%v err=%v (want parked)", armed, err)
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

// TestSignals_ResolveWhenArmed covers the case where the task is already parked. The outcome is
// buffered like every other -- `delivered` reports that the instance was also UN-PARKED, so the
// engine reaches it now rather than at the next arm. specs/external-outcome-as-signal.md.
func TestSignals_ResolveWhenArmed(t *testing.T) {
	for _, b := range testBackends(t) {
		t.Run(b.name, func(t *testing.T) {
			ctx := context.Background()
			insertExternalParked(t, b.db, "inst-armed", 0, nil)

			delivered, err := b.db.DeliverSignal(ctx, "inst-armed", "approval", "s1", model.ExternalOutcome{Result: map[string]any{"approved": true}})
			if err != nil || !delivered {
				t.Fatalf("deliver to armed task: delivered=%v err=%v (want delivered)", delivered, err)
			}
			if c, _ := b.db.CountBufferedSignals("inst-armed", "approval"); c != 1 {
				t.Fatalf("an armed delivery must still buffer, got %d buffered", c)
			}
			got, _ := b.db.GetInstance("inst-armed")
			if got.WaitState != model.WaitStateNone {
				t.Fatalf("expected un-parked, got wait_state %q", got.WaitState)
			}
			_, outcome, ok, err := b.db.PeekSignal("inst-armed", "approval")
			if err != nil || !ok {
				t.Fatalf("peek: ok=%v err=%v", ok, err)
			}
			res, _ := outcome.Result.(map[string]any)
			if res["approved"] != true {
				t.Fatalf("expected the buffered result {approved:true}, got %#v", outcome.Result)
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
			if _, err := b.db.DeliverSignal(ctx, "inst-done", "approval", "s1", model.ExternalOutcome{Result: map[string]any{"n": 1}}); err == nil {
				t.Fatal("expected signal to a completed instance to be rejected")
			}
		})
	}
}

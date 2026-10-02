package api

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"genroc/internal/db"
	"genroc/internal/model"
)

// Go rather than e2e: the fixture is a lease that expired under a worker, which no endpoint makes.
func TestUpgrade_ExplainsALeaseLeftByAnExpiredWorker(t *testing.T) {
	h, cleanup := newTestHandlers(t)
	defer cleanup()
	ctx := context.Background()

	def := func(note string) map[string]any {
		return map[string]any{
			"name":         "leased",
			"input_schema": map[string]any{"type": "object", "properties": map[string]any{note: map[string]any{"type": "string"}}},
			"tasks":        []any{map[string]any{"id": "hold", "action": map[string]any{"type": "external"}, "switch": "end"}},
		}
	}
	if r := batchApply(h, "latest", def("a")); !r.OK {
		t.Fatalf("apply v1: %s", r.Error)
	}
	inst := &model.ProcessInstance{
		ID: "leased-1", ProcessName: "leased", ProcessVersion: 1, Task: "hold",
		State:  map[string]any{"input": map[string]any{}, "outputs": map[string]any{}, model.StateLastError: nil},
		Status: model.StatusRunning,
	}
	if err := h.db.SaveInstance(inst); err != nil {
		t.Fatalf("SaveInstance: %v", err)
	}
	if _, err := h.db.ClaimInstances("gone-worker", time.Minute, 10, db.AllowTakeover()); err != nil {
		t.Fatalf("claim: %v", err)
	}
	db.AdvanceClock(2 * time.Minute)
	if _, err := h.db.PauseProcess(ctx, inst.ID, "test"); err != nil {
		t.Fatalf("pause: %v", err)
	}
	paused, err := h.db.GetInstance(inst.ID)
	if err != nil {
		t.Fatal(err)
	}
	if paused.Status != model.StatusPaused || paused.WorkerID == nil {
		t.Fatalf("fixture: status=%s worker_id=%v, want paused with the expired worker still recorded", paused.Status, paused.WorkerID)
	}
	if r := batchApply(h, "latest", def("b")); !r.OK {
		t.Fatalf("apply v2: %s", r.Error)
	}

	r := h.upgradeInstance(inst.ID, json.RawMessage(`{"to_version":2}`), "test")
	if !r.OK {
		t.Fatalf("upgrade answered %q; a recorded lease is a refusal with a reason, not a race lost to a concurrent change", r.Error)
	}
	var resp UpgradeResp
	if err := json.Unmarshal(r.Data, &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Upgraded || len(resp.Moves) != 1 {
		t.Fatalf("resp = %+v, want one refused move", resp)
	}
	if reason := resp.Moves[0].Reason; !strings.Contains(reason, "lease") || !strings.Contains(reason, "resume") {
		t.Errorf("reason = %q; it must say a worker's lease is recorded and that resuming clears it", reason)
	}
	after, err := h.db.GetInstance(inst.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.ProcessVersion != 1 || after.WorkerID == nil {
		t.Errorf("version=%d worker_id=%v; the refusal must leave the row, worker_id included, untouched",
			after.ProcessVersion, after.WorkerID)
	}
}

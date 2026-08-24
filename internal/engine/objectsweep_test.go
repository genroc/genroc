package engine

import (
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"genroc/internal/db"
	"genroc/internal/model"
)

// TestCollectObjects_RunsWithLogRetentionDisabled pins a coupling that was harmless until it
// was not. The object sweep used to live inside pruneLogs(), which returns early when log
// retention is disabled ("keep logs forever"). That cost nothing while a released object was
// deleted on the spot; once releases only leave a grace claim and the sweep is what collects
// them, the same early return means a run with `--log-retention 0` never collects anything and
// grows without bound.
//
// LogConfig{} below is exactly that configuration — retention zero — which is why the assertion
// is worth making here rather than in a run that happens to have retention on.
func TestCollectObjects_RunsWithLogRetentionDisabled(t *testing.T) {
	database := openTestDB(t)
	database.SetObjectGrace(0) // released content is collectable at once
	eng := New(database, 0 /* manual */, 1, true, 0, 0, LogConfig{}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if eng.logCfg.Retention != 0 {
		t.Fatal("this test is only meaningful with log retention disabled")
	}

	big := strings.Repeat("x", 10*1024) // over the externalization threshold
	inst := &model.ProcessInstance{
		ID: "sweep-1", ProcessName: "test", Status: model.StatusRunning,
		ContextData: map[string]any{"outputs": map[string]any{"out": big}},
	}
	if err := database.SaveInstance(inst); err != nil {
		t.Fatalf("SaveInstance: %v", err)
	}
	reloaded, err := database.GetInstance("sweep-1")
	if err != nil {
		t.Fatalf("GetInstance: %v", err)
	}
	ref, ok := reloaded.ContextData["outputs"].(map[string]any)["out"].(*model.ObjectRef)
	if !ok {
		t.Fatalf("the big output was not externalized")
	}

	// Release it, then sweep.
	reloaded.ContextData["outputs"].(map[string]any)["out"] = "small"
	if err := database.UpdateInstanceProgress(reloaded); err != nil {
		t.Fatalf("UpdateInstanceProgress: %v", err)
	}
	db.AdvanceClock(time.Second) // past the zero-length grace window
	eng.collectObjects()

	if n, err := database.CountObjectRefs(ref.Ref); err != nil {
		t.Fatalf("CountObjectRefs: %v", err)
	} else if n != 0 {
		t.Fatalf("%d claim(s) survived the sweep", n)
	}
	if _, _, err := database.GetObjectContent(ref.Ref); err == nil {
		t.Fatal("released content survived a sweep — the object sweep is gated on log retention again")
	}
}

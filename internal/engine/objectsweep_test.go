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

// LogConfig{} is `--log-retention 0`, where pruneLogs returns early: a sweep inside it
// would never collect.
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
		State: map[string]any{"outputs": map[string]any{"out": big}},
	}
	if err := database.SaveInstance(inst); err != nil {
		t.Fatalf("SaveInstance: %v", err)
	}
	reloaded, err := database.GetInstance("sweep-1")
	if err != nil {
		t.Fatalf("GetInstance: %v", err)
	}
	ref, ok := reloaded.State["outputs"].(map[string]any)["out"].(*model.ObjectRef)
	if !ok {
		t.Fatalf("the big output was not externalized")
	}

	reloaded.State["outputs"].(map[string]any)["out"] = "small"
	if err := database.UpdateInstanceProgress(reloaded); err != nil {
		t.Fatalf("UpdateInstanceProgress: %v", err)
	}
	// Two sweeps: the first NOTICES nothing claims it and starts the clock, the second collects.
	// specs/object-store.md.
	eng.collectObjects()
	db.AdvanceClock(time.Second)
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

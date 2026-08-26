package engine

import (
	"context"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"genroc/internal/db"
	"genroc/internal/errcode"
	"genroc/internal/idgen"
	"genroc/internal/model"
	"genroc/internal/shape"
)

// A panic under advance becomes a terminal EnginePanic failure instead of taking the worker
// down with every other instance it advances. Go rather than e2e: no HTTP request provokes a
// panic on purpose — the trigger has to be planted in stored state.

func newPanicEngine(t *testing.T, name string) (*db.DB, *Engine) {
	t.Helper()
	database, err := db.OpenSQLite(filepath.Join(t.TempDir(), name+".db"), "OFF")
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	t.Cleanup(func() { database.Close() })
	return database, New(database, time.Second, 1, false, time.Minute, 30*time.Second,
		LogConfig{}, slog.New(slog.NewTextHandler(io.Discard, nil)))
}

func saveInstance(t *testing.T, database *db.DB, process string, ctxData map[string]any) *model.ProcessInstance {
	t.Helper()
	inst := &model.ProcessInstance{
		ID:             idgen.New(),
		ProcessName:    process,
		ProcessVersion: 1,
		Task:           "a",
		State:          ctxData,
		Status:         model.StatusRunning,
		CreatedAt:      time.Now(),
	}
	if err := database.SaveInstance(inst); err != nil {
		t.Fatalf("save instance: %v", err)
	}
	return inst
}

// The primary claim: the worker survives and the instance carries the reason. The trigger is a
// nil context map (setTaskOutput writes into it), on a definition that stays valid — so the
// recovery path's own audit write works and the failure is recorded in full.
func TestAdvancePanicFailsInstanceInsteadOfWorker(t *testing.T) {
	database, e := newPanicEngine(t, "panic")

	def := &model.ProcessDefinition{
		Name: "panics",
		Tasks: []*model.Task{{
			ID:     "a",
			Output: &shape.Shape{Raw: []byte(`"$: 1"`)},
			Switch: model.SwitchMap{{Goto: model.GotoEnd}},
		}},
	}
	if err := database.SaveDefinition(def, 1, nil, "", "latest"); err != nil {
		t.Fatalf("save definition: %v", err)
	}
	inst := saveInstance(t, database, "panics", nil)

	// The point of the test: this returns instead of unwinding into the caller.
	if err := e.runAdvance(context.Background(), inst); err != nil {
		t.Fatalf("runAdvance returned an error: %v", err)
	}

	got, err := database.GetInstance(inst.ID)
	if err != nil {
		t.Fatalf("reload instance: %v", err)
	}
	if got.Status != model.StatusFailed {
		t.Errorf("status = %q, want %q", got.Status, model.StatusFailed)
	}
	if got.ErrorCode != string(errcode.EnginePanic) {
		t.Errorf("error_code = %q, want %q", got.ErrorCode, errcode.EnginePanic)
	}
	if !strings.Contains(got.ErrorMessage, "panic while advancing") {
		t.Errorf("error = %q, want it to mention the panic", got.ErrorMessage)
	}

	// The stack lands in the instance's own trail, so a panicked instance is
	// debuggable from the API alone rather than from the worker's console.
	logs, _, err := database.ListLogs(inst.ID, db.LogQuery{})
	if err != nil {
		t.Fatalf("list logs: %v", err)
	}
	var found bool
	for _, l := range logs {
		if l.Code == string(errcode.EnginePanic) && strings.Contains(l.Data, "runtime") {
			found = true
		}
	}
	if !found {
		t.Errorf("no audit entry carrying the panic stack; got %d entries", len(logs))
	}
}

// Recording the panic can itself panic — audit resolves the definition to redact secrets, so
// one malformed enough to panic advance is a good bet to panic the recording. Here a null task
// entry dereferences in both places; the instance must still persist as failed.
func TestAdvancePanicSurvivesAPanicWhileRecordingIt(t *testing.T) {
	database, e := newPanicEngine(t, "double_panic")

	def := &model.ProcessDefinition{
		Name:  "panics_twice",
		Tasks: []*model.Task{nil}, // marshals to "tasks":[null]
	}
	if err := database.SaveDefinition(def, 1, nil, "", "latest"); err != nil {
		t.Fatalf("save definition: %v", err)
	}
	inst := saveInstance(t, database, "panics_twice", map[string]any{"input": map[string]any{}})

	if err := e.runAdvance(context.Background(), inst); err != nil {
		t.Fatalf("runAdvance returned an error: %v", err)
	}

	got, err := database.GetInstance(inst.ID)
	if err != nil {
		t.Fatalf("reload instance: %v", err)
	}
	if got.Status != model.StatusFailed {
		t.Errorf("status = %q, want %q", got.Status, model.StatusFailed)
	}
	if got.ErrorCode != string(errcode.EnginePanic) {
		t.Errorf("error_code = %q, want %q", got.ErrorCode, errcode.EnginePanic)
	}
}

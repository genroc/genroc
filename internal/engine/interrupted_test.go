package engine

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"genroc/internal/db"
	"genroc/internal/errcode"
	"genroc/internal/model"
)

// The interruption is staged, not simulated: a foreign worker claims the instance with a
// lease short enough to expire immediately, which is the state a crashed worker leaves
// behind. Manual tick mode makes each advance a separate, ordered step.

// interruptedFixture builds "charge" (an only_once fetch at a hit-counting URL, with the
// given on_error) followed by a call-less "verify", and returns the instance id and the
// counter.
func interruptedFixture(t *testing.T, database *db.DB, name string, status model.Status, onError []model.ErrorCase) (string, *atomic.Int32) {
	t.Helper()

	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"ok":true}`))
	}))
	t.Cleanup(srv.Close)

	onlyOnce := true
	process := fmt.Sprintf("%s-%d", name, time.Now().UnixNano())
	tasks := []*model.Task{
		{
			ID:       "charge",
			Action:   &model.Action{Type: model.ActionTypeFetch, URL: srv.URL},
			OnlyOnce: &onlyOnce,
			OnError:  onError,
			Switch:   model.SwitchMap{{Goto: model.GotoEnd}},
		},
		{ID: "verify", Switch: model.SwitchMap{{Goto: model.GotoEnd}}},
	}
	if err := database.SaveDefinition(&model.ProcessDefinition{Name: process, Tasks: tasks}, 1, nil, process+"-hash", ""); err != nil {
		t.Fatalf("SaveDefinition: %v", err)
	}

	id := fmt.Sprintf("%s-i-%d", name, time.Now().UnixNano())
	if err := database.SaveInstance(&model.ProcessInstance{
		ID: id, ProcessName: process, ProcessVersion: 1,
		Task: "charge", ContextData: map[string]any{}, Status: status,
	}); err != nil {
		t.Fatalf("SaveInstance: %v", err)
	}
	return id, &hits
}

func interrupt(t *testing.T, database *db.DB, id string) {
	t.Helper()
	claimed, err := database.ClaimInstances("dead-worker", time.Millisecond, 10, db.AllowTakeover())
	if err != nil {
		t.Fatalf("stage interruption: %v", err)
	}
	var found bool
	for _, c := range claimed {
		if c.ID == id {
			found = true
		}
	}
	if !found {
		t.Fatalf("stage interruption: instance %s was not claimable", id)
	}
	time.Sleep(5 * time.Millisecond) // let the foreign lease expire
}

func tickEngine(t *testing.T, database *db.DB) *Engine {
	t.Helper()
	return New(database, 0 /* manual tick */, 4, true, 0, 0, LogConfig{}, slog.New(slog.NewTextHandler(io.Discard, nil)))
}

func TestInterrupted_RoutesToHandler(t *testing.T) {
	database := openTestDB(t)
	id, hits := interruptedFixture(t, database, "routed", model.StatusRunning,
		[]model.ErrorCase{{Code: []string{"only_once.interrupted"}, Goto: "verify"}})
	interrupt(t, database, id)

	eng := tickEngine(t, database)
	if _, err := eng.Tick(context.Background()); err != nil {
		t.Fatalf("Tick: %v", err)
	}

	inst, err := database.GetInstance(id)
	if err != nil {
		t.Fatalf("GetInstance: %v", err)
	}
	if inst.Task != "verify" {
		t.Errorf("instance parked on %q, want the handler task %q", inst.Task, "verify")
	}
	if inst.Status != model.StatusRunning {
		t.Errorf("status %q, want running — routing is not a terminal outcome", inst.Status)
	}
	if inst.ErrorCode != "" {
		t.Errorf("error_code %q, want empty: the error was handled, not recorded as an outcome", inst.ErrorCode)
	}
	if got := hits.Load(); got != 0 {
		t.Errorf("the interrupted action was executed %d times; the engine must never re-run it", got)
	}

	// `error` is what the handler reads to know what it is recovering from.
	errCtx, _ := inst.ContextData["error"].(map[string]any)
	if errCtx == nil {
		t.Fatalf("no `error` in context, got %#v", inst.ContextData)
	}
	if errCtx["code"] != string(errcode.OnlyOnceInterrupted) {
		t.Errorf("`error.code` = %v, want %s", errCtx["code"], errcode.OnlyOnceInterrupted)
	}
	if errCtx["task"] != "charge" {
		t.Errorf("`error.task` = %v, want the interrupted task", errCtx["task"])
	}

	// The handler runs on the next claim and the process finishes normally.
	if _, err := eng.Tick(context.Background()); err != nil {
		t.Fatalf("Tick (handler): %v", err)
	}
	if inst, err = database.GetInstance(id); err != nil {
		t.Fatalf("GetInstance: %v", err)
	} else if inst.Status != model.StatusCompleted {
		t.Errorf("after the handler ran, status %q, want completed", inst.Status)
	}
}

func TestInterrupted_UncaughtFails(t *testing.T) {
	database := openTestDB(t)
	id, hits := interruptedFixture(t, database, "uncaught", model.StatusRunning, nil)
	interrupt(t, database, id)

	if _, err := tickEngine(t, database).Tick(context.Background()); err != nil {
		t.Fatalf("Tick: %v", err)
	}

	inst, err := database.GetInstance(id)
	if err != nil {
		t.Fatalf("GetInstance: %v", err)
	}
	if inst.Status != model.StatusFailed {
		t.Fatalf("status %q, want failed with no handler", inst.Status)
	}
	if inst.ErrorCode != string(errcode.OnlyOnceInterrupted) {
		t.Errorf("error_code %q, want %s", inst.ErrorCode, errcode.OnlyOnceInterrupted)
	}
	if got := hits.Load(); got != 0 {
		t.Errorf("the interrupted action was executed %d times; the engine must never re-run it", got)
	}
}

// What a definition registered before the unknowable-set rule looks like: validation
// refuses this today, but stored rules never re-validate.
func TestInterrupted_StoredRetryRuleIsNotRetried(t *testing.T) {
	database := openTestDB(t)
	id, hits := interruptedFixture(t, database, "storedretry", model.StatusRunning,
		[]model.ErrorCase{{Code: []string{"only_once.interrupted"}, Retry: model.RetryAttempts(3), Goto: "verify"}})
	interrupt(t, database, id)

	if _, err := tickEngine(t, database).Tick(context.Background()); err != nil {
		t.Fatalf("Tick: %v", err)
	}

	inst, err := database.GetInstance(id)
	if err != nil {
		t.Fatalf("GetInstance: %v", err)
	}
	if inst.RetryCount != 0 {
		t.Errorf("retry_count = %d, want 0: an unknowable error is never retried", inst.RetryCount)
	}
	if inst.WakeAt != nil {
		t.Errorf("wake_at = %v, want nil: no retry was scheduled", inst.WakeAt)
	}
	if inst.Task != "verify" {
		t.Errorf("instance parked on %q, want the handler task — the rule routes even though its retries are refused", inst.Task)
	}
	if got := hits.Load(); got != 0 {
		t.Errorf("the interrupted action was executed %d times", got)
	}
}

func TestInterrupted_PlainTaskStillReRuns(t *testing.T) {
	database := openTestDB(t)

	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()

	process := fmt.Sprintf("plain-%d", time.Now().UnixNano())
	tasks := []*model.Task{{
		ID:     "charge",
		Action: &model.Action{Type: model.ActionTypeFetch, URL: srv.URL},
		Switch: model.SwitchMap{{Goto: model.GotoEnd}},
	}}
	if err := database.SaveDefinition(&model.ProcessDefinition{Name: process, Tasks: tasks}, 1, nil, process+"-hash", ""); err != nil {
		t.Fatalf("SaveDefinition: %v", err)
	}
	id := fmt.Sprintf("plain-i-%d", time.Now().UnixNano())
	if err := database.SaveInstance(&model.ProcessInstance{
		ID: id, ProcessName: process, ProcessVersion: 1,
		Task: "charge", ContextData: map[string]any{}, Status: model.StatusRunning,
	}); err != nil {
		t.Fatalf("SaveInstance: %v", err)
	}
	interrupt(t, database, id)

	if _, err := tickEngine(t, database).Tick(context.Background()); err != nil {
		t.Fatalf("Tick: %v", err)
	}
	inst, err := database.GetInstance(id)
	if err != nil {
		t.Fatalf("GetInstance: %v", err)
	}
	if inst.Status != model.StatusCompleted {
		t.Errorf("status %q, want completed: a plain task is simply re-run", inst.Status)
	}
	if got := hits.Load(); got != 1 {
		t.Errorf("the task ran %d times, want 1 (the re-run)", got)
	}
}

// ── a pending pause ──────────────────────────────────────────────────────────

func TestInterrupted_WhilePausing_RoutesThenPauses(t *testing.T) {
	database := openTestDB(t)
	id, hits := interruptedFixture(t, database, "pausingrouted", model.StatusPausing,
		[]model.ErrorCase{{Code: []string{"only_once.interrupted"}, Goto: "verify"}})
	interrupt(t, database, id)

	if _, err := tickEngine(t, database).Tick(context.Background()); err != nil {
		t.Fatalf("Tick: %v", err)
	}

	inst, err := database.GetInstance(id)
	if err != nil {
		t.Fatalf("GetInstance: %v", err)
	}
	if inst.Status != model.StatusPaused {
		t.Errorf("status %q, want paused: the operator's pause still lands", inst.Status)
	}
	if inst.Task != "verify" {
		t.Errorf("paused on %q, want the handler task: the decision is taken before the pause, the handler runs after it", inst.Task)
	}
	if got := hits.Load(); got != 0 {
		t.Errorf("the interrupted action was executed %d times", got)
	}
}

func TestInterrupted_WhilePausing_UncaughtFails(t *testing.T) {
	database := openTestDB(t)
	id, _ := interruptedFixture(t, database, "pausinguncaught", model.StatusPausing, nil)
	interrupt(t, database, id)

	if _, err := tickEngine(t, database).Tick(context.Background()); err != nil {
		t.Fatalf("Tick: %v", err)
	}

	inst, err := database.GetInstance(id)
	if err != nil {
		t.Fatalf("GetInstance: %v", err)
	}
	if inst.Status != model.StatusFailed {
		t.Fatalf("status %q, want failed: a failure outranks a pause", inst.Status)
	}
	if inst.ErrorCode != string(errcode.OnlyOnceInterrupted) {
		t.Errorf("error_code %q, want %s", inst.ErrorCode, errcode.OnlyOnceInterrupted)
	}
}

// The pause path's original job, which must survive the branch added in front of it.
func TestInterrupted_WhilePausing_PlainTaskJustPauses(t *testing.T) {
	database := openTestDB(t)

	process := fmt.Sprintf("pausingplain-%d", time.Now().UnixNano())
	tasks := []*model.Task{{
		ID:     "charge",
		Action: &model.Action{Type: model.ActionTypeFetch, URL: "http://127.0.0.1:1/never"},
		Switch: model.SwitchMap{{Goto: model.GotoEnd}},
	}}
	if err := database.SaveDefinition(&model.ProcessDefinition{Name: process, Tasks: tasks}, 1, nil, process+"-hash", ""); err != nil {
		t.Fatalf("SaveDefinition: %v", err)
	}
	id := fmt.Sprintf("pausingplain-i-%d", time.Now().UnixNano())
	if err := database.SaveInstance(&model.ProcessInstance{
		ID: id, ProcessName: process, ProcessVersion: 1,
		Task: "charge", ContextData: map[string]any{}, Status: model.StatusPausing,
	}); err != nil {
		t.Fatalf("SaveInstance: %v", err)
	}
	interrupt(t, database, id)

	if _, err := tickEngine(t, database).Tick(context.Background()); err != nil {
		t.Fatalf("Tick: %v", err)
	}
	inst, err := database.GetInstance(id)
	if err != nil {
		t.Fatalf("GetInstance: %v", err)
	}
	if inst.Status != model.StatusPaused {
		t.Errorf("status %q, want paused", inst.Status)
	}
	if inst.Task != "charge" {
		t.Errorf("paused on %q, want the task it was on: it is re-run on resume", inst.Task)
	}
}

// advance() tests 'failing' before anything else, so a tree on its way down is settled,
// not adjudicated — and keeps the failure that started it.
func TestInterrupted_FailingTreeKeepsItsOriginalCause(t *testing.T) {
	database := openTestDB(t)
	id, hits := interruptedFixture(t, database, "failing", model.StatusFailing,
		[]model.ErrorCase{{Code: []string{"only_once.interrupted"}, Goto: "verify"}})

	// Stamp the cause that poisoned the tree, the way FailAncestors would have.
	inst, err := database.GetInstance(id)
	if err != nil {
		t.Fatalf("GetInstance: %v", err)
	}
	inst.Error = "sibling exploded"
	inst.ErrorCode = "child_failed"
	if err := database.UpdateInstance(inst); err != nil {
		t.Fatalf("UpdateInstance: %v", err)
	}
	interrupt(t, database, id)

	if _, err := tickEngine(t, database).Tick(context.Background()); err != nil {
		t.Fatalf("Tick: %v", err)
	}

	if inst, err = database.GetInstance(id); err != nil {
		t.Fatalf("GetInstance: %v", err)
	}
	if inst.Status != model.StatusFailed {
		t.Errorf("status %q, want failed: a draining instance settles", inst.Status)
	}
	if inst.ErrorCode != "child_failed" {
		t.Errorf("error_code %q, want the original cause: the interruption must not relabel a tree that was already dying", inst.ErrorCode)
	}
	if inst.Task == "verify" {
		t.Error("a draining instance was routed to a handler; 'failing' outranks the interruption")
	}
	if got := hits.Load(); got != 0 {
		t.Errorf("the interrupted action was executed %d times", got)
	}
}

// The runtime half of the unknowable-set ban: all that stands behind definitions stored
// before validation learned to refuse them.
func TestIsRetryAllowed_UnknowableSet(t *testing.T) {
	yes, no := true, false
	assert := func(v *bool) *model.ErrorCase { return &model.ErrorCase{NotReached: v} }

	cases := []struct {
		name     string
		onlyOnce bool
		code     errcode.Code
		matched  *model.ErrorCase
		want     bool
	}{
		{"plain task, timeout, retried", false, errcode.HTTPTimeout, nil, true},
		{"plain task, interrupted, retried", false, errcode.OnlyOnceInterrupted, nil, true},
		{"only_once, pre.timeout, retried", true, errcode.PreTimeout, nil, true},
		{"only_once, http.500, refused", true, errcode.HTTP(500), nil, false},
		{"only_once, http.500 + not_reached, retried", true, errcode.HTTP(500), assert(&yes), true},
		{"only_once, http.timeout, refused", true, errcode.HTTPTimeout, nil, false},
		{"only_once, http.timeout + not_reached, still refused", true, errcode.HTTPTimeout, assert(&yes), false},
		{"only_once, external.timeout + not_reached, still refused", true, errcode.ExternalTimeout, assert(&yes), false},
		{"only_once, interrupted + not_reached, still refused", true, errcode.OnlyOnceInterrupted, assert(&yes), false},
		{"only_once, interrupted + not_reached:false, refused", true, errcode.OnlyOnceInterrupted, assert(&no), false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			task := &model.Task{ID: "charge"}
			if c.onlyOnce {
				task.OnlyOnce = &yes
			}
			if got := isRetryAllowed(task, c.code, c.matched); got != c.want {
				t.Errorf("isRetryAllowed(%s) = %v, want %v", c.code, got, c.want)
			}
		})
	}
}

package dbtest

import (
	"context"
	"strings"
	"testing"
	"time"

	dbpkg "genroc/internal/db"
	"genroc/internal/model"
)

// insertInst inserts an instance with the given status, parent, call stack, and error.
func insertInst(t *testing.T, db *dbpkg.DB, id string, status model.Status, parentID string, callStack []string, errMsg string) {
	t.Helper()
	inst := &model.ProcessInstance{
		ID:             id,
		ProcessName:    "test",
		ProcessVersion: 1,
		Task:           "step1",
		ContextData:    map[string]any{},
		Status:         status,
		ParentID:       parentID,
		CallStack:      callStack,
		Error:          errMsg,
	}
	if err := db.SaveInstance(inst); err != nil {
		t.Fatalf("insertInst %q: %v", id, err)
	}
}

func mustStatus(t *testing.T, db *dbpkg.DB, id string) model.Status {
	t.Helper()
	inst, err := db.GetInstance(id)
	if err != nil {
		t.Fatalf("GetInstance %q: %v", id, err)
	}
	return inst.Status
}

func mustError(t *testing.T, db *dbpkg.DB, id string) string {
	t.Helper()
	inst, err := db.GetInstance(id)
	if err != nil {
		t.Fatalf("GetInstance %q: %v", id, err)
	}
	return inst.Error
}

func mustWaitState(t *testing.T, db *dbpkg.DB, id string) model.WaitState {
	t.Helper()
	inst, err := db.GetInstance(id)
	if err != nil {
		t.Fatalf("GetInstance %q: %v", id, err)
	}
	return inst.WaitState
}

// insertInstW inserts an instance with an explicit wait_state (for testing waiting parents).
func insertInstW(t *testing.T, db *dbpkg.DB, id string, status model.Status, waitState model.WaitState, parentID string, callStack []string, errMsg string) {
	t.Helper()
	inst := &model.ProcessInstance{
		ID:             id,
		ProcessName:    "test",
		ProcessVersion: 1,
		Task:           "step1",
		ContextData:    map[string]any{},
		Status:         status,
		WaitState:      waitState,
		ParentID:       parentID,
		CallStack:      callStack,
		Error:          errMsg,
	}
	if err := db.SaveInstance(inst); err != nil {
		t.Fatalf("insertInstW %q: %v", id, err)
	}
}

// lease claims id for a worker so its row carries a live worker_id + lease_expires_at,
// i.e. it looks like an instance a worker is mid-task on. Claiming is the only way to
// take a lease, so the fixture stays engine-agnostic (no hand-written UPDATE).
func lease(t *testing.T, db *dbpkg.DB, id string) {
	t.Helper()
	claimed, err := db.ClaimInstances("test-worker", time.Minute, 10, dbpkg.AllowTakeover())
	if err != nil {
		t.Fatalf("ClaimInstances: %v", err)
	}
	for _, inst := range claimed {
		if inst.ID == id {
			return
		}
	}
	t.Fatalf("instance %q was not claimed (claimed %d instances)", id, len(claimed))
}

// TestPauseProcess_SingleInstance verifies the lease-dependent split: a running
// instance with nothing in flight is suspended immediately ('paused'), while one a
// worker currently holds is only marked 'pausing' — the pause lands when that
// worker's write releases the lease.
func TestPauseProcess_SingleInstance(t *testing.T) {
	for _, b := range testBackends(t) {
		t.Run(b.name, func(t *testing.T) {
			// Leased arm first: claim it while it is the only instance in the DB.
			insertInst(t, b.db, "held", model.StatusRunning, "", nil, "")
			lease(t, b.db, "held")

			if err := b.db.PauseProcess(context.Background(), "held"); err != nil {
				t.Fatalf("PauseProcess(held): %v", err)
			}
			if got := mustStatus(t, b.db, "held"); got != model.StatusPausing {
				t.Errorf("held: expected pausing (worker mid-task), got %q", got)
			}

			// Unleased arm: nothing in flight, so there is no worker write to wait
			// for — and marking it 'pausing' would strand it, since a parked
			// instance may never be claimed again.
			insertInst(t, b.db, "idle", model.StatusRunning, "", nil, "")

			if err := b.db.PauseProcess(context.Background(), "idle"); err != nil {
				t.Fatalf("PauseProcess(idle): %v", err)
			}
			if got := mustStatus(t, b.db, "idle"); got != model.StatusPaused {
				t.Errorf("idle: expected paused, got %q", got)
			}
		})
	}
}

// TestPauseProcess_Descendants verifies that all descendants of a root are suspended,
// and that pausing changes nothing but the status column — a child parked on its own
// children keeps wait_state='waiting' so resuming picks up exactly where it stopped.
func TestPauseProcess_Descendants(t *testing.T) {
	for _, b := range testBackends(t) {
		t.Run(b.name, func(t *testing.T) {
			insertInst(t, b.db, "root", model.StatusRunning, "", nil, "")
			insertInst(t, b.db, "child1", model.StatusRunning, "root", []string{"root"}, "")
			insertInstW(t, b.db, "child2", model.StatusRunning, model.WaitStateWaiting, "root", []string{"root"}, "")
			// grandchild of root via child1
			insertInst(t, b.db, "gc1", model.StatusRunning, "child1", []string{"root", "child1"}, "")

			if err := b.db.PauseProcess(context.Background(), "root"); err != nil {
				t.Fatalf("PauseProcess: %v", err)
			}

			// Nothing is leased, so the whole tree settles straight to 'paused'.
			for _, id := range []string{"root", "child1", "child2", "gc1"} {
				if got := mustStatus(t, b.db, id); got != model.StatusPaused {
					t.Errorf("%q: expected paused, got %q", id, got)
				}
			}
			if got := mustWaitState(t, b.db, "child2"); got != model.WaitStateWaiting {
				t.Errorf("child2: wait_state should be preserved, got %q", got)
			}
		})
	}
}

// TestPauseProcess_SkipsTerminalDescendants verifies completed/failed children are untouched.
func TestPauseProcess_SkipsTerminalDescendants(t *testing.T) {
	for _, b := range testBackends(t) {
		t.Run(b.name, func(t *testing.T) {
			insertInst(t, b.db, "root", model.StatusRunning, "", nil, "")
			insertInst(t, b.db, "c-running", model.StatusRunning, "root", []string{"root"}, "")
			insertInst(t, b.db, "c-completed", model.StatusCompleted, "root", []string{"root"}, "")
			insertInst(t, b.db, "c-failed", model.StatusFailed, "root", []string{"root"}, "err")

			if err := b.db.PauseProcess(context.Background(), "root"); err != nil {
				t.Fatalf("PauseProcess: %v", err)
			}

			if got := mustStatus(t, b.db, "c-running"); got != model.StatusPaused {
				t.Errorf("c-running: expected paused, got %q", got)
			}
			if got := mustStatus(t, b.db, "c-completed"); got != model.StatusCompleted {
				t.Errorf("c-completed: should stay completed, got %q", got)
			}
			if got := mustStatus(t, b.db, "c-failed"); got != model.StatusFailed {
				t.Errorf("c-failed: should stay failed, got %q", got)
			}
		})
	}
}

// TestPauseProcess_NothingRunning verifies that pausing a tree with nothing running
// is reported rather than silently succeeding — an already-settled (or already-paused)
// process has nothing to suspend.
func TestPauseProcess_NothingRunning(t *testing.T) {
	for _, b := range testBackends(t) {
		t.Run(b.name, func(t *testing.T) {
			insertInst(t, b.db, "root", model.StatusCompleted, "", nil, "")
			insertInst(t, b.db, "child", model.StatusCompleted, "root", []string{"root"}, "")

			err := b.db.PauseProcess(context.Background(), "root")
			if err == nil {
				t.Fatal("expected error pausing a settled tree, got nil")
			}
			if !strings.Contains(err.Error(), "no running instances to pause") {
				t.Errorf("expected 'no running instances to pause' error, got %q", err)
			}
		})
	}
}

// TestPauseProcess_NonRootRejected verifies that pausing a descendant directly
// is rejected with an error naming the tree root, leaving the tree untouched.
func TestPauseProcess_NonRootRejected(t *testing.T) {
	for _, b := range testBackends(t) {
		t.Run(b.name, func(t *testing.T) {
			insertInst(t, b.db, "root", model.StatusRunning, "", nil, "")
			insertInst(t, b.db, "mid", model.StatusRunning, "root", []string{"root"}, "")
			insertInst(t, b.db, "leaf", model.StatusRunning, "mid", []string{"root", "mid"}, "")

			err := b.db.PauseProcess(context.Background(), "leaf")
			if err == nil {
				t.Fatal("expected error for non-root pause, got nil")
			}
			if !strings.Contains(err.Error(), `"root"`) {
				t.Errorf("error should name the root instance, got %q", err)
			}
			for _, id := range []string{"root", "mid", "leaf"} {
				if got := mustStatus(t, b.db, id); got != model.StatusRunning {
					t.Errorf("%q: expected running (untouched), got %q", id, got)
				}
			}
		})
	}
}

// TestResumeProcess_RestoresSubtree verifies that resuming flips a paused tree back
// to running and nothing else: wait_state, wake_at and retry_count survive the
// pause/resume round trip verbatim, which is what makes resume a status flip rather
// than the revival RetryProcess performs.
func TestResumeProcess_RestoresSubtree(t *testing.T) {
	for _, b := range testBackends(t) {
		t.Run(b.name, func(t *testing.T) {
			wakeAt := time.Now().Add(time.Hour).UTC().Truncate(time.Millisecond)
			// Root parked on children; the child parked on a retry backoff.
			insertInstW(t, b.db, "root", model.StatusRunning, model.WaitStateWaiting, "", nil, "")
			backoff := &model.ProcessInstance{
				ID:             "child",
				ProcessName:    "test",
				ProcessVersion: 1,
				Task:           "step1",
				ContextData:    map[string]any{},
				Status:         model.StatusRunning,
				ParentID:       "root",
				CallStack:      []string{"root"},
				RetryCount:     2,
				WakeAt:         &wakeAt,
			}
			if err := b.db.SaveInstance(backoff); err != nil {
				t.Fatalf("SaveInstance child: %v", err)
			}

			if err := b.db.PauseProcess(context.Background(), "root"); err != nil {
				t.Fatalf("PauseProcess: %v", err)
			}
			if err := b.db.ResumeProcess(context.Background(), "root"); err != nil {
				t.Fatalf("ResumeProcess: %v", err)
			}

			for _, id := range []string{"root", "child"} {
				if got := mustStatus(t, b.db, id); got != model.StatusRunning {
					t.Errorf("%q: expected running, got %q", id, got)
				}
			}
			if got := mustWaitState(t, b.db, "root"); got != model.WaitStateWaiting {
				t.Errorf("root: wait_state should be preserved, got %q", got)
			}
			child, err := b.db.GetInstance("child")
			if err != nil {
				t.Fatalf("GetInstance child: %v", err)
			}
			if child.RetryCount != 2 {
				t.Errorf("child: retry_count should be preserved, got %d", child.RetryCount)
			}
			// The timer kept running while paused, so it must come back unchanged.
			if child.WakeAt == nil || !child.WakeAt.Equal(wakeAt) {
				t.Errorf("child: wake_at should be preserved (%v), got %v", wakeAt, child.WakeAt)
			}
		})
	}
}

// TestResumeProcess_FlipsPausing verifies that a resume issued before a pause finished
// landing simply un-requests it: 'pausing' rows go back to running too.
func TestResumeProcess_FlipsPausing(t *testing.T) {
	for _, b := range testBackends(t) {
		t.Run(b.name, func(t *testing.T) {
			insertInst(t, b.db, "root", model.StatusRunning, "", nil, "")
			lease(t, b.db, "root")
			if err := b.db.PauseProcess(context.Background(), "root"); err != nil {
				t.Fatalf("PauseProcess: %v", err)
			}
			if got := mustStatus(t, b.db, "root"); got != model.StatusPausing {
				t.Fatalf("root: expected pausing before resume, got %q", got)
			}

			if err := b.db.ResumeProcess(context.Background(), "root"); err != nil {
				t.Fatalf("ResumeProcess: %v", err)
			}
			if got := mustStatus(t, b.db, "root"); got != model.StatusRunning {
				t.Errorf("root: expected running, got %q", got)
			}
		})
	}
}

// TestResumeProcess_FailingRootOverPausedDescendant covers the wedged tree: a branch
// died while the tree was paused, so the root is 'failing' but cannot settle (a failing
// parent waits for every child, and paused children count as active). The precondition
// is on the subtree, not the root's own status, precisely so resuming unblocks this.
func TestResumeProcess_FailingRootOverPausedDescendant(t *testing.T) {
	for _, b := range testBackends(t) {
		t.Run(b.name, func(t *testing.T) {
			insertInstW(t, b.db, "root", model.StatusFailing, model.WaitStateWaiting, "", nil, "boom")
			insertChild(t, b.db, "c-dead", model.StatusFailed, "root", "step1", []string{"root"}, "boom")
			insertChild(t, b.db, "c-paused", model.StatusPaused, "root", "step1", []string{"root"}, "")

			if err := b.db.ResumeProcess(context.Background(), "root"); err != nil {
				t.Fatalf("ResumeProcess: %v", err)
			}

			if got := mustStatus(t, b.db, "c-paused"); got != model.StatusRunning {
				t.Errorf("c-paused: expected running, got %q", got)
			}
			// The root's own outcome is untouched — it stays doomed and drains to
			// failed once the resumed branch settles.
			if got := mustStatus(t, b.db, "root"); got != model.StatusFailing {
				t.Errorf("root: expected failing (untouched), got %q", got)
			}
			if got := mustStatus(t, b.db, "c-dead"); got != model.StatusFailed {
				t.Errorf("c-dead: expected failed (untouched), got %q", got)
			}
		})
	}
}

// TestResumeProcess_NothingPaused verifies that resuming a tree with no suspended
// instance is reported rather than silently succeeding.
func TestResumeProcess_NothingPaused(t *testing.T) {
	for _, b := range testBackends(t) {
		t.Run(b.name, func(t *testing.T) {
			insertInst(t, b.db, "root", model.StatusRunning, "", nil, "")
			insertInst(t, b.db, "child", model.StatusRunning, "root", []string{"root"}, "")

			err := b.db.ResumeProcess(context.Background(), "root")
			if err == nil {
				t.Fatal("expected error resuming a running tree, got nil")
			}
			if !strings.Contains(err.Error(), "not paused") {
				t.Errorf("expected 'not paused' error, got %q", err)
			}
			for _, id := range []string{"root", "child"} {
				if got := mustStatus(t, b.db, id); got != model.StatusRunning {
					t.Errorf("%q: expected running (untouched), got %q", id, got)
				}
			}
		})
	}
}

// TestResumeProcess_NonRootRejected verifies that resuming a descendant directly is
// rejected with an error naming the tree root, leaving the tree untouched.
func TestResumeProcess_NonRootRejected(t *testing.T) {
	for _, b := range testBackends(t) {
		t.Run(b.name, func(t *testing.T) {
			insertInst(t, b.db, "root", model.StatusPaused, "", nil, "")
			insertInst(t, b.db, "mid", model.StatusPaused, "root", []string{"root"}, "")
			insertInst(t, b.db, "leaf", model.StatusPaused, "mid", []string{"root", "mid"}, "")

			err := b.db.ResumeProcess(context.Background(), "leaf")
			if err == nil {
				t.Fatal("expected error for non-root resume, got nil")
			}
			if !strings.Contains(err.Error(), `"root"`) {
				t.Errorf("error should name the root instance, got %q", err)
			}
			for _, id := range []string{"root", "mid", "leaf"} {
				if got := mustStatus(t, b.db, id); got != model.StatusPaused {
					t.Errorf("%q: expected paused (untouched), got %q", id, got)
				}
			}
		})
	}
}

// TestUpdateInstance_LandsPendingPause verifies the SQL CASE that settles a pause
// requested while the instance was leased: the worker's write releases the lease, so
// a still-running instance becomes 'paused' while a real outcome (completed) wins.
func TestUpdateInstance_LandsPendingPause(t *testing.T) {
	for _, b := range testBackends(t) {
		t.Run(b.name, func(t *testing.T) {
			// still-running write → the pause lands
			insertInst(t, b.db, "held", model.StatusRunning, "", nil, "")
			lease(t, b.db, "held")
			if err := b.db.PauseProcess(context.Background(), "held"); err != nil {
				t.Fatalf("PauseProcess(held): %v", err)
			}
			held, err := b.db.GetInstance("held")
			if err != nil {
				t.Fatalf("GetInstance held: %v", err)
			}
			held.Status = model.StatusRunning // the worker knows nothing about the pause
			if err := b.db.UpdateInstance(held); err != nil {
				t.Fatalf("UpdateInstance held: %v", err)
			}
			if got := mustStatus(t, b.db, "held"); got != model.StatusPaused {
				t.Errorf("held: expected paused, got %q", got)
			}

			// A finished task writes a real outcome, which is never hidden by a pause.
			insertInst(t, b.db, "done", model.StatusRunning, "", nil, "")
			lease(t, b.db, "done")
			if err := b.db.PauseProcess(context.Background(), "done"); err != nil {
				t.Fatalf("PauseProcess(done): %v", err)
			}
			done, err := b.db.GetInstance("done")
			if err != nil {
				t.Fatalf("GetInstance done: %v", err)
			}
			done.Status = model.StatusCompleted
			if err := b.db.UpdateInstance(done); err != nil {
				t.Fatalf("UpdateInstance done: %v", err)
			}
			if got := mustStatus(t, b.db, "done"); got != model.StatusCompleted {
				t.Errorf("done: expected completed, got %q", got)
			}
		})
	}
}

// TestUpdateInstanceProgress_LandsPendingPause verifies that a progress checkpoint
// lands a pending pause unconditionally — a checkpoint always means "still running",
// and this is also the write that parks on a delay or an external task, where no later
// claim could settle the instance.
func TestUpdateInstanceProgress_LandsPendingPause(t *testing.T) {
	for _, b := range testBackends(t) {
		t.Run(b.name, func(t *testing.T) {
			insertInst(t, b.db, "held", model.StatusRunning, "", nil, "")
			lease(t, b.db, "held")
			if err := b.db.PauseProcess(context.Background(), "held"); err != nil {
				t.Fatalf("PauseProcess: %v", err)
			}

			held, err := b.db.GetInstance("held")
			if err != nil {
				t.Fatalf("GetInstance: %v", err)
			}
			held.WaitState = model.WaitStateExternal
			if err := b.db.UpdateInstanceProgress(held); err != nil {
				t.Fatalf("UpdateInstanceProgress: %v", err)
			}

			if got := mustStatus(t, b.db, "held"); got != model.StatusPaused {
				t.Errorf("held: expected paused, got %q", got)
			}
			if got := mustWaitState(t, b.db, "held"); got != model.WaitStateExternal {
				t.Errorf("held: expected wait_state=external, got %q", got)
			}
		})
	}
}

// TestFailInstanceAndAncestors_OverridesPaused verifies that a child failure marks
// suspended ancestors as 'failing' — a failure is a real outcome and must not be
// hidden by a pause — while preserving their wait_state so they keep draining until
// the remaining children settle.
func TestFailInstanceAndAncestors_OverridesPaused(t *testing.T) {
	for _, b := range testBackends(t) {
		t.Run(b.name, func(t *testing.T) {
			insertInstW(t, b.db, "grand", model.StatusPaused, model.WaitStateWaiting, "", nil, "")
			insertInstW(t, b.db, "parent", model.StatusPausing, model.WaitStateWaiting, "grand", []string{"grand"}, "")
			// leaf is already failed and triggers ancestor failure propagation
			leaf := &model.ProcessInstance{
				ID:        "leaf",
				CallStack: []string{"grand", "parent"},
				Error:     "boom",
			}

			if err := b.db.FailInstanceAndAncestors(leaf); err != nil {
				t.Fatalf("FailInstanceAndAncestors: %v", err)
			}

			for _, id := range []string{"grand", "parent"} {
				if got := mustStatus(t, b.db, id); got != model.StatusFailing {
					t.Errorf("%q: expected failing, got %q", id, got)
				}
				if msg := mustError(t, b.db, id); msg != "boom" {
					t.Errorf("%q: expected error \"boom\", got %q", id, msg)
				}
				if got := mustWaitState(t, b.db, id); got != model.WaitStateWaiting {
					t.Errorf("%q: wait_state should be preserved, got %q", id, got)
				}
			}
		})
	}
}

// TestFailInstanceAndAncestors_AlreadyFailed verifies that already-failed ancestors are not overwritten.
func TestFailInstanceAndAncestors_AlreadyFailed(t *testing.T) {
	for _, b := range testBackends(t) {
		t.Run(b.name, func(t *testing.T) {
			insertInst(t, b.db, "parent", model.StatusFailed, "", nil, "original error")
			leaf := &model.ProcessInstance{
				ID:        "leaf",
				CallStack: []string{"parent"},
				Error:     "new error",
			}

			if err := b.db.FailInstanceAndAncestors(leaf); err != nil {
				t.Fatalf("FailInstanceAndAncestors: %v", err)
			}

			// Failed ancestors are excluded from the UPDATE (status IN condition)
			if msg := mustError(t, b.db, "parent"); msg != "original error" {
				t.Errorf("parent error should be unchanged, got %q", msg)
			}
		})
	}
}

// TestSpawnChildrenAndWait_RunningParent verifies normal spawn: parent → waiting.
func TestSpawnChildrenAndWait_RunningParent(t *testing.T) {
	for _, b := range testBackends(t) {
		t.Run(b.name, func(t *testing.T) {
			insertInst(t, b.db, "parent", model.StatusRunning, "", nil, "")

			parent, err := b.db.GetInstance("parent")
			if err != nil {
				t.Fatalf("GetInstance parent: %v", err)
			}
			child := &model.ProcessInstance{
				ID:          "child",
				ProcessName: "test",
				Task:        "step1",
				ContextData: map[string]any{},
				ParentID:    "parent",
				CallStack:   []string{"parent"},
				Status:      model.StatusRunning,
			}

			if err := b.db.SpawnChildrenAndWait(context.Background(), parent, []*model.ProcessInstance{child}); err != nil {
				t.Fatalf("SpawnChildrenAndWait: %v", err)
			}

			if got := mustStatus(t, b.db, "parent"); got != model.StatusRunning {
				t.Errorf("parent: expected running, got %q", got)
			}
			if got := mustWaitState(t, b.db, "parent"); got != model.WaitStateWaiting {
				t.Errorf("parent: expected wait_state=waiting, got %q", got)
			}
			if got := mustStatus(t, b.db, "child"); got != model.StatusRunning {
				t.Errorf("child: expected running, got %q", got)
			}
		})
	}
}

// TestSpawnChildrenAndWait_PausingParent verifies that a pause landing while the
// parent is mid-spawn settles here or never: the write parks the parent on
// wait_state='waiting', which removes it from the claim predicate, so no later claim
// could move it out of 'pausing'. The children inherit the settled status — a paused
// tree must not spawn runnable work.
func TestSpawnChildrenAndWait_PausingParent(t *testing.T) {
	for _, b := range testBackends(t) {
		t.Run(b.name, func(t *testing.T) {
			insertInst(t, b.db, "parent", model.StatusPausing, "", nil, "")

			parent, err := b.db.GetInstance("parent")
			if err != nil {
				t.Fatalf("GetInstance parent: %v", err)
			}
			child := &model.ProcessInstance{
				ID:          "child",
				ProcessName: "test",
				Task:        "step1",
				ContextData: map[string]any{},
				ParentID:    "parent",
				CallStack:   []string{"parent"},
				Status:      model.StatusRunning,
			}

			if err := b.db.SpawnChildrenAndWait(context.Background(), parent, []*model.ProcessInstance{child}); err != nil {
				t.Fatalf("SpawnChildrenAndWait: %v", err)
			}

			// The pending pause lands: parent is written as 'paused', not 'pausing'.
			if got := mustStatus(t, b.db, "parent"); got != model.StatusPaused {
				t.Errorf("parent: expected paused, got %q", got)
			}
			if got := mustWaitState(t, b.db, "parent"); got != model.WaitStateWaiting {
				t.Errorf("parent: expected wait_state=waiting, got %q", got)
			}
			// child is spawned paused (inherits the parent's settled status)
			if got := mustStatus(t, b.db, "child"); got != model.StatusPaused {
				t.Errorf("child: expected paused, got %q", got)
			}
		})
	}
}

// insertChild inserts a child instance spawned by the given parent task.
func insertChild(t *testing.T, db *dbpkg.DB, id string, status model.Status, parentID, spawnTaskID string, callStack []string, errMsg string) {
	t.Helper()
	inst := &model.ProcessInstance{
		ID:             id,
		ProcessName:    "test",
		ProcessVersion: 1,
		Task:           "step1",
		ContextData:    map[string]any{},
		Status:         status,
		ParentID:       parentID,
		SpawnTaskID:    spawnTaskID,
		CallStack:      callStack,
		Error:          errMsg,
	}
	if err := db.SaveInstance(inst); err != nil {
		t.Fatalf("insertChild %q: %v", id, err)
	}
}

// TestRetryProcess_NonRetryableStatuses verifies that only failed instances can be
// retried: a still-draining ('failing') tree must settle first, and a suspended one
// ('pausing'/'paused') is not a failure at all — retry hands the tree another
// on_error budget, which un-suspending must not grant, so it is pointed at resume.
func TestRetryProcess_NonRetryableStatuses(t *testing.T) {
	for _, b := range testBackends(t) {
		t.Run(b.name, func(t *testing.T) {
			for status, wantMsg := range map[model.Status]string{
				model.StatusRunning:   "not retryable",
				model.StatusFailing:   "not retryable",
				model.StatusCompleted: "not retryable",
				model.StatusPausing:   "resume it instead",
				model.StatusPaused:    "resume it instead",
			} {
				id := "inst-" + string(status)
				insertInst(t, b.db, id, status, "", nil, "")

				err := b.db.RetryProcess(context.Background(), id, false)
				if err == nil {
					t.Fatalf("%s: expected error, got nil", status)
				}
				if !strings.Contains(err.Error(), wantMsg) {
					t.Errorf("%s: expected %q error, got %q", status, wantMsg, err)
				}
				if got := mustStatus(t, b.db, id); got != status {
					t.Errorf("%s: status should be unchanged, got %q", status, got)
				}
			}
		})
	}
}

// TestRetryProcess_NonRootRejected verifies that retrying a descendant directly is
// rejected with an error naming the tree root, leaving the tree untouched.
func TestRetryProcess_NonRootRejected(t *testing.T) {
	for _, b := range testBackends(t) {
		t.Run(b.name, func(t *testing.T) {
			insertInst(t, b.db, "root", model.StatusFailed, "", nil, "child failed")
			insertChild(t, b.db, "child-bad", model.StatusFailed, "root", "step1", []string{"root"}, "boom")

			err := b.db.RetryProcess(context.Background(), "child-bad", false)
			if err == nil {
				t.Fatal("expected error for non-root retry, got nil")
			}
			if !strings.Contains(err.Error(), `"root"`) {
				t.Errorf("error should name the root instance, got %q", err)
			}
			if got := mustStatus(t, b.db, "child-bad"); got != model.StatusFailed {
				t.Errorf("child-bad: expected failed (untouched), got %q", got)
			}
			if got := mustStatus(t, b.db, "root"); got != model.StatusFailed {
				t.Errorf("root: expected failed (untouched), got %q", got)
			}
		})
	}
}

// TestRetryProcess_FailedTree_RevivesOnlyFailedLeaf verifies that retrying the
// root of a failed tree revives only the failed leaf and reconstructs the root
// as running+waiting, leaving completed siblings untouched.
func TestRetryProcess_FailedTree_RevivesOnlyFailedLeaf(t *testing.T) {
	for _, b := range testBackends(t) {
		t.Run(b.name, func(t *testing.T) {
			insertInst(t, b.db, "parent", model.StatusFailed, "", nil, "child failed")
			insertChild(t, b.db, "child-ok", model.StatusCompleted, "parent", "step1", []string{"parent"}, "")
			insertChild(t, b.db, "child-bad", model.StatusFailed, "parent", "step1", []string{"parent"}, "something broke")

			if err := b.db.RetryProcess(context.Background(), "parent", false); err != nil {
				t.Fatalf("RetryProcess: %v", err)
			}

			if got := mustStatus(t, b.db, "child-bad"); got != model.StatusRunning {
				t.Errorf("child-bad: expected running, got %q", got)
			}
			if got := mustError(t, b.db, "child-bad"); got != "" {
				t.Errorf("child-bad: error should be cleared, got %q", got)
			}
			if got := mustStatus(t, b.db, "child-ok"); got != model.StatusCompleted {
				t.Errorf("child-ok: expected completed (untouched), got %q", got)
			}
			if got := mustStatus(t, b.db, "parent"); got != model.StatusRunning {
				t.Errorf("parent: expected running, got %q", got)
			}
			if got := mustWaitState(t, b.db, "parent"); got != model.WaitStateWaiting {
				t.Errorf("parent: expected wait_state=waiting, got %q", got)
			}
		})
	}
}

// TestRetryProcess_FailedTree_RevivesAllFailedChildren verifies that a root
// retry revives every failed child of the pending spawn task in one pass.
func TestRetryProcess_FailedTree_RevivesAllFailedChildren(t *testing.T) {
	for _, b := range testBackends(t) {
		t.Run(b.name, func(t *testing.T) {
			insertInst(t, b.db, "parent", model.StatusFailed, "", nil, "first child error")
			insertChild(t, b.db, "child-bad-1", model.StatusFailed, "parent", "step1", []string{"parent"}, "first child error")
			insertChild(t, b.db, "child-bad-2", model.StatusFailed, "parent", "step1", []string{"parent"}, "second child error")

			if err := b.db.RetryProcess(context.Background(), "parent", false); err != nil {
				t.Fatalf("RetryProcess: %v", err)
			}

			for _, id := range []string{"child-bad-1", "child-bad-2"} {
				if got := mustStatus(t, b.db, id); got != model.StatusRunning {
					t.Errorf("%q: expected running, got %q", id, got)
				}
			}
			if got := mustStatus(t, b.db, "parent"); got != model.StatusRunning {
				t.Errorf("parent: expected running, got %q", got)
			}
			if got := mustWaitState(t, b.db, "parent"); got != model.WaitStateWaiting {
				t.Errorf("parent: expected wait_state=waiting, got %q", got)
			}
		})
	}
}

// TestRetryProcess_FailedTree_DeepChain verifies revival of a multi-level failed
// tree: the origin leaf re-runs, every intermediate ancestor returns to
// running+waiting.
func TestRetryProcess_FailedTree_DeepChain(t *testing.T) {
	for _, b := range testBackends(t) {
		t.Run(b.name, func(t *testing.T) {
			insertInst(t, b.db, "root", model.StatusFailed, "", nil, "boom")
			insertChild(t, b.db, "mid", model.StatusFailed, "root", "step1", []string{"root"}, "boom")
			insertChild(t, b.db, "leaf", model.StatusFailed, "mid", "step1", []string{"root", "mid"}, "boom")

			if err := b.db.RetryProcess(context.Background(), "root", false); err != nil {
				t.Fatalf("RetryProcess: %v", err)
			}

			if got := mustStatus(t, b.db, "leaf"); got != model.StatusRunning {
				t.Errorf("leaf: expected running, got %q", got)
			}
			if got := mustWaitState(t, b.db, "leaf"); got != model.WaitStateNone {
				t.Errorf("leaf: expected wait_state none, got %q", got)
			}
			for _, id := range []string{"root", "mid"} {
				if got := mustStatus(t, b.db, id); got != model.StatusRunning {
					t.Errorf("%q: expected running, got %q", id, got)
				}
				if got := mustWaitState(t, b.db, id); got != model.WaitStateWaiting {
					t.Errorf("%q: expected wait_state=waiting, got %q", id, got)
				}
				if got := mustError(t, b.db, id); got != "" {
					t.Errorf("%q: error should be cleared, got %q", id, got)
				}
			}
		})
	}
}

// TestRetryProcess_FailedTree_ReconstructsCollecting verifies that retrying a
// failed root whose spawn-task children all completed revives it straight to
// collecting, so the engine re-runs the lost collect.
func TestRetryProcess_FailedTree_ReconstructsCollecting(t *testing.T) {
	for _, b := range testBackends(t) {
		t.Run(b.name, func(t *testing.T) {
			insertInst(t, b.db, "root", model.StatusFailed, "", nil, "collect failed")
			insertChild(t, b.db, "c1", model.StatusCompleted, "root", "step1", []string{"root"}, "")
			insertChild(t, b.db, "c2", model.StatusCompleted, "root", "step1", []string{"root"}, "")

			if err := b.db.RetryProcess(context.Background(), "root", false); err != nil {
				t.Fatalf("RetryProcess: %v", err)
			}

			if got := mustStatus(t, b.db, "root"); got != model.StatusRunning {
				t.Errorf("root: expected running, got %q", got)
			}
			if got := mustWaitState(t, b.db, "root"); got != model.WaitStateCollecting {
				t.Errorf("root: expected wait_state=collecting, got %q", got)
			}
			for _, id := range []string{"c1", "c2"} {
				if got := mustStatus(t, b.db, id); got != model.StatusCompleted {
					t.Errorf("%q: expected completed (untouched), got %q", id, got)
				}
			}
		})
	}
}

// TestRetryProcess_Failed_RerunsPendingStep verifies that a failed instance whose
// pending task spawned nothing simply re-runs it (wait_state none).
func TestRetryProcess_Failed_RerunsPendingStep(t *testing.T) {
	for _, b := range testBackends(t) {
		t.Run(b.name, func(t *testing.T) {
			insertInst(t, b.db, "root", model.StatusFailed, "", nil, "boom")

			if err := b.db.RetryProcess(context.Background(), "root", false); err != nil {
				t.Fatalf("RetryProcess: %v", err)
			}

			if got := mustStatus(t, b.db, "root"); got != model.StatusRunning {
				t.Errorf("root: expected running, got %q", got)
			}
			if got := mustWaitState(t, b.db, "root"); got != model.WaitStateNone {
				t.Errorf("root: expected wait_state none, got %q", got)
			}
		})
	}
}

// TestRetryProcess_EmptyQueue verifies that an instance interrupted between its
// last task and the completed write revives cleanly; advance() finishes it.
func TestRetryProcess_EmptyQueue(t *testing.T) {
	for _, b := range testBackends(t) {
		t.Run(b.name, func(t *testing.T) {
			inst := &model.ProcessInstance{
				ID:          "root",
				ProcessName: "test",
				Task:        "",
				ContextData: map[string]any{},
				Status:      model.StatusFailed,
			}
			if err := b.db.SaveInstance(inst); err != nil {
				t.Fatalf("SaveInstance: %v", err)
			}

			if err := b.db.RetryProcess(context.Background(), "root", false); err != nil {
				t.Fatalf("RetryProcess: %v", err)
			}

			if got := mustStatus(t, b.db, "root"); got != model.StatusRunning {
				t.Errorf("root: expected running, got %q", got)
			}
			if got := mustWaitState(t, b.db, "root"); got != model.WaitStateNone {
				t.Errorf("root: expected wait_state none, got %q", got)
			}
		})
	}
}

// TestFailInstanceAndAncestors_LastActiveChild_WakesParent verifies that when
// the failing child is the last active member of its spawn batch, the parent
// is marked failing AND woken (wait_state ”) so the engine can settle it —
// never 'collecting': that state is reserved for all-completed batches.
func TestFailInstanceAndAncestors_LastActiveChild_WakesParent(t *testing.T) {
	for _, b := range testBackends(t) {
		t.Run(b.name, func(t *testing.T) {
			insertInstW(t, b.db, "root", model.StatusRunning, model.WaitStateWaiting, "", nil, "")
			insertChild(t, b.db, "c-done", model.StatusCompleted, "root", "step1", []string{"root"}, "")
			insertChild(t, b.db, "c-bad", model.StatusRunning, "root", "step1", []string{"root"}, "")

			child, err := b.db.GetInstance("c-bad")
			if err != nil {
				t.Fatalf("GetInstance: %v", err)
			}
			child.Status = model.StatusFailed
			child.Error = "boom"
			if err := b.db.FailInstanceAndAncestors(child); err != nil {
				t.Fatalf("FailInstanceAndAncestors: %v", err)
			}

			if got := mustStatus(t, b.db, "c-bad"); got != model.StatusFailed {
				t.Errorf("c-bad: expected failed, got %q", got)
			}
			if got := mustStatus(t, b.db, "root"); got != model.StatusFailing {
				t.Errorf("root: expected failing, got %q", got)
			}
			// All batch children terminal → parent woken so the engine can
			// claim it and settle failing → failed. The wake is to '' (not
			// 'collecting') because a failing parent never merges outputs.
			if got := mustWaitState(t, b.db, "root"); got != model.WaitStateNone {
				t.Errorf("root: expected wait_state none, got %q", got)
			}
			if msg := mustError(t, b.db, "root"); msg != "boom" {
				t.Errorf("root: expected error \"boom\", got %q", msg)
			}
		})
	}
}

// TestFailInstanceAndAncestors_SiblingStillRunning verifies that a failure with
// a still-active sibling leaves the parent failing+waiting — it drains until
// the sibling settles.
func TestFailInstanceAndAncestors_SiblingStillRunning(t *testing.T) {
	for _, b := range testBackends(t) {
		t.Run(b.name, func(t *testing.T) {
			insertInstW(t, b.db, "root", model.StatusRunning, model.WaitStateWaiting, "", nil, "")
			insertChild(t, b.db, "c-running", model.StatusRunning, "root", "step1", []string{"root"}, "")
			insertChild(t, b.db, "c-bad", model.StatusRunning, "root", "step1", []string{"root"}, "")

			child, err := b.db.GetInstance("c-bad")
			if err != nil {
				t.Fatalf("GetInstance: %v", err)
			}
			child.Status = model.StatusFailed
			child.Error = "boom"
			if err := b.db.FailInstanceAndAncestors(child); err != nil {
				t.Fatalf("FailInstanceAndAncestors: %v", err)
			}

			if got := mustStatus(t, b.db, "root"); got != model.StatusFailing {
				t.Errorf("root: expected failing, got %q", got)
			}
			if got := mustWaitState(t, b.db, "root"); got != model.WaitStateWaiting {
				t.Errorf("root: expected wait_state=waiting (sibling active), got %q", got)
			}
			if got := mustStatus(t, b.db, "c-running"); got != model.StatusRunning {
				t.Errorf("c-running: expected running (untouched), got %q", got)
			}
		})
	}
}

// TestFailInstanceAndAncestors_PausedSiblingKeepsParentWaiting verifies that a
// paused sibling counts as ACTIVE: it is live work that simply is not advancing, so
// the parent must keep waiting rather than settle a batch that has not finished.
// (This is the state that wedges a tree — see ResumeProcess.)
func TestFailInstanceAndAncestors_PausedSiblingKeepsParentWaiting(t *testing.T) {
	for _, b := range testBackends(t) {
		t.Run(b.name, func(t *testing.T) {
			insertInstW(t, b.db, "root", model.StatusRunning, model.WaitStateWaiting, "", nil, "")
			insertChild(t, b.db, "c-paused", model.StatusPaused, "root", "step1", []string{"root"}, "")
			insertChild(t, b.db, "c-bad", model.StatusRunning, "root", "step1", []string{"root"}, "")

			child, err := b.db.GetInstance("c-bad")
			if err != nil {
				t.Fatalf("GetInstance: %v", err)
			}
			child.Status = model.StatusFailed
			child.Error = "boom"
			if err := b.db.FailInstanceAndAncestors(child); err != nil {
				t.Fatalf("FailInstanceAndAncestors: %v", err)
			}

			if got := mustStatus(t, b.db, "root"); got != model.StatusFailing {
				t.Errorf("root: expected failing, got %q", got)
			}
			if got := mustWaitState(t, b.db, "root"); got != model.WaitStateWaiting {
				t.Errorf("root: expected wait_state=waiting (paused sibling still active), got %q", got)
			}
			if got := mustStatus(t, b.db, "c-paused"); got != model.StatusPaused {
				t.Errorf("c-paused: expected paused (untouched), got %q", got)
			}
		})
	}
}

// TestFinishChild_PausedParent_ArmsCollect verifies that finishing the last child of
// a paused parent arms it for the collect: a paused parent is healthy, just suspended,
// so it gets 'collecting' like a running one (its status keeps it unclaimable until
// resumed) rather than the ” a failing parent gets.
func TestFinishChild_PausedParent_ArmsCollect(t *testing.T) {
	for _, b := range testBackends(t) {
		t.Run(b.name, func(t *testing.T) {
			insertInstW(t, b.db, "parent", model.StatusPaused, model.WaitStateWaiting, "", nil, "")
			insertChild(t, b.db, "child", model.StatusRunning, "parent", "step1", []string{"parent"}, "")

			child, err := b.db.GetInstance("child")
			if err != nil {
				t.Fatalf("GetInstance: %v", err)
			}
			child.Status = model.StatusCompleted
			if err := b.db.FinishChild(child); err != nil {
				t.Fatalf("FinishChild: %v", err)
			}

			if got := mustWaitState(t, b.db, "parent"); got != model.WaitStateCollecting {
				t.Errorf("parent: expected wait_state=collecting, got %q", got)
			}
			if got := mustStatus(t, b.db, "parent"); got != model.StatusPaused {
				t.Errorf("parent: expected paused (untouched), got %q", got)
			}
		})
	}
}

// TestRetryProcess_OnlyOnce_RejectedUnlessForced verifies that retrying a
// process whose pending task is marked only_once is rejected, and that force
// overrides the protection.
func TestRetryProcess_OnlyOnce_RejectedUnlessForced(t *testing.T) {
	for _, b := range testBackends(t) {
		t.Run(b.name, func(t *testing.T) {
			trueVal := true
			// only_once now lives in the definition, not on the instance.
			saveDef(t, b.db, "oo", 1, []*model.Task{{ID: "step1", OnlyOnce: &trueVal}})
			inst := &model.ProcessInstance{
				ID:             "locked",
				ProcessName:    "oo",
				ProcessVersion: 1,
				Task:           "step1",
				ContextData:    map[string]any{},
				Status:         model.StatusFailed,
				Error:          "failed on only_once task",
			}
			if err := b.db.SaveInstance(inst); err != nil {
				t.Fatalf("SaveInstance: %v", err)
			}

			err := b.db.RetryProcess(context.Background(), "locked", false)
			if err == nil {
				t.Fatal("expected error for only_once task, got nil")
			}
			if mustStatus(t, b.db, "locked") != model.StatusFailed {
				t.Error("status should remain failed after rejected retry")
			}

			// force overrides the protection
			if err := b.db.RetryProcess(context.Background(), "locked", true); err != nil {
				t.Fatalf("RetryProcess force: %v", err)
			}
			if got := mustStatus(t, b.db, "locked"); got != model.StatusRunning {
				t.Errorf("expected running after force retry, got %q", got)
			}
		})
	}
}

// TestRetryProcess_OnlyOnceDeep_RollsBack verifies that an only_once rejection
// deep in the tree aborts the whole transaction — no node is changed.
func TestRetryProcess_OnlyOnceDeep_RollsBack(t *testing.T) {
	for _, b := range testBackends(t) {
		t.Run(b.name, func(t *testing.T) {
			trueVal := true
			// only_once now lives in the definition, not on the instance.
			saveDef(t, b.db, "oo", 1, []*model.Task{{ID: "step1", OnlyOnce: &trueVal}})
			insertInst(t, b.db, "root", model.StatusFailed, "", nil, "child failed")
			leaf := &model.ProcessInstance{
				ID:             "leaf",
				ProcessName:    "oo",
				ProcessVersion: 1,
				Task:           "step1",
				ContextData:    map[string]any{},
				Status:         model.StatusFailed,
				ParentID:       "root",
				SpawnTaskID:    "step1",
				CallStack:      []string{"root"},
				Error:          "boom",
			}
			if err := b.db.SaveInstance(leaf); err != nil {
				t.Fatalf("SaveInstance: %v", err)
			}

			err := b.db.RetryProcess(context.Background(), "root", false)
			if err == nil {
				t.Fatal("expected error for only_once leaf, got nil")
			}
			if got := mustStatus(t, b.db, "root"); got != model.StatusFailed {
				t.Errorf("root: expected failed (rolled back), got %q", got)
			}
			if got := mustStatus(t, b.db, "leaf"); got != model.StatusFailed {
				t.Errorf("leaf: expected failed (rolled back), got %q", got)
			}
			if got := mustError(t, b.db, "leaf"); got != "boom" {
				t.Errorf("leaf: error should be unchanged, got %q", got)
			}

			// force revives the whole path
			if err := b.db.RetryProcess(context.Background(), "root", true); err != nil {
				t.Fatalf("RetryProcess force: %v", err)
			}
			if got := mustStatus(t, b.db, "leaf"); got != model.StatusRunning {
				t.Errorf("leaf: expected running after force, got %q", got)
			}
			if got := mustWaitState(t, b.db, "root"); got != model.WaitStateWaiting {
				t.Errorf("root: expected wait_state=waiting after force, got %q", got)
			}
		})
	}
}

// TestFinishChild_StepScoped verifies that sibling counting is scoped to the
// spawn task: a straggler from another batch must not keep the parent waiting.
func TestFinishChild_StepScoped(t *testing.T) {
	for _, b := range testBackends(t) {
		t.Run(b.name, func(t *testing.T) {
			insertInstW(t, b.db, "parent", model.StatusRunning, model.WaitStateWaiting, "", nil, "")
			// Leftover running child from an earlier spawn task.
			insertChild(t, b.db, "old-straggler", model.StatusRunning, "parent", "taskA", []string{"parent"}, "")
			// Current batch: a single child of taskB.
			insertChild(t, b.db, "current", model.StatusRunning, "parent", "taskB", []string{"parent"}, "")

			child, err := b.db.GetInstance("current")
			if err != nil {
				t.Fatalf("GetInstance: %v", err)
			}
			child.Status = model.StatusCompleted
			if err := b.db.FinishChild(child); err != nil {
				t.Fatalf("FinishChild: %v", err)
			}

			// The taskB batch is done — parent must wake even though a taskA
			// child is still running.
			if got := mustWaitState(t, b.db, "parent"); got != model.WaitStateCollecting {
				t.Errorf("parent: expected wait_state=collecting, got %q", got)
			}
		})
	}
}

// TestChildrenForStep_StepScoped verifies that reading a task's children returns
// only that task's batch, not earlier batches spawned by the same parent.
func TestChildrenForStep_StepScoped(t *testing.T) {
	for _, b := range testBackends(t) {
		t.Run(b.name, func(t *testing.T) {
			insertInst(t, b.db, "parent", model.StatusRunning, "", nil, "")
			oldChild := &model.ProcessInstance{
				ID: "old", ProcessName: "test", Task: "",
				ContextData: map[string]any{"output": "stale"},
				Status:      model.StatusCompleted,
				ParentID:    "parent", SpawnTaskID: "taskA", CallStack: []string{"parent"},
			}
			newChild := &model.ProcessInstance{
				ID: "new", ProcessName: "test", Task: "",
				ContextData: map[string]any{"output": "fresh"},
				Status:      model.StatusCompleted,
				ParentID:    "parent", SpawnTaskID: "taskB", CallStack: []string{"parent"},
			}
			for _, c := range []*model.ProcessInstance{oldChild, newChild} {
				if err := b.db.SaveInstance(c); err != nil {
					t.Fatalf("SaveInstance %q: %v", c.ID, err)
				}
			}

			kids, err := b.db.ChildrenForTask(context.Background(), "parent", "taskB")
			if err != nil {
				t.Fatalf("ChildrenForTask: %v", err)
			}
			if len(kids) != 1 {
				t.Fatalf("expected 1 child for taskB, got %d", len(kids))
			}
			if kids[0].ID != "new" {
				t.Errorf("expected child %q, got %q", "new", kids[0].ID)
			}
			if got := kids[0].ContextData["output"]; got != "fresh" {
				t.Errorf("child output: expected %q, got %v", "fresh", got)
			}
		})
	}
}

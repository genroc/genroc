package dbtest

import (
	"context"
	"errors"
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
		State:          map[string]any{},
		Status:         status,
		ParentID:       parentID,
		CallStack:      callStack,
		ErrorMessage:   errMsg,
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
	return inst.ErrorMessage
}

func mustPhase(t *testing.T, db *dbpkg.DB, id string) model.Phase {
	t.Helper()
	inst, err := db.GetInstance(id)
	if err != nil {
		t.Fatalf("GetInstance %q: %v", id, err)
	}
	return inst.Phase
}

// insertInstW inserts an instance with an explicit phase (for testing waiting parents).
func insertInstW(t *testing.T, db *dbpkg.DB, id string, status model.Status, phase model.Phase, parentID string, callStack []string, errMsg string) {
	t.Helper()
	inst := &model.ProcessInstance{
		ID:             id,
		ProcessName:    "test",
		ProcessVersion: 1,
		Task:           "step1",
		State:          map[string]any{},
		Status:         status,
		Phase:          phase,
		ParentID:       parentID,
		CallStack:      callStack,
		ErrorMessage:   errMsg,
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

// Unleased goes straight to 'paused'; leased is only marked 'pausing', landing with that
// worker's write.
func TestPauseProcess_SingleInstance(t *testing.T) {
	for _, b := range testBackends(t) {
		t.Run(b.name, func(t *testing.T) {
			// Leased arm first: claim it while it is the only instance in the DB.
			insertInst(t, b.db, "held", model.StatusRunning, "", nil, "")
			lease(t, b.db, "held")

			if _, err := b.db.PauseProcess(context.Background(), "held", ""); err != nil {
				t.Fatalf("PauseProcess(held): %v", err)
			}
			if got := mustStatus(t, b.db, "held"); got != model.StatusPausing {
				t.Errorf("held: expected pausing (worker mid-task), got %q", got)
			}

			// Unleased arm: nothing in flight, so there is no worker write to wait
			// for — and marking it 'pausing' would strand it, since a parked
			// instance may never be claimed again.
			insertInst(t, b.db, "idle", model.StatusRunning, "", nil, "")

			if _, err := b.db.PauseProcess(context.Background(), "idle", ""); err != nil {
				t.Fatalf("PauseProcess(idle): %v", err)
			}
			if got := mustStatus(t, b.db, "idle"); got != model.StatusPaused {
				t.Errorf("idle: expected paused, got %q", got)
			}
		})
	}
}

// Pause changes only status: a child parked on its own children keeps phase='children'.
func TestPauseProcess_Descendants(t *testing.T) {
	for _, b := range testBackends(t) {
		t.Run(b.name, func(t *testing.T) {
			insertInst(t, b.db, "root", model.StatusRunning, "", nil, "")
			insertInst(t, b.db, "child1", model.StatusRunning, "root", []string{"root"}, "")
			insertInstW(t, b.db, "child2", model.StatusRunning, model.PhaseChildren, "root", []string{"root"}, "")
			// grandchild of root via child1
			insertInst(t, b.db, "gc1", model.StatusRunning, "child1", []string{"root", "child1"}, "")

			if _, err := b.db.PauseProcess(context.Background(), "root", ""); err != nil {
				t.Fatalf("PauseProcess: %v", err)
			}

			// Nothing is leased, so the whole tree settles straight to 'paused'.
			for _, id := range []string{"root", "child1", "child2", "gc1"} {
				if got := mustStatus(t, b.db, id); got != model.StatusPaused {
					t.Errorf("%q: expected paused, got %q", id, got)
				}
			}
			if got := mustPhase(t, b.db, "child2"); got != model.PhaseChildren {
				t.Errorf("child2: phase should be preserved, got %q", got)
			}
		})
	}
}

func TestPauseProcess_SkipsTerminalDescendants(t *testing.T) {
	for _, b := range testBackends(t) {
		t.Run(b.name, func(t *testing.T) {
			insertInst(t, b.db, "root", model.StatusRunning, "", nil, "")
			insertInst(t, b.db, "c-running", model.StatusRunning, "root", []string{"root"}, "")
			insertInst(t, b.db, "c-completed", model.StatusCompleted, "root", []string{"root"}, "")
			insertInst(t, b.db, "c-failed", model.StatusFailed, "root", []string{"root"}, "err")

			if _, err := b.db.PauseProcess(context.Background(), "root", ""); err != nil {
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

// A no-op that errors cannot converge when a group of ids is re-run. specs/id-list-commands.md.
func TestPauseProcess_NothingRunning(t *testing.T) {
	for _, b := range testBackends(t) {
		t.Run(b.name, func(t *testing.T) {
			insertInst(t, b.db, "root", model.StatusCompleted, "", nil, "")
			insertInst(t, b.db, "child", model.StatusCompleted, "root", []string{"root"}, "")

			res, err := b.db.PauseProcess(context.Background(), "root", "")
			if err != nil {
				t.Fatalf("pausing a settled tree must not fail: %v", err)
			}
			if res.Outcome != model.OutcomeUnchanged {
				t.Errorf("expected outcome unchanged, got %q", res.Outcome)
			}
			if res.Instances != 0 {
				t.Errorf("unchanged wrote %d instances; it must write none", res.Instances)
			}
			if res.Status != model.StatusCompleted {
				t.Errorf("expected the root's own status back, got %q", res.Status)
			}
		})
	}
}

// A leased row is only ASKED to stop, so a second pause must not report the tree stopped.
// specs/id-list-commands.md §202.
func TestPauseProcess_OutcomeAcceptedWhileDraining(t *testing.T) {
	for _, b := range testBackends(t) {
		t.Run(b.name, func(t *testing.T) {
			insertInst(t, b.db, "held", model.StatusRunning, "", nil, "")
			lease(t, b.db, "held")

			res, err := b.db.PauseProcess(context.Background(), "held", "")
			if err != nil {
				t.Fatalf("PauseProcess(held): %v", err)
			}
			if res.Outcome != model.OutcomeAccepted {
				t.Errorf("a leased row is draining, not stopped: expected accepted, got %q", res.Outcome)
			}

			// The second pause selects nothing (status is 'pausing', not 'running'). It must
			// still report the drain: reporting unchanged here would say the tree had stopped
			// while a worker was inside a task.
			again, err := b.db.PauseProcess(context.Background(), "held", "")
			if err != nil {
				t.Fatalf("PauseProcess(held) again: %v", err)
			}
			if again.Outcome != model.OutcomeAccepted {
				t.Errorf("re-pausing a draining tree: expected accepted, got %q", again.Outcome)
			}

			// An unleased tree has nothing in flight, so it really has stopped.
			insertInst(t, b.db, "idle", model.StatusRunning, "", nil, "")
			done, err := b.db.PauseProcess(context.Background(), "idle", "")
			if err != nil {
				t.Fatalf("PauseProcess(idle): %v", err)
			}
			if done.Outcome != model.OutcomeApplied {
				t.Errorf("nothing in flight: expected applied, got %q", done.Outcome)
			}
		})
	}
}

func TestPauseProcess_NonRootRejected(t *testing.T) {
	for _, b := range testBackends(t) {
		t.Run(b.name, func(t *testing.T) {
			insertInst(t, b.db, "root", model.StatusRunning, "", nil, "")
			insertInst(t, b.db, "mid", model.StatusRunning, "root", []string{"root"}, "")
			insertInst(t, b.db, "leaf", model.StatusRunning, "mid", []string{"root", "mid"}, "")

			_, err := b.db.PauseProcess(context.Background(), "leaf", "")
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

// phase, wake_at and retry_count survive verbatim: resume is a status flip, not a revival.
func TestResumeProcess_RestoresSubtree(t *testing.T) {
	for _, b := range testBackends(t) {
		t.Run(b.name, func(t *testing.T) {
			wakeAt := time.Now().Add(time.Hour).UTC().Truncate(time.Millisecond)
			// Root parked on children; the child parked on a retry backoff.
			insertInstW(t, b.db, "root", model.StatusRunning, model.PhaseChildren, "", nil, "")
			backoff := &model.ProcessInstance{
				ID:             "child",
				ProcessName:    "test",
				ProcessVersion: 1,
				Task:           "step1",
				State:          map[string]any{},
				Status:         model.StatusRunning,
				ParentID:       "root",
				CallStack:      []string{"root"},
				RetryCount:     2,
				WakeAt:         &wakeAt,
			}
			if err := b.db.SaveInstance(backoff); err != nil {
				t.Fatalf("SaveInstance child: %v", err)
			}

			if _, err := b.db.PauseProcess(context.Background(), "root", ""); err != nil {
				t.Fatalf("PauseProcess: %v", err)
			}
			if _, err := b.db.ResumeProcess(context.Background(), "root", ""); err != nil {
				t.Fatalf("ResumeProcess: %v", err)
			}

			for _, id := range []string{"root", "child"} {
				if got := mustStatus(t, b.db, id); got != model.StatusRunning {
					t.Errorf("%q: expected running, got %q", id, got)
				}
			}
			if got := mustPhase(t, b.db, "root"); got != model.PhaseChildren {
				t.Errorf("root: phase should be preserved, got %q", got)
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

// A resume issued before a pause landed un-requests it.
func TestResumeProcess_FlipsPausing(t *testing.T) {
	for _, b := range testBackends(t) {
		t.Run(b.name, func(t *testing.T) {
			insertInst(t, b.db, "root", model.StatusRunning, "", nil, "")
			lease(t, b.db, "root")
			if _, err := b.db.PauseProcess(context.Background(), "root", ""); err != nil {
				t.Fatalf("PauseProcess: %v", err)
			}
			if got := mustStatus(t, b.db, "root"); got != model.StatusPausing {
				t.Fatalf("root: expected pausing before resume, got %q", got)
			}

			if _, err := b.db.ResumeProcess(context.Background(), "root", ""); err != nil {
				t.Fatalf("ResumeProcess: %v", err)
			}
			if got := mustStatus(t, b.db, "root"); got != model.StatusRunning {
				t.Errorf("root: expected running, got %q", got)
			}
		})
	}
}

// Resuming what a failure paused would run siblings under a parent that can no longer collect
// them; retry is the verb that revives the failure and its paused branches together.
func TestResumeProcess_RefusesAFailedTree(t *testing.T) {
	for _, b := range testBackends(t) {
		t.Run(b.name, func(t *testing.T) {
			for _, root := range []model.Status{model.StatusFailing, model.StatusFailed} {
				id := "root-" + string(root)
				insertInstW(t, b.db, id, root, model.PhaseChildren, "", nil, "boom")
				insertChild(t, b.db, id+"-dead", model.StatusFailed, id, "step1", []string{id}, "boom")
				insertChild(t, b.db, id+"-paused", model.StatusPaused, id, "step1", []string{id}, "")

				_, err := b.db.ResumeProcess(context.Background(), id, "")
				if !errors.Is(err, dbpkg.ErrConflict) {
					t.Fatalf("%s root: expected ErrConflict, got %v", root, err)
				}
				if !strings.Contains(err.Error(), "retry") {
					t.Errorf("%s root: the refusal must point at retry, got %q", root, err)
				}
				if got := mustStatus(t, b.db, id+"-paused"); got != model.StatusPaused {
					t.Errorf("%s root: a refused resume still flipped the paused sibling: %q", root, got)
				}
			}
		})
	}
}

// With nothing paused, a LIVE tree is unchanged and a SETTLED one refused, decided under the
// tree lock. specs/id-list-commands.md.
func TestResumeProcess_NothingPaused(t *testing.T) {
	for _, b := range testBackends(t) {
		t.Run(b.name, func(t *testing.T) {
			insertInst(t, b.db, "root", model.StatusRunning, "", nil, "")
			insertInst(t, b.db, "child", model.StatusRunning, "root", []string{"root"}, "")

			res, err := b.db.ResumeProcess(context.Background(), "root", "")
			if err != nil {
				t.Fatalf("resuming a live tree must not fail: %v", err)
			}
			if res.Outcome != model.OutcomeUnchanged {
				t.Errorf("expected outcome unchanged, got %q", res.Outcome)
			}
			for _, id := range []string{"root", "child"} {
				if got := mustStatus(t, b.db, id); got != model.StatusRunning {
					t.Errorf("%q: expected running (untouched), got %q", id, got)
				}
			}

			// Settled is the other side of the split: the promise cannot be kept, so it stays
			// a conflict the operator has to answer with retry or a new instance.
			insertInst(t, b.db, "done", model.StatusCompleted, "", nil, "")
			if _, err := b.db.ResumeProcess(context.Background(), "done", ""); err == nil {
				t.Fatal("expected a conflict resuming a settled tree, got nil")
			} else if !errors.Is(err, dbpkg.ErrConflict) {
				t.Errorf("expected ErrConflict, got %q", err)
			} else if !strings.Contains(err.Error(), "settled") {
				t.Errorf("the message must say why it cannot advance, got %q", err)
			}
		})
	}
}

func TestResumeProcess_NonRootRejected(t *testing.T) {
	for _, b := range testBackends(t) {
		t.Run(b.name, func(t *testing.T) {
			insertInst(t, b.db, "root", model.StatusPaused, "", nil, "")
			insertInst(t, b.db, "mid", model.StatusPaused, "root", []string{"root"}, "")
			insertInst(t, b.db, "leaf", model.StatusPaused, "mid", []string{"root", "mid"}, "")

			_, err := b.db.ResumeProcess(context.Background(), "leaf", "")
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

// mustClaim asserts id is claimable now: what settles a stop its holder's own write kept.
func mustClaim(t *testing.T, db *dbpkg.DB, id, why string) *model.ProcessInstance {
	t.Helper()
	claimed, err := db.ClaimInstances("settle-worker", time.Minute, 10, dbpkg.AllowTakeover())
	if err != nil {
		t.Fatalf("ClaimInstances: %v", err)
	}
	for _, inst := range claimed {
		if inst.ID == id {
			return inst
		}
	}
	t.Fatalf("%s was not claimable: %s", id, why)
	return nil
}

// The worker's own write keeps the pause for the next claim, whose settle runs FinishChild's
// settle-up; landing it in the write would leave the ancestors draining above it forever. A real
// outcome (completed) still wins.
func TestUpdateInstance_KeepsPendingPause(t *testing.T) {
	for _, b := range testBackends(t) {
		t.Run(b.name, func(t *testing.T) {
			insertInst(t, b.db, "held", model.StatusRunning, "", nil, "")
			lease(t, b.db, "held")
			if _, err := b.db.PauseProcess(context.Background(), "held", ""); err != nil {
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
			if got := mustStatus(t, b.db, "held"); got != model.StatusPausing {
				t.Errorf("held: the worker's 'running' overwrote the pending pause, got %q", got)
			}
			mustClaim(t, b.db, "held", "a kept pause is settled by the next claim, or never")

			// A finished task writes a real outcome, which is never hidden by a pause.
			insertInst(t, b.db, "done", model.StatusRunning, "", nil, "")
			lease(t, b.db, "done")
			if _, err := b.db.PauseProcess(context.Background(), "done", ""); err != nil {
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

// The checkpoint that parks on an external task keeps the pause too: the claim predicate grants a
// draining row whatever its wake_at or park, so the next claim still settles it.
func TestUpdateInstanceProgress_KeepsPendingPause(t *testing.T) {
	for _, b := range testBackends(t) {
		t.Run(b.name, func(t *testing.T) {
			insertInst(t, b.db, "held", model.StatusRunning, "", nil, "")
			lease(t, b.db, "held")
			if _, err := b.db.PauseProcess(context.Background(), "held", ""); err != nil {
				t.Fatalf("PauseProcess: %v", err)
			}

			held, err := b.db.GetInstance("held")
			if err != nil {
				t.Fatalf("GetInstance: %v", err)
			}
			held.Phase = model.PhaseExternal
			if err := b.db.UpdateInstanceProgress(held); err != nil {
				t.Fatalf("UpdateInstanceProgress: %v", err)
			}

			if got := mustStatus(t, b.db, "held"); got != model.StatusPausing {
				t.Errorf("held: expected pausing, got %q", got)
			}
			if got := mustPhase(t, b.db, "held"); got != model.PhaseExternal {
				t.Errorf("held: expected phase=external, got %q", got)
			}
			mustClaim(t, b.db, "held", "a pause kept on an external park must still reach a worker")
		})
	}
}

// A failure must not be hidden by a pause, but phase is kept so the ancestors drain until the
// remaining children settle.
func TestFailInstanceAndAncestors_OverridesPaused(t *testing.T) {
	for _, b := range testBackends(t) {
		t.Run(b.name, func(t *testing.T) {
			insertInstW(t, b.db, "grand", model.StatusPaused, model.PhaseChildren, "", nil, "")
			insertInstW(t, b.db, "parent", model.StatusPausing, model.PhaseChildren, "grand", []string{"grand"}, "")
			// The leaf must exist as a row: its own write is the fenced one, and a write
			// that matches no row reads as a lost lease (which is correct — a vanished
			// row is equally not this worker's to write).
			insertInst(t, b.db, "leaf", model.StatusRunning, "parent", []string{"grand", "parent"}, "")
			leaf := &model.ProcessInstance{
				ID:           "leaf",
				State:        map[string]any{},
				Status:       model.StatusFailed,
				CallStack:    []string{"grand", "parent"},
				ErrorMessage: "boom",
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
				if got := mustPhase(t, b.db, id); got != model.PhaseChildren {
					t.Errorf("%q: phase should be preserved, got %q", id, got)
				}
			}
		})
	}
}

func TestFailInstanceAndAncestors_AlreadyFailed(t *testing.T) {
	for _, b := range testBackends(t) {
		t.Run(b.name, func(t *testing.T) {
			insertInst(t, b.db, "parent", model.StatusFailed, "", nil, "original error")
			insertInst(t, b.db, "leaf", model.StatusRunning, "parent", []string{"parent"}, "")
			leaf := &model.ProcessInstance{
				ID:           "leaf",
				State:        map[string]any{},
				Status:       model.StatusFailed,
				CallStack:    []string{"parent"},
				ErrorMessage: "new error",
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
				State:       map[string]any{},
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
			if got := mustPhase(t, b.db, "parent"); got != model.PhaseChildren {
				t.Errorf("parent: expected phase=waiting, got %q", got)
			}
			if got := mustStatus(t, b.db, "child"); got != model.StatusRunning {
				t.Errorf("child: expected running, got %q", got)
			}
		})
	}
}

// A pause pending at spawn is inherited, not landed: a parent that stopped here would have nothing
// below it to land and settle the ancestors draining above it. Each child settles on its first
// claim, and the last one settles the parent, which keeps its wait.
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
				State:       map[string]any{},
				ParentID:    "parent",
				CallStack:   []string{"parent"},
				Status:      model.StatusRunning,
			}

			if err := b.db.SpawnChildrenAndWait(context.Background(), parent, []*model.ProcessInstance{child}); err != nil {
				t.Fatalf("SpawnChildrenAndWait: %v", err)
			}
			if got := mustStatus(t, b.db, "parent"); got != model.StatusPausing {
				t.Errorf("parent: a pause landed at spawn strands its ancestors, got %q", got)
			}
			if got := mustStatus(t, b.db, "child"); got != model.StatusPausing {
				t.Errorf("child: expected the pending pause inherited, got %q", got)
			}

			// What settlePausing writes on the child's first claim.
			claimed := mustClaim(t, b.db, "child", "an inherited pause is settled by the child's first claim")
			claimed.Status = model.StatusPaused
			if err := b.db.FinishChild(claimed); err != nil {
				t.Fatalf("FinishChild: %v", err)
			}
			if got := mustStatus(t, b.db, "parent"); got != model.StatusPaused {
				t.Errorf("parent: the child's settle did not settle the parent, got %q", got)
			}
			if got := mustPhase(t, b.db, "parent"); got != model.PhaseChildren {
				t.Errorf("parent: a pause keeps the wait, got phase %q", got)
			}
		})
	}
}

// The cancel twin: inherited, settled by each child's claim, and the last settles the parent.
func TestSpawnChildrenAndWait_CancellingParent(t *testing.T) {
	for _, b := range testBackends(t) {
		t.Run(b.name, func(t *testing.T) {
			insertInst(t, b.db, "parent", model.StatusCancelling, "", nil, "")
			parent, err := b.db.GetInstance("parent")
			if err != nil {
				t.Fatalf("GetInstance parent: %v", err)
			}
			child := &model.ProcessInstance{
				ID: "child", ProcessName: "test", Task: "step1", State: map[string]any{},
				ParentID: "parent", CallStack: []string{"parent"}, Status: model.StatusRunning,
			}
			if err := b.db.SpawnChildrenAndWait(context.Background(), parent, []*model.ProcessInstance{child}); err != nil {
				t.Fatalf("SpawnChildrenAndWait: %v", err)
			}
			if got := mustStatus(t, b.db, "child"); got != model.StatusCancelling {
				t.Errorf("child: expected the pending cancel inherited, got %q", got)
			}
			claimed := mustClaim(t, b.db, "child", "an inherited cancel is settled by the child's first claim")
			claimed.Status = model.StatusCancelled
			if err := b.db.FinishChild(claimed); err != nil {
				t.Fatalf("FinishChild: %v", err)
			}
			if got := mustStatus(t, b.db, "parent"); got != model.StatusCancelled {
				t.Errorf("parent: the child's settle did not settle the parent, got %q", got)
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
		State:          map[string]any{},
		Status:         status,
		ParentID:       parentID,
		SpawnTaskID:    spawnTaskID,
		CallStack:      callStack,
		ErrorMessage:   errMsg,
	}
	if err := db.SaveInstance(inst); err != nil {
		t.Fatalf("insertChild %q: %v", id, err)
	}
}

// 'failing' must settle first; a suspended tree is not a failure, and retry's fresh on_error
// budget is not un-suspending's to grant.
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

				_, err := b.db.RetryProcess(context.Background(), id, false, "")
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

func TestRetryProcess_NonRootRejected(t *testing.T) {
	for _, b := range testBackends(t) {
		t.Run(b.name, func(t *testing.T) {
			insertInst(t, b.db, "root", model.StatusFailed, "", nil, "child failed")
			insertChild(t, b.db, "child-bad", model.StatusFailed, "root", "step1", []string{"root"}, "boom")

			_, err := b.db.RetryProcess(context.Background(), "child-bad", false, "")
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

// The root is reconstructed as running+waiting; completed siblings are untouched.
func TestRetryProcess_FailedTree_RevivesOnlyFailedLeaf(t *testing.T) {
	for _, b := range testBackends(t) {
		t.Run(b.name, func(t *testing.T) {
			insertInst(t, b.db, "parent", model.StatusFailed, "", nil, "child failed")
			insertChild(t, b.db, "child-ok", model.StatusCompleted, "parent", "step1", []string{"parent"}, "")
			insertChild(t, b.db, "child-bad", model.StatusFailed, "parent", "step1", []string{"parent"}, "something broke")

			if _, err := b.db.RetryProcess(context.Background(), "parent", false, ""); err != nil {
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
			if got := mustPhase(t, b.db, "parent"); got != model.PhaseChildren {
				t.Errorf("parent: expected phase=waiting, got %q", got)
			}
		})
	}
}

func TestRetryProcess_FailedTree_RevivesAllFailedChildren(t *testing.T) {
	for _, b := range testBackends(t) {
		t.Run(b.name, func(t *testing.T) {
			insertInst(t, b.db, "parent", model.StatusFailed, "", nil, "first child error")
			insertChild(t, b.db, "child-bad-1", model.StatusFailed, "parent", "step1", []string{"parent"}, "first child error")
			insertChild(t, b.db, "child-bad-2", model.StatusFailed, "parent", "step1", []string{"parent"}, "second child error")

			if _, err := b.db.RetryProcess(context.Background(), "parent", false, ""); err != nil {
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
			if got := mustPhase(t, b.db, "parent"); got != model.PhaseChildren {
				t.Errorf("parent: expected phase=waiting, got %q", got)
			}
		})
	}
}

// The origin leaf re-runs; every intermediate ancestor returns to running+waiting.
func TestRetryProcess_FailedTree_DeepChain(t *testing.T) {
	for _, b := range testBackends(t) {
		t.Run(b.name, func(t *testing.T) {
			insertInst(t, b.db, "root", model.StatusFailed, "", nil, "boom")
			insertChild(t, b.db, "mid", model.StatusFailed, "root", "step1", []string{"root"}, "boom")
			insertChild(t, b.db, "leaf", model.StatusFailed, "mid", "step1", []string{"root", "mid"}, "boom")

			if _, err := b.db.RetryProcess(context.Background(), "root", false, ""); err != nil {
				t.Fatalf("RetryProcess: %v", err)
			}

			if got := mustStatus(t, b.db, "leaf"); got != model.StatusRunning {
				t.Errorf("leaf: expected running, got %q", got)
			}
			if got := mustPhase(t, b.db, "leaf"); got != model.PhaseNone {
				t.Errorf("leaf: expected phase none, got %q", got)
			}
			for _, id := range []string{"root", "mid"} {
				if got := mustStatus(t, b.db, id); got != model.StatusRunning {
					t.Errorf("%q: expected running, got %q", id, got)
				}
				if got := mustPhase(t, b.db, id); got != model.PhaseChildren {
					t.Errorf("%q: expected phase=waiting, got %q", id, got)
				}
				if got := mustError(t, b.db, id); got != "" {
					t.Errorf("%q: error should be cleared, got %q", id, got)
				}
			}
		})
	}
}

// Every spawn-task child completed, so the root revives straight to collecting.
func TestRetryProcess_FailedTree_ReconstructsCollecting(t *testing.T) {
	for _, b := range testBackends(t) {
		t.Run(b.name, func(t *testing.T) {
			insertInst(t, b.db, "root", model.StatusFailed, "", nil, "collect failed")
			insertChild(t, b.db, "c1", model.StatusCompleted, "root", "step1", []string{"root"}, "")
			insertChild(t, b.db, "c2", model.StatusCompleted, "root", "step1", []string{"root"}, "")

			if _, err := b.db.RetryProcess(context.Background(), "root", false, ""); err != nil {
				t.Fatalf("RetryProcess: %v", err)
			}

			if got := mustStatus(t, b.db, "root"); got != model.StatusRunning {
				t.Errorf("root: expected running, got %q", got)
			}
			if got := mustPhase(t, b.db, "root"); got != model.PhaseCollecting {
				t.Errorf("root: expected phase=collecting, got %q", got)
			}
			for _, id := range []string{"c1", "c2"} {
				if got := mustStatus(t, b.db, id); got != model.StatusCompleted {
					t.Errorf("%q: expected completed (untouched), got %q", id, got)
				}
			}
		})
	}
}

// Nothing was spawned, so the pending task simply re-runs (phase none).
func TestRetryProcess_Failed_RerunsPendingStep(t *testing.T) {
	for _, b := range testBackends(t) {
		t.Run(b.name, func(t *testing.T) {
			insertInst(t, b.db, "root", model.StatusFailed, "", nil, "boom")

			if _, err := b.db.RetryProcess(context.Background(), "root", false, ""); err != nil {
				t.Fatalf("RetryProcess: %v", err)
			}

			if got := mustStatus(t, b.db, "root"); got != model.StatusRunning {
				t.Errorf("root: expected running, got %q", got)
			}
			if got := mustPhase(t, b.db, "root"); got != model.PhaseNone {
				t.Errorf("root: expected phase none, got %q", got)
			}
		})
	}
}

// Interrupted between its last task and the completed write; advance() finishes it.
func TestRetryProcess_EmptyQueue(t *testing.T) {
	for _, b := range testBackends(t) {
		t.Run(b.name, func(t *testing.T) {
			inst := &model.ProcessInstance{
				ID:          "root",
				ProcessName: "test",
				Task:        "",
				State:       map[string]any{},
				Status:      model.StatusFailed,
			}
			if err := b.db.SaveInstance(inst); err != nil {
				t.Fatalf("SaveInstance: %v", err)
			}

			if _, err := b.db.RetryProcess(context.Background(), "root", false, ""); err != nil {
				t.Fatalf("RetryProcess: %v", err)
			}

			if got := mustStatus(t, b.db, "root"); got != model.StatusRunning {
				t.Errorf("root: expected running, got %q", got)
			}
			if got := mustPhase(t, b.db, "root"); got != model.PhaseNone {
				t.Errorf("root: expected phase none, got %q", got)
			}
		})
	}
}

// Nothing in the batch is busy, so the parent settles to 'failed' in the failure's own
// transaction — woken to the empty phase, never 'collecting': a failed batch is never merged.
func TestFailInstanceAndAncestors_LastActiveChild_SettlesParent(t *testing.T) {
	for _, b := range testBackends(t) {
		t.Run(b.name, func(t *testing.T) {
			insertInstW(t, b.db, "root", model.StatusRunning, model.PhaseChildren, "", nil, "")
			insertChild(t, b.db, "c-done", model.StatusCompleted, "root", "step1", []string{"root"}, "")
			insertChild(t, b.db, "c-bad", model.StatusRunning, "root", "step1", []string{"root"}, "")

			child, err := b.db.GetInstance("c-bad")
			if err != nil {
				t.Fatalf("GetInstance: %v", err)
			}
			child.Status = model.StatusFailed
			child.ErrorMessage = "boom"
			if err := b.db.FailInstanceAndAncestors(child); err != nil {
				t.Fatalf("FailInstanceAndAncestors: %v", err)
			}

			if got := mustStatus(t, b.db, "c-bad"); got != model.StatusFailed {
				t.Errorf("c-bad: expected failed, got %q", got)
			}
			if got := mustStatus(t, b.db, "root"); got != model.StatusFailed {
				t.Errorf("root: nothing below it is busy, so it must settle now, got %q", got)
			}
			if got := mustPhase(t, b.db, "root"); got != model.PhaseNone {
				t.Errorf("root: expected phase none, got %q", got)
			}
			if msg := mustError(t, b.db, "root"); msg != "boom" {
				t.Errorf("root: expected error \"boom\", got %q", msg)
			}
		})
	}
}

// A sibling no worker holds stops at once, so the failure does not wait on work it has doomed.
func TestFailInstanceAndAncestors_PausesAnIdleSibling(t *testing.T) {
	for _, b := range testBackends(t) {
		t.Run(b.name, func(t *testing.T) {
			insertInstW(t, b.db, "root", model.StatusRunning, model.PhaseChildren, "", nil, "")
			insertChild(t, b.db, "c-running", model.StatusRunning, "root", "step1", []string{"root"}, "")
			insertChild(t, b.db, "c-bad", model.StatusRunning, "root", "step1", []string{"root"}, "")

			child, err := b.db.GetInstance("c-bad")
			if err != nil {
				t.Fatalf("GetInstance: %v", err)
			}
			child.Status = model.StatusFailed
			child.ErrorMessage = "boom"
			if err := b.db.FailInstanceAndAncestors(child); err != nil {
				t.Fatalf("FailInstanceAndAncestors: %v", err)
			}

			if got := mustStatus(t, b.db, "c-running"); got != model.StatusPaused {
				t.Errorf("c-running: a failure must pause the rest of its tree, got %q", got)
			}
			if got := mustStatus(t, b.db, "root"); got != model.StatusFailed {
				t.Errorf("root: a paused sibling is not busy, so the root settles now, got %q", got)
			}
			if got := mustError(t, b.db, "c-running"); got != "" {
				t.Errorf("c-running: a paused sibling did not fail and must carry no error, got %q", got)
			}
		})
	}
}

// A sibling a worker holds drains: the root stays 'failing' until that worker's task lands and
// the next claim settles it, and only then reaches 'failed' — so a failed root has nothing in flight.
func TestFailInstanceAndAncestors_HeldSiblingDrainsTheRoot(t *testing.T) {
	for _, b := range testBackends(t) {
		t.Run(b.name, func(t *testing.T) {
			insertInstW(t, b.db, "root", model.StatusRunning, model.PhaseChildren, "", nil, "")
			insertChild(t, b.db, "c-held", model.StatusRunning, "root", "step1", []string{"root"}, "")
			insertChild(t, b.db, "c-bad", model.StatusRunning, "root", "step1", []string{"root"}, "")
			lease(t, b.db, "c-held") // claims c-bad too: both are mid-task

			child, err := b.db.GetInstance("c-bad")
			if err != nil {
				t.Fatalf("GetInstance: %v", err)
			}
			child.Status = model.StatusFailed
			child.ErrorMessage = "boom"
			if err := b.db.FailInstanceAndAncestors(child); err != nil {
				t.Fatalf("FailInstanceAndAncestors: %v", err)
			}
			if got := mustStatus(t, b.db, "c-held"); got != model.StatusPausing {
				t.Errorf("c-held: a held sibling can only be asked to stop, got %q", got)
			}
			if got := mustStatus(t, b.db, "root"); got != model.StatusFailing {
				t.Errorf("root: must drain while a task below it is in flight, got %q", got)
			}
			if got := mustPhase(t, b.db, "root"); got != model.PhaseChildren {
				t.Errorf("root: expected phase=children while draining, got %q", got)
			}

			held, err := b.db.GetInstance("c-held")
			if err != nil {
				t.Fatalf("GetInstance: %v", err)
			}
			held.Status = model.StatusRunning // its task finished; the worker knows of no pause
			if err := b.db.UpdateInstance(held); err != nil {
				t.Fatalf("UpdateInstance: %v", err)
			}
			if got := mustStatus(t, b.db, "root"); got != model.StatusFailing {
				t.Errorf("root: settled before the sibling's pause did, got %q", got)
			}
			settle := mustClaim(t, b.db, "c-held", "the sibling's kept pause must reach a worker")
			settle.Status = model.StatusPaused
			if err := b.db.FinishChild(settle); err != nil {
				t.Fatalf("FinishChild: %v", err)
			}
			if got := mustStatus(t, b.db, "root"); got != model.StatusFailed {
				t.Errorf("root: the last busy sibling settled, so the root must too, got %q", got)
			}
			if msg := mustError(t, b.db, "root"); msg != "boom" {
				t.Errorf("root: the failure that started it must be reported, got %q", msg)
			}
		})
	}
}

// A paused sibling is not busy, so it does not hold the failure open: the tree settles to
// 'failed' and the sibling waits, paused, for a retry.
func TestFailInstanceAndAncestors_APausedSiblingDoesNotHoldTheFailure(t *testing.T) {
	for _, b := range testBackends(t) {
		t.Run(b.name, func(t *testing.T) {
			insertInstW(t, b.db, "root", model.StatusRunning, model.PhaseChildren, "", nil, "")
			insertChild(t, b.db, "c-paused", model.StatusPaused, "root", "step1", []string{"root"}, "")
			insertChild(t, b.db, "c-bad", model.StatusRunning, "root", "step1", []string{"root"}, "")

			child, err := b.db.GetInstance("c-bad")
			if err != nil {
				t.Fatalf("GetInstance: %v", err)
			}
			child.Status = model.StatusFailed
			child.ErrorMessage = "boom"
			if err := b.db.FailInstanceAndAncestors(child); err != nil {
				t.Fatalf("FailInstanceAndAncestors: %v", err)
			}

			if got := mustStatus(t, b.db, "root"); got != model.StatusFailed {
				t.Errorf("root: expected failed, got %q", got)
			}
			if got := mustStatus(t, b.db, "c-paused"); got != model.StatusPaused {
				t.Errorf("c-paused: expected paused (untouched), got %q", got)
			}
		})
	}
}

// A paused parent is healthy, so it gets 'collecting' like a running one (its status keeps it
// unclaimable) rather than a failing parent's empty phase.
func TestFinishChild_PausedParent_ArmsCollect(t *testing.T) {
	for _, b := range testBackends(t) {
		t.Run(b.name, func(t *testing.T) {
			insertInstW(t, b.db, "parent", model.StatusPaused, model.PhaseChildren, "", nil, "")
			insertChild(t, b.db, "child", model.StatusRunning, "parent", "step1", []string{"parent"}, "")

			child, err := b.db.GetInstance("child")
			if err != nil {
				t.Fatalf("GetInstance: %v", err)
			}
			child.Status = model.StatusCompleted
			if err := b.db.FinishChild(child); err != nil {
				t.Fatalf("FinishChild: %v", err)
			}

			if got := mustPhase(t, b.db, "parent"); got != model.PhaseCollecting {
				t.Errorf("parent: expected phase=collecting, got %q", got)
			}
			if got := mustStatus(t, b.db, "parent"); got != model.StatusPaused {
				t.Errorf("parent: expected paused (untouched), got %q", got)
			}
		})
	}
}

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
				State:          map[string]any{},
				Status:         model.StatusFailed,
				ErrorMessage:   "failed on only_once task",
			}
			if err := b.db.SaveInstance(inst); err != nil {
				t.Fatalf("SaveInstance: %v", err)
			}

			_, err := b.db.RetryProcess(context.Background(), "locked", false, "")
			if err == nil {
				t.Fatal("expected error for only_once task, got nil")
			}
			if mustStatus(t, b.db, "locked") != model.StatusFailed {
				t.Error("status should remain failed after rejected retry")
			}

			// force overrides the protection
			if _, err := b.db.RetryProcess(context.Background(), "locked", true, ""); err != nil {
				t.Fatalf("RetryProcess force: %v", err)
			}
			if got := mustStatus(t, b.db, "locked"); got != model.StatusRunning {
				t.Errorf("expected running after force retry, got %q", got)
			}
		})
	}
}

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
				State:          map[string]any{},
				Status:         model.StatusFailed,
				ParentID:       "root",
				SpawnTaskID:    "step1",
				CallStack:      []string{"root"},
				ErrorMessage:   "boom",
			}
			if err := b.db.SaveInstance(leaf); err != nil {
				t.Fatalf("SaveInstance: %v", err)
			}

			_, err := b.db.RetryProcess(context.Background(), "root", false, "")
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
			if _, err := b.db.RetryProcess(context.Background(), "root", true, ""); err != nil {
				t.Fatalf("RetryProcess force: %v", err)
			}
			if got := mustStatus(t, b.db, "leaf"); got != model.StatusRunning {
				t.Errorf("leaf: expected running after force, got %q", got)
			}
			if got := mustPhase(t, b.db, "root"); got != model.PhaseChildren {
				t.Errorf("root: expected phase=waiting after force, got %q", got)
			}
		})
	}
}

// A straggler from another batch must not keep the parent waiting.
func TestFinishChild_StepScoped(t *testing.T) {
	for _, b := range testBackends(t) {
		t.Run(b.name, func(t *testing.T) {
			insertInstW(t, b.db, "parent", model.StatusRunning, model.PhaseChildren, "", nil, "")
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
			if got := mustPhase(t, b.db, "parent"); got != model.PhaseCollecting {
				t.Errorf("parent: expected phase=collecting, got %q", got)
			}
		})
	}
}

// Only that task's batch, not earlier ones spawned by the same parent.
func TestChildrenForStep_StepScoped(t *testing.T) {
	for _, b := range testBackends(t) {
		t.Run(b.name, func(t *testing.T) {
			insertInst(t, b.db, "parent", model.StatusRunning, "", nil, "")
			oldChild := &model.ProcessInstance{
				ID: "old", ProcessName: "test", Task: "",
				State:    map[string]any{"output": "stale"},
				Status:   model.StatusCompleted,
				ParentID: "parent", SpawnTaskID: "taskA", CallStack: []string{"parent"},
			}
			newChild := &model.ProcessInstance{
				ID: "new", ProcessName: "test", Task: "",
				State:    map[string]any{"output": "fresh"},
				Status:   model.StatusCompleted,
				ParentID: "parent", SpawnTaskID: "taskB", CallStack: []string{"parent"},
			}
			for _, c := range []*model.ProcessInstance{oldChild, newChild} {
				if err := b.db.SaveInstance(c); err != nil {
					t.Fatalf("SaveInstance %q: %v", c.ID, err)
				}
			}

			kids, err := b.db.ChildrenForTask(context.Background(), "parent", "taskB", 0)
			if err != nil {
				t.Fatalf("ChildrenForTask: %v", err)
			}
			if len(kids) != 1 {
				t.Fatalf("expected 1 child for taskB, got %d", len(kids))
			}
			if kids[0].ID != "new" {
				t.Errorf("expected child %q, got %q", "new", kids[0].ID)
			}
			if got := kids[0].State["output"]; got != "fresh" {
				t.Errorf("child output: expected %q, got %v", "fresh", got)
			}
		})
	}
}

// The cleared slot's claim outlives the retry (`objects` is the LOADED list) and drops with the
// next write.
func TestRetryProcess_ClearsTheReportedErrorAndKeepsTheCaughtOne(t *testing.T) {
	for _, b := range testBackends(t) {
		t.Run(b.name, func(t *testing.T) {
			inst := &model.ProcessInstance{
				ID:             "root",
				ProcessName:    "test",
				ProcessVersion: 1,
				Task:           "step1",
				Status:         model.StatusFailed,
				ErrorCode:      "went_wrong",
				ErrorMessage:   "the upstream refused",
				State: map[string]any{
					// Both slots hold a value past the externalization threshold, so each one
					// takes an object claim this retry has to drop.
					model.StateLastError: map[string]any{"code": "http.500", "data": bigString("caught")},
					model.StateErrorData: map[string]any{"trace": bigString("reported")},
				},
			}
			if err := b.db.SaveInstance(inst); err != nil {
				t.Fatalf("SaveInstance: %v", err)
			}
			stored, err := b.db.GetInstance("root")
			if err != nil {
				t.Fatalf("GetInstance: %v", err)
			}
			marker := func(slot, key string) string {
				t.Helper()
				m, ok := stored.State[slot].(map[string]any)[key].(*model.ObjectRef)
				if !ok {
					t.Fatalf("setup: %s.%s is %T, want an externalized marker",
						slot, key, stored.State[slot].(map[string]any)[key])
				}
				return m.Ref
			}
			// refs[0] is the CAUGHT error's payload (kept), refs[1] the REPORTED one (cleared).
			refs := []string{marker(model.StateLastError, "data"), marker(model.StateErrorData, "trace")}

			if _, err := b.db.RetryProcess(context.Background(), "root", false, ""); err != nil {
				t.Fatalf("RetryProcess: %v", err)
			}

			revived, err := b.db.GetInstance("root")
			if err != nil {
				t.Fatalf("GetInstance after retry: %v", err)
			}
			if revived.ErrorCode != "" || revived.ErrorMessage != "" {
				t.Errorf("the reported error survived: code=%q message=%q", revived.ErrorCode, revived.ErrorMessage)
			}
			if _, ok := revived.State[model.StateErrorData]; ok {
				t.Errorf("the reported error's payload survived: %v", revived.State[model.StateErrorData])
			}
			if _, ok := revived.State[model.StateLastError]; !ok {
				t.Error("the caught error was cleared: it is state at the task being revived, not part of the conclusion being undone")
			}
			// The next write drops the reported payload's claim; releasing the caught error's
			// would leave the context pointing at content nothing holds.
			if err := b.db.UpdateInstance(revived); err != nil {
				t.Fatalf("UpdateInstance: %v", err)
			}
			for ref, want := range map[string]int64{refs[0]: 1, refs[1]: 0} {
				n, err := b.db.CountObjectRefs(ref)
				if err != nil {
					t.Fatalf("CountObjectRefs: %v", err)
				}
				if n != want {
					t.Errorf("object %s has %d owner(s), want %d", ref, n, want)
				}
			}
		})
	}
}

// saveInst inserts an instance with explicit epochs, which insertInst/insertChild cannot
// express — a looped spawn task is only representable by setting them.
func saveInst(t *testing.T, db *dbpkg.DB, inst *model.ProcessInstance) {
	t.Helper()
	if inst.State == nil {
		inst.State = map[string]any{}
	}
	if err := db.SaveInstance(inst); err != nil {
		t.Fatalf("save %q: %v", inst.ID, err)
	}
}

// A parent reconstructed onto its own batch must keep the epoch that IDENTIFIES that batch.
// Bumping it orphans every child: the collect that follows finds nothing, so a child_map
// merges {} and the instance reports 'completed' with its children's outputs silently gone.
func TestRetryProcess_KeepsBatchEpoch(t *testing.T) {
	for _, b := range testBackends(t) {
		t.Run(b.name, func(t *testing.T) {
			insertInst(t, b.db, "root", model.StatusFailed, "", nil, "boom")
			insertChild(t, b.db, "c1", model.StatusCompleted, "root", "step1", []string{"root"}, "")
			insertChild(t, b.db, "c2", model.StatusFailed, "root", "step1", []string{"root"}, "boom")

			if _, err := b.db.RetryProcess(context.Background(), "root", false, ""); err != nil {
				t.Fatalf("RetryProcess: %v", err)
			}
			root, err := b.db.GetInstance("root")
			if err != nil {
				t.Fatalf("get root: %v", err)
			}
			if root.TaskEpoch != 0 {
				t.Errorf("root task_epoch = %d, want 0: the batch is addressed by it, so a bump loses every child", root.TaskEpoch)
			}
			kids, err := b.db.ChildrenForTask(context.Background(), "root", "step1", root.TaskEpoch)
			if err != nil {
				t.Fatalf("children for task: %v", err)
			}
			if len(kids) != 2 {
				t.Errorf("batch at the revived epoch has %d children, want 2: the parent collects only what this query returns", len(kids))
			}
		})
	}
}

// Without a batch the revived task re-runs from the top, and that IS a fresh occurrence:
// an external task derives its token from task_epoch, so re-arming on the old number would
// let a result submitted against the previous arming be accepted. specs/lease-fencing.md.
func TestRetryProcess_BumpsEpochWithoutABatch(t *testing.T) {
	for _, b := range testBackends(t) {
		t.Run(b.name, func(t *testing.T) {
			insertInst(t, b.db, "root", model.StatusFailed, "", nil, "boom")

			if _, err := b.db.RetryProcess(context.Background(), "root", false, ""); err != nil {
				t.Fatalf("RetryProcess: %v", err)
			}
			root, err := b.db.GetInstance("root")
			if err != nil {
				t.Fatalf("get root: %v", err)
			}
			if root.TaskEpoch != 1 {
				t.Errorf("root task_epoch = %d, want 1: a re-armed external task must not reuse the previous arming's token", root.TaskEpoch)
			}
		})
	}
}

// A spawn task re-entered by a loop leaves an older generation under the same
// (parent_id, spawn_task_id). Only the CURRENT epoch's batch is the parent's to revive;
// an unscoped lookup revives two generations at once and reconstructs the wrong wait state.
func TestRetryProcess_ScopesBatchToCurrentEpoch(t *testing.T) {
	for _, b := range testBackends(t) {
		for _, inst := range []*model.ProcessInstance{
			{ID: "root", ProcessName: "test", ProcessVersion: 1, Task: "step1",
				Status: model.StatusFailed, TaskEpoch: 1, ErrorMessage: "boom"},
			{ID: "old", ProcessName: "test", ProcessVersion: 1, Task: "step1",
				Status: model.StatusFailed, ParentID: "root", SpawnTaskID: "step1",
				ParentTaskEpoch: 0, CallStack: []string{"root"}, ErrorMessage: "stale"},
			{ID: "cur", ProcessName: "test", ProcessVersion: 1, Task: "step1",
				Status: model.StatusFailed, ParentID: "root", SpawnTaskID: "step1",
				ParentTaskEpoch: 1, CallStack: []string{"root"}, ErrorMessage: "boom"},
		} {
			saveInst(t, b.db, inst)
		}

		if _, err := b.db.RetryProcess(context.Background(), "root", false, ""); err != nil {
			t.Fatalf("RetryProcess: %v", err)
		}
		if got := mustStatus(t, b.db, "cur"); got != model.StatusRunning {
			t.Errorf("current-epoch child is %q, want running", got)
		}
		if got := mustStatus(t, b.db, "old"); got != model.StatusFailed {
			t.Errorf("previous generation is %q, want failed (untouched): it belongs to a batch the parent has already collected", got)
		}
	}
}

// The engine re-spawns on the next collect: the replacement's input needs expression evaluation,
// which this package cannot do. specs/child-error-handling.md §12.
func TestRetryProcess_MarksRaisedBatchForRespawn(t *testing.T) {
	for _, b := range testBackends(t) {
		t.Run(b.name, func(t *testing.T) {
			saveDef(t, b.db, "test", 1, []*model.Task{
				{ID: "first", Switch: model.SwitchMap{{Goto: model.GotoEnd}}},
			})
			insertInst(t, b.db, "root", model.StatusFailed, "", nil, "unmatched raise")
			insertChild(t, b.db, "kept", model.StatusCompleted, "root", "step1", []string{"root"}, "")
			insertChild(t, b.db, "raiser", model.StatusRaised, "root", "step1", []string{"root"}, "declined")

			if _, err := b.db.RetryProcess(context.Background(), "root", false, ""); err != nil {
				t.Fatalf("RetryProcess: %v", err)
			}

			if got := mustStatus(t, b.db, "raiser"); got != model.StatusRaised {
				t.Errorf("the raised attempt is %q, want raised: it is retired by the engine, not here", got)
			}
			kids, err := b.db.ChildrenForTask(context.Background(), "root", "step1", 0)
			if err != nil {
				t.Fatalf("children for task: %v", err)
			}
			if len(kids) != 2 {
				t.Errorf("batch has %d children, want the 2 it had: this walk adds none", len(kids))
			}
			// Every child is terminal, so the parent comes back armed to COLLECT — which is
			// where the engine's admission runs and sees the grant.
			if got := mustPhase(t, b.db, "root"); got != model.PhaseCollecting {
				t.Errorf("parent phase = %q, want collecting", got)
			}
			root, err := b.db.GetInstance("root")
			if err != nil {
				t.Fatalf("get root: %v", err)
			}
			if root.State["_retry_override"] != true {
				t.Error("no override marker: without it the engine's admission consults the budget, which is spent, and the raised slot is never re-spawned")
			}
		})
	}
}

// A failure pauses the rest of its tree, so the retry that revives the failure must resume those
// rows too, keeping their place; left paused, the revived parent would wait on them forever.
func TestRetryProcess_ResumesWhatTheFailurePaused(t *testing.T) {
	for _, b := range testBackends(t) {
		t.Run(b.name, func(t *testing.T) {
			insertInst(t, b.db, "root", model.StatusFailed, "", nil, "boom")
			insertChild(t, b.db, "c-bad", model.StatusFailed, "root", "step1", []string{"root"}, "boom")
			insertChildW(t, b.db, "c-paused", model.StatusPaused, model.PhaseChildren, "root", []string{"root"})
			insertChild(t, b.db, "gc-paused", model.StatusPaused, "c-paused", "step1", []string{"root", "c-paused"}, "")

			if _, err := b.db.RetryProcess(context.Background(), "root", false, ""); err != nil {
				t.Fatalf("RetryProcess: %v", err)
			}
			for _, id := range []string{"root", "c-bad", "c-paused", "gc-paused"} {
				if got := mustStatus(t, b.db, id); got != model.StatusRunning {
					t.Errorf("%s: expected running after retry, got %q", id, got)
				}
			}
			if got := mustPhase(t, b.db, "c-paused"); got != model.PhaseChildren {
				t.Errorf("c-paused: a resumed row keeps its wait, got phase %q", got)
			}
			if got := mustPhase(t, b.db, "root"); got != model.PhaseChildren {
				t.Errorf("root: its batch has live rows again, so it waits, got phase %q", got)
			}
		})
	}
}

// One statement splits the tree: the held leaf and every row above it drain, the rest stop now.
// The leaf's settle then climbs the tree in one transaction, so the root reports the whole tree.
func TestPauseProcess_AHeldLeafDrainsItsAncestors(t *testing.T) {
	for _, b := range testBackends(t) {
		t.Run(b.name, func(t *testing.T) {
			insertInstW(t, b.db, "root", model.StatusRunning, model.PhaseChildren, "", nil, "")
			insertChildW(t, b.db, "mid1", model.StatusRunning, model.PhaseChildren, "root", []string{"root"})
			insertChildW(t, b.db, "mid2", model.StatusRunning, model.PhaseChildren, "root", []string{"root"})
			insertChild(t, b.db, "leaf1", model.StatusRunning, "mid1", "step1", []string{"root", "mid1"}, "")
			lease(t, b.db, "leaf1")
			insertChild(t, b.db, "leaf2", model.StatusRunning, "mid2", "step1", []string{"root", "mid2"}, "")

			res, err := b.db.PauseProcess(context.Background(), "root", "")
			if err != nil {
				t.Fatalf("PauseProcess: %v", err)
			}
			if res.Outcome != model.OutcomeAccepted || res.Status != model.StatusPausing {
				t.Errorf("a tree with a task in flight is only asked to stop: got %s/%s", res.Outcome, res.Status)
			}
			want := map[string]model.Status{
				"root": model.StatusPausing, "mid1": model.StatusPausing, "leaf1": model.StatusPausing,
				"mid2": model.StatusPaused, "leaf2": model.StatusPaused,
			}
			for id, w := range want {
				if got := mustStatus(t, b.db, id); got != w {
					t.Errorf("%s: expected %s, got %q", id, w, got)
				}
			}
			if again, err := b.db.PauseProcess(context.Background(), "root", ""); err != nil || again.Outcome != model.OutcomeAccepted {
				t.Errorf("a repeated pause of a draining tree must say accepted: %v %v", again.Outcome, err)
			}

			leaf, err := b.db.GetInstance("leaf1")
			if err != nil {
				t.Fatalf("GetInstance: %v", err)
			}
			leaf.Status = model.StatusRunning
			if err := b.db.UpdateInstance(leaf); err != nil {
				t.Fatalf("UpdateInstance: %v", err)
			}
			settle := mustClaim(t, b.db, "leaf1", "the leaf's kept pause must reach a worker")
			settle.Status = model.StatusPaused
			if err := b.db.FinishChild(settle); err != nil {
				t.Fatalf("FinishChild: %v", err)
			}
			for _, id := range []string{"root", "mid1", "leaf1"} {
				if got := mustStatus(t, b.db, id); got != model.StatusPaused {
					t.Errorf("%s: the leaf's settle must climb to it, got %q", id, got)
				}
			}
			for _, id := range []string{"root", "mid1"} {
				if got := mustPhase(t, b.db, id); got != model.PhaseChildren {
					t.Errorf("%s: a pause keeps the wait, got phase %q", id, got)
				}
			}
		})
	}
}

// insertChildW inserts a child with an explicit phase, spawned by its parent's step1.
func insertChildW(t *testing.T, db *dbpkg.DB, id string, status model.Status, phase model.Phase, parentID string, callStack []string) {
	t.Helper()
	if err := db.SaveInstance(&model.ProcessInstance{
		ID: id, ProcessName: "test", ProcessVersion: 1, Task: "step1", State: map[string]any{},
		Status: status, Phase: phase, ParentID: parentID, SpawnTaskID: "step1",
		CallStack: callStack,
	}); err != nil {
		t.Fatalf("insertChildW %q: %v", id, err)
	}
}

// failLeaf writes a leaf's failure with its own code, as the engine's failInstance does.
func failLeaf(t *testing.T, db *dbpkg.DB, inst *model.ProcessInstance, code string) {
	t.Helper()
	inst.Status, inst.ErrorCode, inst.ErrorMessage = model.StatusFailed, code, code
	if err := db.FailInstanceAndAncestors(inst); err != nil {
		t.Fatalf("FailInstanceAndAncestors %s: %v", inst.ID, err)
	}
}

// Two siblings fail one after the other: the root reports the first, the one that poisoned it.
// The second finds its ancestors already failed and must neither overwrite them nor error.
func TestFailInstanceAndAncestors_TheFirstFailureKeepsTheReport(t *testing.T) {
	for _, b := range testBackends(t) {
		t.Run(b.name, func(t *testing.T) {
			insertInstW(t, b.db, "root", model.StatusRunning, model.PhaseChildren, "", nil, "")
			insertChild(t, b.db, "c1", model.StatusRunning, "root", "step1", []string{"root"}, "")
			insertChild(t, b.db, "c2", model.StatusRunning, "root", "step1", []string{"root"}, "")
			lease(t, b.db, "c1") // claims c2 too: both are mid-task when the first fails

			c1, _ := b.db.GetInstance("c1")
			c2, _ := b.db.GetInstance("c2")
			failLeaf(t, b.db, c1, "first")
			if got := mustStatus(t, b.db, "root"); got != model.StatusFailing {
				t.Fatalf("root: c2 is still held, so the root must drain, got %q", got)
			}
			failLeaf(t, b.db, c2, "second")

			root, err := b.db.GetInstance("root")
			if err != nil {
				t.Fatalf("GetInstance: %v", err)
			}
			if root.Status != model.StatusFailed {
				t.Errorf("root: the last busy child failed, so the root settles, got %q", root.Status)
			}
			if root.ErrorCode != "first" || root.ErrorMessage != "first" {
				t.Errorf("root reports %q/%q, want the failure that poisoned it", root.ErrorCode, root.ErrorMessage)
			}
		})
	}
}

// root → {m1 → l1 (fails), m2 → l2 (held)}: the failure pauses the other branch, and a held row
// two levels away holds the root open until its settle climbs both levels.
func TestFailInstanceAndAncestors_AHeldCousinDrainsTheRoot(t *testing.T) {
	for _, b := range testBackends(t) {
		t.Run(b.name, func(t *testing.T) {
			insertInstW(t, b.db, "root", model.StatusRunning, model.PhaseChildren, "", nil, "")
			insertChildW(t, b.db, "m1", model.StatusRunning, model.PhaseChildren, "root", []string{"root"})
			insertChildW(t, b.db, "m2", model.StatusRunning, model.PhaseChildren, "root", []string{"root"})
			insertChild(t, b.db, "l1", model.StatusRunning, "m1", "step1", []string{"root", "m1"}, "")
			insertChild(t, b.db, "l2", model.StatusRunning, "m2", "step1", []string{"root", "m2"}, "")
			lease(t, b.db, "l1") // claims l2 too

			l1, _ := b.db.GetInstance("l1")
			failLeaf(t, b.db, l1, "boom")
			want := map[string]model.Status{
				"m1": model.StatusFailed, "root": model.StatusFailing,
				"m2": model.StatusPausing, "l2": model.StatusPausing,
			}
			for id, w := range want {
				if got := mustStatus(t, b.db, id); got != w {
					t.Errorf("after the failure, %s: expected %s, got %q", id, w, got)
				}
			}
			if got := mustError(t, b.db, "m2"); got != "" {
				t.Errorf("m2 was paused, not failed, and must carry no error: %q", got)
			}

			l2, _ := b.db.GetInstance("l2")
			l2.Status = model.StatusRunning
			if err := b.db.UpdateInstance(l2); err != nil {
				t.Fatalf("UpdateInstance: %v", err)
			}
			settle := mustClaim(t, b.db, "l2", "the cousin's kept pause must reach a worker")
			settle.Status = model.StatusPaused
			if err := b.db.FinishChild(settle); err != nil {
				t.Fatalf("FinishChild: %v", err)
			}
			for id, w := range map[string]model.Status{"m2": model.StatusPaused, "root": model.StatusFailed} {
				if got := mustStatus(t, b.db, id); got != w {
					t.Errorf("after the cousin settled, %s: expected %s, got %q", id, w, got)
				}
			}
		})
	}
}

// A worker dies holding the sibling a failure is draining. Its lease lapses, the next claim
// takes the row over and settles it, and the root still fails: a crash cannot strand the drain.
func TestFailInstanceAndAncestors_ADeadHolderStillSettles(t *testing.T) {
	for _, b := range testBackends(t) {
		t.Run(b.name, func(t *testing.T) {
			insertInstW(t, b.db, "root", model.StatusRunning, model.PhaseChildren, "", nil, "")
			insertChild(t, b.db, "c-dead", model.StatusRunning, "root", "step1", []string{"root"}, "")
			insertChild(t, b.db, "c-bad", model.StatusRunning, "root", "step1", []string{"root"}, "")
			lease(t, b.db, "c-dead") // claims c-bad too; c-dead's worker then dies

			bad, _ := b.db.GetInstance("c-bad")
			failLeaf(t, b.db, bad, "boom")
			if got := mustStatus(t, b.db, "c-dead"); got != model.StatusPausing {
				t.Fatalf("c-dead: expected pausing, got %q", got)
			}

			advance(t, 2*time.Minute) // past the dead worker's lease
			settle := mustClaim(t, b.db, "c-dead", "a lapsed holder's draining row must be reclaimable")
			if !settle.ReclaimedExpired {
				t.Errorf("the takeover must be flagged, or an interrupted only_once task goes unnoticed")
			}
			settle.Status = model.StatusPaused
			if err := b.db.FinishChild(settle); err != nil {
				t.Fatalf("FinishChild: %v", err)
			}
			if got := mustStatus(t, b.db, "root"); got != model.StatusFailed {
				t.Errorf("root: expected failed once the reclaimed sibling settled, got %q", got)
			}
		})
	}
}

// A raise is a settled outcome, so it lands a draining parent like completion does.
func TestFinishChild_ARaiseUnderADrainingParentSettlesIt(t *testing.T) {
	for _, b := range testBackends(t) {
		t.Run(b.name, func(t *testing.T) {
			insertInstW(t, b.db, "root", model.StatusRunning, model.PhaseChildren, "", nil, "")
			insertChild(t, b.db, "c-raise", model.StatusRunning, "root", "step1", []string{"root"}, "")
			insertChild(t, b.db, "c-idle", model.StatusRunning, "root", "step1", []string{"root"}, "")
			lease(t, b.db, "c-raise") // claims c-idle too
			idle, _ := b.db.GetInstance("c-idle")
			if err := b.db.UpdateInstance(idle); err != nil { // its task boundary: no longer held
				t.Fatalf("UpdateInstance: %v", err)
			}
			if _, err := b.db.PauseProcess(context.Background(), "root", ""); err != nil {
				t.Fatalf("PauseProcess: %v", err)
			}
			if got := mustStatus(t, b.db, "root"); got != model.StatusPausing {
				t.Fatalf("root: expected pausing over the held child, got %q", got)
			}

			raised, _ := b.db.GetInstance("c-raise")
			raised.Status, raised.ErrorCode = model.StatusRaised, "declined"
			if err := b.db.FinishChild(raised); err != nil {
				t.Fatalf("FinishChild: %v", err)
			}
			if got := mustStatus(t, b.db, "root"); got != model.StatusPaused {
				t.Errorf("root: its last busy child raised, so the pause settles, got %q", got)
			}
			if got := mustStatus(t, b.db, "c-raise"); got != model.StatusRaised {
				t.Errorf("c-raise: the raise must stand over the pause, got %q", got)
			}
		})
	}
}

// A child retry under a pending pause hands the pause to the replacement, as a first spawn does,
// rather than settling the parent with nothing below it to land.
func TestRespawnSlotsAndWait_InheritsAPendingPause(t *testing.T) {
	for _, b := range testBackends(t) {
		t.Run(b.name, func(t *testing.T) {
			insertInstW(t, b.db, "parent", model.StatusPausing, model.PhaseCollecting, "", nil, "")
			insertChild(t, b.db, "attempt1", model.StatusRaised, "parent", "step1", []string{"parent"}, "")
			parent, err := b.db.GetInstance("parent")
			if err != nil {
				t.Fatalf("GetInstance: %v", err)
			}
			replacement := &model.ProcessInstance{
				ID: "attempt2", ProcessName: "test", ProcessVersion: 1, Task: "step1", State: map[string]any{},
				ParentID: "parent", SpawnTaskID: "step1", CallStack: []string{"parent"}, Status: model.StatusRunning,
			}
			if err := b.db.RespawnSlotsAndWait(context.Background(), parent, []string{"attempt1"},
				[]*model.ProcessInstance{replacement}); err != nil {
				t.Fatalf("RespawnSlotsAndWait: %v", err)
			}
			if got := mustStatus(t, b.db, "attempt2"); got != model.StatusPausing {
				t.Errorf("replacement: expected the pending pause inherited, got %q", got)
			}
			if got := mustStatus(t, b.db, "parent"); got != model.StatusPausing {
				t.Errorf("parent: a pause landed at respawn strands its ancestors, got %q", got)
			}
			settle := mustClaim(t, b.db, "attempt2", "an inherited pause is settled by the first claim")
			settle.Status = model.StatusPaused
			if err := b.db.FinishChild(settle); err != nil {
				t.Fatalf("FinishChild: %v", err)
			}
			if got := mustStatus(t, b.db, "parent"); got != model.StatusPaused {
				t.Errorf("parent: the replacement's settle did not settle it, got %q", got)
			}
		})
	}
}

// A verb landing between the settling claim and its write. The settle writes 'paused' for the pause
// it saw; it must not overwrite a newer decision: a cancel lands instead, a resume stands.
func TestSettle_AVerbBetweenTheClaimAndTheWriteWins(t *testing.T) {
	for _, verb := range []string{"cancel", "resume"} {
		for _, b := range testBackends(t) {
			t.Run(b.name+"/"+verb, func(t *testing.T) {
				ctx := context.Background()
				insertInstW(t, b.db, "root", model.StatusRunning, model.PhaseChildren, "", nil, "")
				insertChild(t, b.db, "leaf", model.StatusRunning, "root", "step1", []string{"root"}, "")
				lease(t, b.db, "leaf")
				if _, err := b.db.PauseProcess(ctx, "root", ""); err != nil {
					t.Fatalf("PauseProcess: %v", err)
				}
				leaf, _ := b.db.GetInstance("leaf")
				if err := b.db.UpdateInstance(leaf); err != nil { // its task ends: the pause is kept
					t.Fatalf("UpdateInstance: %v", err)
				}
				settle := mustClaim(t, b.db, "leaf", "the kept pause must reach a worker")

				var want model.Status
				switch verb {
				case "cancel":
					if _, err := b.db.CancelProcess(ctx, "root", ""); err != nil {
						t.Fatalf("CancelProcess: %v", err)
					}
					want = model.StatusCancelled
				default:
					if _, err := b.db.ResumeProcess(ctx, "root", ""); err != nil {
						t.Fatalf("ResumeProcess: %v", err)
					}
					want = model.StatusRunning
				}

				settle.Status = model.StatusPaused // what settlePausing writes, unaware of the verb
				if err := b.db.FinishChild(settle); err != nil {
					t.Fatalf("FinishChild: %v", err)
				}
				if got := mustStatus(t, b.db, "leaf"); got != want {
					t.Errorf("leaf: the %s in the window was overwritten: got %q, want %q", verb, got, want)
				}
				if got := mustStatus(t, b.db, "root"); got != want {
					t.Errorf("root: expected %q after the %s, got %q", want, verb, got)
				}
			})
		}
	}
}

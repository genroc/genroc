package dbtest

import (
	"context"
	"strings"
	"testing"

	dbpkg "genroc/internal/db"
	"genroc/internal/model"
)

// The lifecycle consequences of 'raised' — the parts of specs/child-error-handling.md §11
// that break silently. A raise is a settled OUTCOME, not an interruption; everything here
// follows from that, but each consequence falls in a different place.

// insertRaised inserts a child that concluded with a raise, code and all.
func insertRaised(t *testing.T, db *dbpkg.DB, id, parentID, spawnTaskID, code string, callStack []string) {
	t.Helper()
	inst := &model.ProcessInstance{
		ID:             id,
		ProcessName:    "test",
		ProcessVersion: 1,
		Task:           "step1",
		State:          map[string]any{},
		Status:         model.StatusRaised,
		ParentID:       parentID,
		SpawnTaskID:    spawnTaskID,
		CallStack:      callStack,
		ErrorMessage:   "an anticipated condition",
		ErrorCode:      code,
	}
	if err := db.SaveInstance(inst); err != nil {
		t.Fatalf("insertRaised %q: %v", id, err)
	}
}

func mustErrorCode(t *testing.T, db *dbpkg.DB, id string) string {
	t.Helper()
	inst, err := db.GetInstance(id)
	if err != nil {
		t.Fatalf("GetInstance %q: %v", id, err)
	}
	return inst.ErrorCode
}

// A raised root is not retryable, and the rejection has to explain why: "not retryable"
// alone reads like a bug, since unlike a pause there is no other verb to reach for.
func TestRetryProcess_RaisedRootRejectedWithReason(t *testing.T) {
	for _, b := range testBackends(t) {
		t.Run(b.name, func(t *testing.T) {
			insertRaised(t, b.db, "root", "", "", "insufficient_funds", nil)

			_, err := b.db.RetryProcess(context.Background(), "root", false)
			if err == nil {
				t.Fatal("expected raised root to be rejected")
			}
			// The code and the reasoning both matter: the operator has to be able to tell
			// this from a fault without opening the definition.
			for _, want := range []string{"insufficient_funds", "declared outcome", "raised"} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("message should mention %q, got %q", want, err)
				}
			}
			if got := mustStatus(t, b.db, "root"); got != model.StatusRaised {
				t.Errorf("status should be unchanged, got %q", got)
			}
		})
	}
}

// A raised child is finished work, so revive keeps it exactly as it is — the same
// treatment 'completed' gets, and for the same reason. Reviving it would re-run a
// process that concluded by design.
func TestRetryProcess_RaisedChildIsKept(t *testing.T) {
	for _, b := range testBackends(t) {
		t.Run(b.name, func(t *testing.T) {
			// A failed parent that was collecting a batch of one raised child: the
			// parent failed *at resolution* because no rule matched the code.
			insertInstW(t, b.db, "parent", model.StatusFailed, model.WaitStateNone, "", nil, "no rule matched")
			insertRaised(t, b.db, "kid", "parent", "step1", "card_declined", []string{"parent"})

			if _, err := b.db.RetryProcess(context.Background(), "parent", false); err != nil {
				t.Fatalf("RetryProcess: %v", err)
			}

			if got := mustStatus(t, b.db, "kid"); got != model.StatusRaised {
				t.Errorf("raised child should be kept, got %q", got)
			}
			if got := mustErrorCode(t, b.db, "kid"); got != "card_declined" {
				t.Errorf("raised child should keep its code, got %q", got)
			}
			if got := mustStatus(t, b.db, "parent"); got != model.StatusRunning {
				t.Errorf("parent should be revived, got %q", got)
			}
		})
	}
}

// The §11.4 bug: revive asks "after revival, is anything still active?" to rebuild the
// parent's wait_state. A raised child is settled, so the answer must be no — otherwise
// the parent is parked in 'waiting' forever on a child that has already concluded, and
// nothing logs why. This is the one failure mode here that is silent and unrecoverable.
func TestRetryProcess_RaisedChildDoesNotStrandParentInWaiting(t *testing.T) {
	for _, b := range testBackends(t) {
		t.Run(b.name, func(t *testing.T) {
			insertInstW(t, b.db, "parent", model.StatusFailed, model.WaitStateNone, "", nil, "boom")
			// Batch of the parent's current task: one raised, one completed.
			insertRaised(t, b.db, "kid-raised", "parent", "step1", "out_of_stock", []string{"parent"})
			insertChild(t, b.db, "kid-done", model.StatusCompleted, "parent", "step1", []string{"parent"}, "")

			if _, err := b.db.RetryProcess(context.Background(), "parent", false); err != nil {
				t.Fatalf("RetryProcess: %v", err)
			}

			// Since §12 the raised slot is re-spawned, so the batch is no longer settled and
			// the parent waits. What must stay true is what the old assertion was really
			// protecting: it waits only because something in the batch can still run.
			if got := mustWaitState(t, b.db, "parent"); got != model.WaitStateWaiting {
				t.Fatalf("parent should wait for the replacement, got wait_state %q", got)
			}
			kids, err := b.db.ChildrenForTask(context.Background(), "parent", "step1", 0)
			if err != nil {
				t.Fatalf("children for task: %v", err)
			}
			active := 0
			for _, k := range kids {
				if !k.Status.Terminal() {
					active++
				}
			}
			if active == 0 {
				t.Fatal("parent is waiting on a batch where nothing can run — wedged forever")
			}
		})
	}
}

// Reviving clears the REPORTED error and keeps the CAUGHT one. Leaving error_code behind
// fails quietly: the instance goes on to complete and still reports having died of the old
// code, corrupting exactly the column the code exists to serve. Clearing the caught error
// fails just as quietly in the other direction — see the assertion below.
func TestRetryProcess_ClearsErrorCodeAndErrorData(t *testing.T) {
	for _, b := range testBackends(t) {
		t.Run(b.name, func(t *testing.T) {
			inst := &model.ProcessInstance{
				ID:             "root",
				ProcessName:    "test",
				ProcessVersion: 1,
				Task:           "step1",
				State: map[string]any{
					"error": map[string]any{"task": "step1", "code": "http.500", "message": "boom"},
				},
				Status:       model.StatusFailed,
				ErrorMessage: "task \"step1\": http.500: boom",
				ErrorCode:    "http.500",
			}
			if err := b.db.SaveInstance(inst); err != nil {
				t.Fatalf("SaveInstance: %v", err)
			}

			if _, err := b.db.RetryProcess(context.Background(), "root", false); err != nil {
				t.Fatalf("RetryProcess: %v", err)
			}

			revived, err := b.db.GetInstance("root")
			if err != nil {
				t.Fatalf("GetInstance: %v", err)
			}
			if revived.ErrorCode != "" {
				t.Errorf("error_code should be cleared, got %q", revived.ErrorCode)
			}
			if revived.ErrorMessage != "" {
				t.Errorf("error should be cleared, got %q", revived.ErrorMessage)
			}
			// The CAUGHT error is kept: it is this instance's state at the task it stopped on
			// (migration 034), and a revived node can stand on a task reachable only through
			// on_error, where mustErr/mayErr promises an `error` exists.
			if got := revived.State["error"]; got == nil {
				t.Error("`error` should survive: clearing it hands a revived handler task a null the analysis says it can never see")
			}
		})
	}
}

// The SQL half of Status.Terminal(). CountActiveSiblings decides when a parent wakes,
// and it is a separate list from the Go predicate — the two are kept in step by hand,
// so a change to one that misses the other hangs the parent.
func TestFinishChild_RaisedSiblingWakesParent(t *testing.T) {
	for _, b := range testBackends(t) {
		t.Run(b.name, func(t *testing.T) {
			insertInstW(t, b.db, "parent", model.StatusRunning, model.WaitStateWaiting, "", nil, "")
			insertRaised(t, b.db, "kid-a", "parent", "step1", "out_of_stock", []string{"parent"})

			// The second child finishes last; with 'raised' counted as settled this is the
			// completion that empties the batch and wakes the parent.
			kidB := &model.ProcessInstance{
				ID: "kid-b", ProcessName: "test", ProcessVersion: 1, Task: "step1",
				State: map[string]any{}, Status: model.StatusRunning,
				ParentID: "parent", SpawnTaskID: "step1", CallStack: []string{"parent"},
			}
			if err := b.db.SaveInstance(kidB); err != nil {
				t.Fatalf("SaveInstance kid-b: %v", err)
			}
			kidB.Status = model.StatusCompleted
			if err := b.db.FinishChild(kidB); err != nil {
				t.Fatalf("FinishChild: %v", err)
			}

			if got := mustWaitState(t, b.db, "parent"); got != model.WaitStateCollecting {
				t.Errorf("parent should be armed for collect, got %q "+
					"(a raised sibling counted as active leaves it waiting forever)", got)
			}
		})
	}
}

// A settled outcome is never reopened. FailAncestors deliberately omits 'raised' from
// the statuses it can flip to 'failing' — the asymmetry with CountActiveSiblings above
// is the point: 'raised' is terminal for "is the batch done" but is not a failure, so
// it neither poisons upward nor is poisoned from above.
func TestFailInstanceAndAncestors_DoesNotReopenRaised(t *testing.T) {
	for _, b := range testBackends(t) {
		t.Run(b.name, func(t *testing.T) {
			insertRaised(t, b.db, "root", "", "", "declined", nil)
			insertInst(t, b.db, "kid", model.StatusRunning, "root", []string{"root"}, "")

			kid, err := b.db.GetInstance("kid")
			if err != nil {
				t.Fatalf("GetInstance: %v", err)
			}
			kid.Status = model.StatusFailed
			kid.ErrorMessage = "boom"
			kid.ErrorCode = "http.500"
			if err := b.db.FailInstanceAndAncestors(kid); err != nil {
				t.Fatalf("FailInstanceAndAncestors: %v", err)
			}

			if got := mustStatus(t, b.db, "root"); got != model.StatusRaised {
				t.Errorf("a settled raised ancestor must not be reopened, got %q", got)
			}
			if got := mustErrorCode(t, b.db, "root"); got != "declined" {
				t.Errorf("its code must survive too, got %q", got)
			}
		})
	}
}

// A poisoned tree is filterable by the code of the failure that started it, not only at
// the one instance that observed it.
func TestFailInstanceAndAncestors_PropagatesErrorCode(t *testing.T) {
	for _, b := range testBackends(t) {
		t.Run(b.name, func(t *testing.T) {
			insertInst(t, b.db, "root", model.StatusRunning, "", nil, "")
			insertInst(t, b.db, "kid", model.StatusRunning, "root", []string{"root"}, "")

			kid, err := b.db.GetInstance("kid")
			if err != nil {
				t.Fatalf("GetInstance: %v", err)
			}
			kid.Status = model.StatusFailed
			kid.ErrorMessage = "the service rejected it"
			kid.ErrorCode = "submit_contract_violation"
			if err := b.db.FailInstanceAndAncestors(kid); err != nil {
				t.Fatalf("FailInstanceAndAncestors: %v", err)
			}

			if got := mustStatus(t, b.db, "root"); got != model.StatusFailing {
				t.Fatalf("ancestor should be failing, got %q", got)
			}
			if got := mustErrorCode(t, b.db, "root"); got != "submit_contract_violation" {
				t.Errorf("ancestor should inherit the code, got %q", got)
			}
		})
	}
}

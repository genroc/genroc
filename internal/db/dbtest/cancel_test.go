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

// The same split as pause, for the same reason: a divergence is a bug in whichever moved.
func TestCancelProcess_LeasedVersusParked(t *testing.T) {
	for _, b := range testBackends(t) {
		t.Run(b.name, func(t *testing.T) {
			insertInst(t, b.db, "held", model.StatusRunning, "", nil, "")
			lease(t, b.db, "held")

			if _, err := b.db.CancelProcess(context.Background(), "held", ""); err != nil {
				t.Fatalf("CancelProcess(held): %v", err)
			}
			if got := mustStatus(t, b.db, "held"); got != model.StatusCancelling {
				t.Errorf("held: expected cancelling (worker mid-task), got %q", got)
			}

			insertInst(t, b.db, "idle", model.StatusRunning, "", nil, "")
			if _, err := b.db.CancelProcess(context.Background(), "idle", ""); err != nil {
				t.Fatalf("CancelProcess(idle): %v", err)
			}
			if got := mustStatus(t, b.db, "idle"); got != model.StatusCancelled {
				t.Errorf("idle: expected cancelled, got %q", got)
			}
		})
	}
}

// A paused tree is live work only cancel can dispose of; pause's 'running'-only selector would
// leave exactly those rows.
func TestCancelProcess_TakesAPausedTree(t *testing.T) {
	for _, b := range testBackends(t) {
		t.Run(b.name, func(t *testing.T) {
			insertInst(t, b.db, "root", model.StatusRunning, "", nil, "")
			insertInstW(t, b.db, "kid", model.StatusRunning, model.PhaseNone, "root", []string{"root"}, "")
			if _, err := b.db.PauseProcess(context.Background(), "root", ""); err != nil {
				t.Fatalf("PauseProcess: %v", err)
			}

			res, err := b.db.CancelProcess(context.Background(), "root", "")
			if err != nil {
				t.Fatalf("CancelProcess: %v", err)
			}
			if res.Outcome != model.OutcomeApplied {
				t.Errorf("nothing was in flight: expected applied, got %q", res.Outcome)
			}
			for _, id := range []string{"root", "kid"} {
				if got := mustStatus(t, b.db, id); got != model.StatusCancelled {
					t.Errorf("%s: a paused row must cancel, got %q", id, got)
				}
			}
		})
	}
}

// A finished process stays finished: the completed work really did happen.
func TestCancelProcess_LeavesTerminalRowsAlone(t *testing.T) {
	for _, b := range testBackends(t) {
		t.Run(b.name, func(t *testing.T) {
			insertInst(t, b.db, "root", model.StatusRunning, "", nil, "")
			insertInst(t, b.db, "done", model.StatusCompleted, "root", []string{"root"}, "")
			insertInst(t, b.db, "dead", model.StatusFailed, "root", []string{"root"}, "")

			if _, err := b.db.CancelProcess(context.Background(), "root", ""); err != nil {
				t.Fatalf("CancelProcess: %v", err)
			}
			if got := mustStatus(t, b.db, "done"); got != model.StatusCompleted {
				t.Errorf("a completed child must stay completed, got %q", got)
			}
			if got := mustStatus(t, b.db, "dead"); got != model.StatusFailed {
				t.Errorf("a failed child must stay failed, got %q", got)
			}
		})
	}
}

// CountDrainingInTree must count cancel's own draining state; pause's would answer about the
// wrong verb.
func TestCancelProcess_ReportsADrainingTree(t *testing.T) {
	for _, b := range testBackends(t) {
		t.Run(b.name, func(t *testing.T) {
			insertInst(t, b.db, "held", model.StatusRunning, "", nil, "")
			lease(t, b.db, "held")

			first, err := b.db.CancelProcess(context.Background(), "held", "")
			if err != nil {
				t.Fatalf("CancelProcess: %v", err)
			}
			if first.Outcome != model.OutcomeAccepted {
				t.Errorf("a leased row is draining, not stopped: expected accepted, got %q", first.Outcome)
			}
			again, err := b.db.CancelProcess(context.Background(), "held", "")
			if err != nil {
				t.Fatalf("CancelProcess again: %v", err)
			}
			if again.Outcome != model.OutcomeAccepted {
				t.Errorf("re-cancelling a draining tree: expected accepted, got %q", again.Outcome)
			}
		})
	}
}

// An assertion, so re-running a group of ids converges. specs/id-list-commands.md.
func TestCancelProcess_SettledTreeIsUnchanged(t *testing.T) {
	for _, b := range testBackends(t) {
		t.Run(b.name, func(t *testing.T) {
			insertInst(t, b.db, "done", model.StatusCompleted, "", nil, "")
			res, err := b.db.CancelProcess(context.Background(), "done", "")
			if err != nil {
				t.Fatalf("CancelProcess: %v", err)
			}
			if res.Outcome != model.OutcomeUnchanged {
				t.Errorf("a settled tree satisfies the assertion: expected unchanged, got %q", res.Outcome)
			}
		})
	}
}

// inTree matches root_id, so a descendant id would silently select nothing.
func TestCancelProcess_NonRootRejected(t *testing.T) {
	for _, b := range testBackends(t) {
		t.Run(b.name, func(t *testing.T) {
			insertInst(t, b.db, "root", model.StatusRunning, "", nil, "")
			insertInst(t, b.db, "kid", model.StatusRunning, "root", []string{"root"}, "")

			_, err := b.db.CancelProcess(context.Background(), "kid", "")
			if err == nil {
				t.Fatal("cancelling a descendant must be refused in favour of its root")
			}
			if !strings.Contains(err.Error(), "root") {
				t.Errorf("the refusal must name the root to call instead, got %q", err)
			}
			if got := mustStatus(t, b.db, "kid"); got != model.StatusRunning {
				t.Errorf("a refused cancel must write nothing, got %q", got)
			}
		})
	}
}

// Retry revives a tree whose DEFINITION ran out of attempts; an operator's stop was never an
// attempt.
func TestRetryProcess_RefusesCancelled(t *testing.T) {
	for _, b := range testBackends(t) {
		t.Run(b.name, func(t *testing.T) {
			insertInst(t, b.db, "stopped", model.StatusRunning, "", nil, "")
			if _, err := b.db.CancelProcess(context.Background(), "stopped", ""); err != nil {
				t.Fatalf("CancelProcess: %v", err)
			}
			_, err := b.db.RetryProcess(context.Background(), "stopped", false, "")
			if err == nil {
				t.Fatal("a cancelled tree must not be retryable; that conflation is what 022 removed")
			}
			if !strings.Contains(err.Error(), "cancel") {
				t.Errorf("the refusal must say it was cancelled, got %q", err)
			}
			if got := mustStatus(t, b.db, "stopped"); got != model.StatusCancelled {
				t.Errorf("a refused retry must write nothing, got %q", got)
			}
		})
	}
}

// The heartbeat is the ONLY channel to a running worker, and a cancelled claim must not be
// renewed.
func TestRenewExternalClaims_ReportsCancelled(t *testing.T) {
	for _, b := range testBackends(t) {
		t.Run(b.name, func(t *testing.T) {
			ctx := context.Background()
			insertExternalParked(t, b.db, "inst-cancel", 0, nil)
			insertExternalParked(t, b.db, "inst-live", 0, nil)
			if _, err := b.db.ClaimExternalTasks("w1", claimLease, 2, "", 0, ""); err != nil {
				t.Fatalf("claim: %v", err)
			}
			cancelBefore, _ := b.db.GetInstance("inst-cancel")
			liveBefore, _ := b.db.GetInstance("inst-live")
			// Without this the renewal recomputes the SAME expiry it granted, and the "was not
			// extended" assertion below holds whether or not the guard exists.
			dbpkg.AdvanceClock(claimLease / 3)
			if _, err := b.db.CancelProcess(ctx, "inst-cancel", ""); err != nil {
				t.Fatalf("CancelProcess: %v", err)
			}

			out, err := b.db.RenewExternalClaims(ctx, "w1",
				[]string{"inst-cancel", "inst-live"}, claimLease)
			if err != nil {
				t.Fatalf("renew: %v", err)
			}
			if len(out.Cancelled) != 1 || out.Cancelled[0] != "inst-cancel" {
				t.Errorf("the cancelled claim must be named so the worker can stop THAT one: %+v", out)
			}
			if len(out.Renewed) != 1 || out.Renewed[0] != "inst-live" {
				t.Errorf("a live claim beside a cancelled one must still renew: %+v", out)
			}

			after, _ := b.db.GetInstance("inst-cancel")
			if !after.ExternalLeaseExpiresAt.Equal(*cancelBefore.ExternalLeaseExpiresAt) {
				t.Error("a cancelled claim's lease was extended; it must be left to lapse")
			}
			// The control: the renewal really did move a lease this round, so the assertion
			// above is about the guard rather than about a clock that never advanced.
			liveAfter, _ := b.db.GetInstance("inst-live")
			if !liveAfter.ExternalLeaseExpiresAt.After(*liveBefore.ExternalLeaseExpiresAt) {
				t.Fatal("no lease moved at all; this round proves nothing about the cancelled one")
			}
			// The holder stays: cleared, the next renewal would answer `lost`, which tells the
			// worker to stop WITHOUT releasing.
			if after.ExternalWorkerID == nil {
				t.Error("cancel cleared the external holder; the heartbeat then cannot say 'cancelled'")
			}
		})
	}
}

// The worker may be mid-answer when the cancel lands; taking it writes to a row nobody reads.
func TestResolveExternalTask_RefusedAfterCancel(t *testing.T) {
	for _, b := range testBackends(t) {
		t.Run(b.name, func(t *testing.T) {
			ctx := context.Background()
			insertExternalParked(t, b.db, "inst-late", 0, nil)
			claimed, err := b.db.ClaimExternalTasks("w1", claimLease, 1, "", 0, "")
			if err != nil || len(claimed) != 1 {
				t.Fatalf("claim: got %d err=%v", len(claimed), err)
			}
			if _, err := b.db.CancelProcess(ctx, "inst-late", ""); err != nil {
				t.Fatalf("CancelProcess: %v", err)
			}
			err = b.db.ResolveExternalTask(ctx, "inst-late", claimed[0].TaskEpoch,
				dbpkg.BoundToClaim(claimed[0].ExternalClaimEpoch),
				model.ExternalOutcome{Result: map[string]any{"ok": true}})
			if !errors.Is(err, dbpkg.ErrConflict) {
				t.Fatalf("a cancelled instance took an outcome (err=%v); refusing is what tells the worker to stop", err)
			}
		})
	}
}

// The silent one: without the CASE the row sits in 'cancelling' until a reclaim, and a parked
// row is never reclaimed.
func TestUpdateInstance_LandsPendingCancel(t *testing.T) {
	for _, b := range testBackends(t) {
		t.Run(b.name, func(t *testing.T) {
			insertInst(t, b.db, "held", model.StatusRunning, "", nil, "")
			lease(t, b.db, "held")
			if _, err := b.db.CancelProcess(context.Background(), "held", ""); err != nil {
				t.Fatalf("CancelProcess(held): %v", err)
			}
			held, err := b.db.GetInstance("held")
			if err != nil {
				t.Fatalf("GetInstance held: %v", err)
			}
			held.Status = model.StatusRunning // the worker knows nothing about the cancel
			if err := b.db.UpdateInstance(held); err != nil {
				t.Fatalf("UpdateInstance held: %v", err)
			}
			if got := mustStatus(t, b.db, "held"); got != model.StatusCancelled {
				t.Errorf("held: expected cancelled, got %q", got)
			}

			// A finished task writes a real outcome, and a cancel does not hide it -- the same
			// rule pause follows, because the work genuinely happened.
			insertInst(t, b.db, "done", model.StatusRunning, "", nil, "")
			lease(t, b.db, "done")
			if _, err := b.db.CancelProcess(context.Background(), "done", ""); err != nil {
				t.Fatalf("CancelProcess(done): %v", err)
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

// The checkpoint also PARKS on a delay or external task, so a cancel not landing here leaves
// 'cancelling' on a row no later claim can settle.
func TestUpdateInstanceProgress_LandsPendingCancel(t *testing.T) {
	for _, b := range testBackends(t) {
		t.Run(b.name, func(t *testing.T) {
			insertInst(t, b.db, "held", model.StatusRunning, "", nil, "")
			lease(t, b.db, "held")
			if _, err := b.db.CancelProcess(context.Background(), "held", ""); err != nil {
				t.Fatalf("CancelProcess: %v", err)
			}
			held, err := b.db.GetInstance("held")
			if err != nil {
				t.Fatalf("GetInstance: %v", err)
			}
			held.Phase = model.PhaseExternal
			if err := b.db.UpdateInstanceProgress(held); err != nil {
				t.Fatalf("UpdateInstanceProgress: %v", err)
			}
			if got := mustStatus(t, b.db, "held"); got != model.StatusCancelled {
				t.Errorf("held: expected cancelled, got %q", got)
			}
			// The park survives the landing write: ReleaseExternalClaim finds a claim by
			// phase='external', and releasing is what the heartbeat tells a cancelled worker.
			after, err := b.db.GetInstance("held")
			if err != nil {
				t.Fatalf("GetInstance: %v", err)
			}
			if after.Phase != model.PhaseExternal {
				t.Errorf("a landed cancel must keep the park it was stopped in, got %q", after.Phase)
			}
		})
	}
}

// Migration 045 as behaviour: a draining row is LEASED, so a dead holder's row settles only by a
// reclaim; a terminal row must never advance.
func TestCancelStatesAreClaimableOrNot(t *testing.T) {
	for _, b := range testBackends(t) {
		t.Run(b.name, func(t *testing.T) {
			// Draining: leased, then its holder's lease lapses -- the crash-recovery shape.
			insertInst(t, b.db, "draining", model.StatusRunning, "", nil, "")
			lease(t, b.db, "draining")
			if _, err := b.db.CancelProcess(context.Background(), "draining", ""); err != nil {
				t.Fatalf("CancelProcess(draining): %v", err)
			}
			insertInst(t, b.db, "stopped", model.StatusRunning, "", nil, "")
			if _, err := b.db.CancelProcess(context.Background(), "stopped", ""); err != nil {
				t.Fatalf("CancelProcess(stopped): %v", err)
			}
			dbpkg.AdvanceClock(time.Hour) // the dead worker's lease lapses

			claimed, err := b.db.ClaimInstances("w2", time.Minute, 10, dbpkg.AllowTakeover())
			if err != nil {
				t.Fatalf("ClaimInstances: %v", err)
			}
			got := map[string]bool{}
			for _, c := range claimed {
				got[c.ID] = true
			}
			if !got["draining"] {
				t.Error("a 'cancelling' row was not reclaimable; a worker that died holding one would strand it forever")
			}
			if got["stopped"] {
				t.Error("a 'cancelled' row was claimed; nothing may advance a stopped process")
			}
		})
	}
}

// The landing CASE checks 'pausing' before 'cancelling'; the later, more final cancel must win,
// or an operator who asked to stop the tree must resume it to get rid of it.
func TestCancelOverAPendingPause(t *testing.T) {
	for _, b := range testBackends(t) {
		t.Run(b.name, func(t *testing.T) {
			insertInst(t, b.db, "held", model.StatusRunning, "", nil, "")
			lease(t, b.db, "held")
			if _, err := b.db.PauseProcess(context.Background(), "held", ""); err != nil {
				t.Fatalf("PauseProcess: %v", err)
			}
			if got := mustStatus(t, b.db, "held"); got != model.StatusPausing {
				t.Fatalf("setup: expected pausing, got %q", got)
			}
			if _, err := b.db.CancelProcess(context.Background(), "held", ""); err != nil {
				t.Fatalf("CancelProcess: %v", err)
			}
			if got := mustStatus(t, b.db, "held"); got != model.StatusCancelling {
				t.Fatalf("cancel must take a draining pause: got %q", got)
			}

			held, err := b.db.GetInstance("held")
			if err != nil {
				t.Fatalf("GetInstance: %v", err)
			}
			held.Status = model.StatusRunning // the worker knows about neither request
			if err := b.db.UpdateInstance(held); err != nil {
				t.Fatalf("UpdateInstance: %v", err)
			}
			if got := mustStatus(t, b.db, "held"); got != model.StatusCancelled {
				t.Errorf("the later, final decision must land: expected cancelled, got %q", got)
			}
		})
	}
}

// Pause selects 'running' only, so a cancelled tree is unchanged: a stop has no way out.
func TestPauseDoesNotReopenACancelledTree(t *testing.T) {
	for _, b := range testBackends(t) {
		t.Run(b.name, func(t *testing.T) {
			insertInst(t, b.db, "stopped", model.StatusRunning, "", nil, "")
			if _, err := b.db.CancelProcess(context.Background(), "stopped", ""); err != nil {
				t.Fatalf("CancelProcess: %v", err)
			}
			res, err := b.db.PauseProcess(context.Background(), "stopped", "")
			if err != nil {
				t.Fatalf("PauseProcess: %v", err)
			}
			if res.Outcome != model.OutcomeUnchanged {
				t.Errorf("pausing a cancelled tree: expected unchanged, got %q", res.Outcome)
			}
			if got := mustStatus(t, b.db, "stopped"); got != model.StatusCancelled {
				t.Errorf("a cancelled tree must stay cancelled, got %q", got)
			}
		})
	}
}

// Cancel writes status only, as pause does. ReleaseExternalClaim finds a claim by
// phase='external', so a cleared park would make a cancelled worker's release impossible.
func TestCancelledRowKeepsItsPark(t *testing.T) {
	for _, b := range testBackends(t) {
		t.Run(b.name, func(t *testing.T) {
			insertInstW(t, b.db, "parked", model.StatusRunning, model.PhaseChildren, "", nil, "")
			if _, err := b.db.CancelProcess(context.Background(), "parked", ""); err != nil {
				t.Fatalf("CancelProcess: %v", err)
			}
			got, err := b.db.GetInstance("parked")
			if err != nil {
				t.Fatalf("GetInstance: %v", err)
			}
			if got.Status != model.StatusCancelled {
				t.Errorf("expected cancelled, got %q", got.Status)
			}
			if got.Phase != model.PhaseChildren {
				t.Errorf("cancel must write status alone, got phase %q", got.Phase)
			}
		})
	}
}

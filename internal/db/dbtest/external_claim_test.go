package dbtest

import (
	"context"
	"sync"
	"testing"
	"time"

	dbpkg "genroc/internal/db"
	"genroc/internal/model"
)

const (
	claimLease = 30 * time.Second
	// Short, so a test can expire a claim by advancing the shared DB clock just past it: the
	// offset only ever grows, so every test pays for every other test's jump.
	shortLease = time.Second
)

// SKIP LOCKED's property, invisible to a single-engine test: an overlap means two workers run the
// same task's side effects.
func TestClaimExternalTasks_DisjointUnderConcurrency(t *testing.T) {
	for _, b := range testBackends(t) {
		t.Run(b.name, func(t *testing.T) {
			const n = 12
			for i := range n {
				insertExternalParked(t, b.db, id(i), 0, nil)
			}

			var wg sync.WaitGroup
			got := make([][]*model.ProcessInstance, 2)
			errs := make([]error, 2)
			for w := range 2 {
				wg.Add(1)
				go func() {
					defer wg.Done()
					got[w], errs[w] = b.db.ClaimExternalTasks(workerName(w), claimLease, n, "", 0, "")
				}()
			}
			wg.Wait()

			seen := map[string]string{}
			for w := range 2 {
				if errs[w] != nil {
					t.Fatalf("worker %d: %v", w, errs[w])
				}
				for _, inst := range got[w] {
					if prev, dup := seen[inst.ID]; dup {
						t.Fatalf("%s was claimed by both %s and %s — concurrent claims must be disjoint",
							inst.ID, prev, workerName(w))
					}
					seen[inst.ID] = workerName(w)
				}
			}
			if len(seen) != n {
				t.Fatalf("claimed %d of %d tasks; the two workers should partition the queue", len(seen), n)
			}
		})
	}
}

// Touching the engine's lease columns would lock the holder out of its own answer, delay
// external.timeout and forge only_once evidence; moving task_epoch voids every handle out.
func TestClaimExternalTasks_LeavesEngineColumnsAlone(t *testing.T) {
	for _, b := range testBackends(t) {
		t.Run(b.name, func(t *testing.T) {
			insertExternalParked(t, b.db, "inst-cols", 7, nil)
			before, err := b.db.GetInstance("inst-cols")
			if err != nil {
				t.Fatalf("GetInstance: %v", err)
			}

			if _, err := b.db.ClaimExternalTasks("w1", claimLease, 1, "", 0, ""); err != nil {
				t.Fatalf("claim: %v", err)
			}
			after, err := b.db.GetInstance("inst-cols")
			if err != nil {
				t.Fatalf("GetInstance: %v", err)
			}

			if after.WorkerID != nil {
				t.Errorf("worker_id = %v, want nil — a claim is not an engine lease", *after.WorkerID)
			}
			if after.LeaseExpiresAt != nil {
				t.Errorf("lease_expires_at = %v, want nil — a claim must not hold the engine off this row", after.LeaseExpiresAt)
			}
			if after.LeaseEpoch != before.LeaseEpoch {
				t.Errorf("lease_epoch %d -> %d; only ClaimInstances may move it", before.LeaseEpoch, after.LeaseEpoch)
			}
			if after.TaskEpoch != before.TaskEpoch {
				t.Errorf("task_epoch %d -> %d; a claim is not a new occurrence, and moving it voids every handed-out handle",
					before.TaskEpoch, after.TaskEpoch)
			}
			if after.ExternalClaimEpoch != before.ExternalClaimEpoch+1 {
				t.Errorf("external_claim_epoch %d -> %d, want +1: a claim is a grant",
					before.ExternalClaimEpoch, after.ExternalClaimEpoch)
			}
		})
	}
}

// And the other direction: an expiry nobody took over still lets the late holder answer.
func TestClaimExternalTasks_ExpiryReclaimAndFencing(t *testing.T) {
	for _, b := range testBackends(t) {
		t.Run(b.name, func(t *testing.T) {
			ctx := context.Background()
			insertExternalParked(t, b.db, "inst-fence", 3, nil)

			first, err := b.db.ClaimExternalTasks("w1", shortLease, 1, "", 0, "")
			if err != nil || len(first) != 1 {
				t.Fatalf("first claim: got %d err=%v", len(first), err)
			}
			firstEpoch := first[0].ExternalClaimEpoch

			// A live claim is not on offer to anyone else.
			if again, err := b.db.ClaimExternalTasks("w2", claimLease, 1, "", 0, ""); err != nil || len(again) != 0 {
				t.Fatalf("second claim while live: got %d err=%v, want none", len(again), err)
			}

			// Expire it: the row becomes claimable again, and NOTHING was written to make that
			// happen — expiry is the absence of a live lease, not an event.
			expire(t)
			second, err := b.db.ClaimExternalTasks("w2", claimLease, 1, "", 0, "")
			if err != nil || len(second) != 1 {
				t.Fatalf("re-claim after expiry: got %d err=%v", len(second), err)
			}
			if second[0].ExternalClaimEpoch != firstEpoch+1 {
				t.Fatalf("re-claim epoch %d, want %d: a re-claim is a new grant", second[0].ExternalClaimEpoch, firstEpoch+1)
			}
			if second[0].ExternalWorkerID == nil || *second[0].ExternalWorkerID != "w2" {
				t.Fatalf("holder = %v, want w2", second[0].ExternalWorkerID)
			}

			// The first holder is now fenced: its handle names a grant that has been superseded.
			err = b.db.ResolveExternalTask(ctx, "inst-fence", 3, dbpkg.BoundToClaim(firstEpoch),
				model.ExternalOutcome{Result: map[string]any{"late": true}})
			if err == nil {
				t.Fatal("a superseded claim's answer was accepted; the re-claim must fence it out")
			}

			// The live holder answers fine.
			if err := b.db.ResolveExternalTask(ctx, "inst-fence", 3, dbpkg.BoundToClaim(second[0].ExternalClaimEpoch),
				model.ExternalOutcome{Result: map[string]any{"ok": true}}); err != nil {
				t.Fatalf("the live holder's answer was refused: %v", err)
			}
		})
	}
}

// Re-claim, not expiry, invalidates a handle: discarding work already done is strictly worse.
func TestResolveExternalTask_LateHolderStillAnswers(t *testing.T) {
	for _, b := range testBackends(t) {
		t.Run(b.name, func(t *testing.T) {
			ctx := context.Background()
			insertExternalParked(t, b.db, "inst-late", 1, nil)
			claimed, err := b.db.ClaimExternalTasks("w1", shortLease, 1, "", 0, "")
			if err != nil || len(claimed) != 1 {
				t.Fatalf("claim: got %d err=%v", len(claimed), err)
			}
			expire(t) // lease gone, but nobody re-claimed

			if err := b.db.ResolveExternalTask(ctx, "inst-late", 1, dbpkg.BoundToClaim(claimed[0].ExternalClaimEpoch),
				model.ExternalOutcome{Result: map[string]any{"ok": true}}); err != nil {
				t.Fatalf("an expired but un-taken-over claim was refused: %v", err)
			}
		})
	}
}

// A two-part token must not answer over a live claim, yet must work once it is gone: that keeps
// the approval-UI path unaffected by claiming.
func TestResolveExternalTask_UnclaimedHandleVsLiveClaim(t *testing.T) {
	for _, b := range testBackends(t) {
		t.Run(b.name, func(t *testing.T) {
			ctx := context.Background()
			insertExternalParked(t, b.db, "inst-un", 2, nil)
			if _, err := b.db.ClaimExternalTasks("w1", shortLease, 1, "", 0, ""); err != nil {
				t.Fatalf("claim: %v", err)
			}

			err := b.db.ResolveExternalTask(ctx, "inst-un", 2, dbpkg.Unclaimed,
				model.ExternalOutcome{Result: map[string]any{"ok": true}})
			if err == nil {
				t.Fatal("an unclaimed handle answered over a live claim")
			}

			expire(t)
			if err := b.db.ResolveExternalTask(ctx, "inst-un", 2, dbpkg.Unclaimed,
				model.ExternalOutcome{Result: map[string]any{"ok": true}}); err != nil {
				t.Fatalf("an unclaimed handle was refused once the claim lapsed: %v", err)
			}
		})
	}
}

// No epoch bump (it would fence the worker out of its own answer), and scoped to the holder.
func TestRenewExternalClaims(t *testing.T) {
	for _, b := range testBackends(t) {
		t.Run(b.name, func(t *testing.T) {
			ctx := context.Background()
			insertExternalParked(t, b.db, "inst-renew", 0, nil)
			claimed, err := b.db.ClaimExternalTasks("w1", time.Second, 1, "", 0, "")
			if err != nil || len(claimed) != 1 {
				t.Fatalf("claim: got %d err=%v", len(claimed), err)
			}
			epoch := claimed[0].ExternalClaimEpoch

			out, err := b.db.RenewExternalClaims(ctx, "w1", []string{"inst-renew"}, claimLease)
			if err != nil || len(out.Renewed) != 1 || out.Renewed[0] != "inst-renew" {
				t.Fatalf("renew: %+v err=%v, want inst-renew renewed", out, err)
			}
			after, _ := b.db.GetInstance("inst-renew")
			if after.ExternalClaimEpoch != epoch {
				t.Fatalf("renew moved the claim epoch %d -> %d; it must extend the grant, not re-grant it",
					epoch, after.ExternalClaimEpoch)
			}
			if after.ExternalWorkerID == nil {
				t.Fatal("renew cleared the holder; an unlisted row must expire with it intact")
			}

			// Scoped to the holder: a stranger's renewal touches nothing, and the answer names
			// the id as lost rather than staying silent about it.
			if out, err := b.db.RenewExternalClaims(ctx, "w2", []string{"inst-renew"}, claimLease); err != nil ||
				len(out.Renewed) != 0 || len(out.Lost) != 1 {
				t.Fatalf("renew by a non-holder: %+v err=%v, want it reported lost", out, err)
			}
		})
	}
}

// Unlike an expiry, a release bumps the epoch: the releaser's handle must stop at once.
func TestReleaseExternalClaim(t *testing.T) {
	for _, b := range testBackends(t) {
		t.Run(b.name, func(t *testing.T) {
			ctx := context.Background()
			insertExternalParked(t, b.db, "inst-rel", 5, nil)
			claimed, err := b.db.ClaimExternalTasks("w1", claimLease, 1, "", 0, "")
			if err != nil || len(claimed) != 1 {
				t.Fatalf("claim: got %d err=%v", len(claimed), err)
			}
			epoch := claimed[0].ExternalClaimEpoch

			if err := b.db.ReleaseExternalClaim(ctx, "inst-rel", 5, epoch); err != nil {
				t.Fatalf("release: %v", err)
			}
			again, err := b.db.ClaimExternalTasks("w2", claimLease, 1, "", 0, "")
			if err != nil || len(again) != 1 {
				t.Fatalf("re-claim after release: got %d err=%v — a release returns the task at once", len(again), err)
			}
			if err := b.db.ResolveExternalTask(ctx, "inst-rel", 5, dbpkg.BoundToClaim(epoch),
				model.ExternalOutcome{Result: map[string]any{"late": true}}); err == nil {
				t.Fatal("a released claim's handle still answered; releasing must void it at once")
			}
			// Releasing twice is a conflict, not a silent no-op.
			if err := b.db.ReleaseExternalClaim(ctx, "inst-rel", 5, epoch); err == nil {
				t.Fatal("releasing a claim already handed back was accepted")
			}
		})
	}
}

// The task's own timeout outranks the claim: an answer to work the engine is about to fail can
// no longer be accepted.
func TestClaimExternalTasks_SkipsTasksPastTheirDeadline(t *testing.T) {
	for _, b := range testBackends(t) {
		t.Run(b.name, func(t *testing.T) {
			past := dbpkg.Now().Add(-time.Minute)
			insertExternalParked(t, b.db, "inst-due", 0, &past)
			future := dbpkg.Now().Add(time.Hour)
			insertExternalParked(t, b.db, "inst-live", 0, &future)

			got, err := b.db.ClaimExternalTasks("w1", claimLease, 10, "", 0, "")
			if err != nil {
				t.Fatalf("claim: %v", err)
			}
			for _, inst := range got {
				if inst.ID == "inst-due" {
					t.Fatal("claimed a task whose deadline had passed; the engine owes it external.timeout")
				}
			}
			if len(got) != 1 || got[0].ID != "inst-live" {
				t.Fatalf("claimed %d tasks, want just inst-live", len(got))
			}
		})
	}
}

func id(i int) string         { return "inst-c" + string(rune('a'+i)) }
func workerName(w int) string { return "w" + string(rune('1'+w)) }

// expire lets a shortLease claim lapse by moving the DB clock past it. Nothing is WRITTEN to
// make the row claimable again — that is the design ("expiry alone writes nothing"), and a
// helper that updated the row would quietly test a mechanism the code does not have.
func expire(t *testing.T) {
	t.Helper()
	dbpkg.AdvanceClock(2 * shortLease)
}

// Passes with no production code, which is why it exists: nothing else notices the day the claim
// is "simplified" back onto worker_id/lease_expires_at.
func TestExternalClaim_DoesNotDelayTheEngineTimeout(t *testing.T) {
	for _, b := range testBackends(t) {
		t.Run(b.name, func(t *testing.T) {
			// Deadline already passed, so the engine owes this row external.timeout now.
			past := dbpkg.Now().Add(-time.Minute)
			insertExternalParked(t, b.db, "inst-clock", 0, &past)
			// Claim it out of band. ClaimExternalTasks would refuse a task past its deadline,
			// so write the claim directly: the point is a HOLDER on the row, however it got there.
			claimDirect(t, b.db, "inst-clock", "w1", claimLease)

			got, err := b.db.ClaimInstances("engine-1", time.Minute, 10, dbpkg.AllowTakeover())
			if err != nil {
				t.Fatalf("ClaimInstances: %v", err)
			}
			for _, inst := range got {
				if inst.ID == "inst-clock" {
					return // the engine took it despite the live claim, which is the property
				}
			}
			t.Fatal("a live external claim held the engine off the row; the task deadline is authoritative and external.timeout must fire on time")
		})
	}
}

// The engines derive ExternalReclaimed differently, Postgres through a hand-written scan list
// that must track instanceColumns; the e2e covers only one.
func TestClaimExternalTasks_ReportsAReclaim(t *testing.T) {
	for _, b := range testBackends(t) {
		t.Run(b.name, func(t *testing.T) {
			insertExternalParked(t, b.db, "inst-recl", 0, nil)

			first, err := b.db.ClaimExternalTasks("w1", shortLease, 1, "", 0, "")
			if err != nil || len(first) != 1 {
				t.Fatalf("first claim: got %d err=%v", len(first), err)
			}
			if first[0].ExternalReclaimed {
				t.Fatal("a first claim reported a re-claim; nothing held this row before")
			}

			expire(t)
			second, err := b.db.ClaimExternalTasks("w2", shortLease, 1, "", 0, "")
			if err != nil || len(second) != 1 {
				t.Fatalf("re-claim: got %d err=%v", len(second), err)
			}
			if !second[0].ExternalReclaimed {
				t.Fatal("a re-claim after a lapsed hold did not report it; on an only_once task this is what stops the work being run twice")
			}
		})
	}
}

// A signal carries no handle to fence with, so a live claim gets what a live lease gets: buffering.
func TestDeliverSignal_DefersToALiveClaim(t *testing.T) {
	for _, b := range testBackends(t) {
		t.Run(b.name, func(t *testing.T) {
			ctx := context.Background()
			insertExternalParked(t, b.db, "inst-sigclaim", 0, nil)

			// Armed and unclaimed: delivered outright.
			delivered, err := b.db.DeliverSignal(ctx, "inst-sigclaim", "approval",
				model.ExternalOutcome{Result: map[string]any{"n": 1}})
			if err != nil || !delivered {
				t.Fatalf("armed+unclaimed: delivered=%v err=%v, want delivered", delivered, err)
			}

			// Now park it again and put a live claim on it.
			insertExternalParked(t, b.db, "inst-sigclaim2", 0, nil)
			if _, err := b.db.ClaimExternalTasks("w1", claimLease, 1, "", 0, ""); err != nil {
				t.Fatalf("claim: %v", err)
			}
			delivered, err = b.db.DeliverSignal(ctx, "inst-sigclaim2", "approval",
				model.ExternalOutcome{Result: map[string]any{"n": 2}})
			if err != nil {
				t.Fatalf("signal under a live claim: %v", err)
			}
			if delivered {
				t.Fatal("a signal answered over a live claim; it must buffer instead, as it does for the engine's own lease")
			}
		})
	}
}

// claimDirect writes a claim onto a row the claim API would not offer, so a test can put a
// holder on an already-due task. It writes only the claim columns — the same three
// ClaimExternalTasks writes — so it cannot accidentally prove a property by touching more.
func claimDirect(t *testing.T, db *dbpkg.DB, instanceID, worker string, dur time.Duration) {
	t.Helper()
	if err := db.ClaimExternalTaskDirect(context.Background(), instanceID, worker, dur); err != nil {
		t.Fatalf("claimDirect: %v", err)
	}
}

// A signal under a live claim buffers without un-parking, so the claim's end must hand the answer
// to the engine. specs/external-task-queue.md §Renew and release.
func TestReleaseExternalClaim_HandsABufferedAnswerToTheEngine(t *testing.T) {
	for _, b := range testBackends(t) {
		t.Run(b.name, func(t *testing.T) {
			ctx := context.Background()
			insertExternalParked(t, b.db, "inst-relsig", 4, nil)
			claimed, err := b.db.ClaimExternalTasks("w1", claimLease, 1, "", 0, "")
			if err != nil || len(claimed) != 1 {
				t.Fatalf("claim: got %d err=%v", len(claimed), err)
			}
			if delivered, err := b.db.DeliverSignal(ctx, "inst-relsig", "approval",
				model.ExternalOutcome{Result: map[string]any{"n": 1}}); err != nil || delivered {
				t.Fatalf("signal under a live claim: delivered=%v err=%v, want buffered", delivered, err)
			}

			if err := b.db.ReleaseExternalClaim(ctx, "inst-relsig", 4, claimed[0].ExternalClaimEpoch); err != nil {
				t.Fatalf("release: %v", err)
			}
			assertAnswerGoesToTheEngine(t, b.db, "inst-relsig", "release")
		})
	}
}

// Expiry writes nothing, so the next claim is where a lapse is observed: it must hand the answer
// buffered under it to the engine rather than grant the task again.
func TestClaimExternalTasks_HandsAnAnswerBufferedUnderALapsedClaimToTheEngine(t *testing.T) {
	for _, b := range testBackends(t) {
		t.Run(b.name, func(t *testing.T) {
			ctx := context.Background()
			insertExternalParked(t, b.db, "inst-lapsesig", 2, nil)
			if claimed, err := b.db.ClaimExternalTasks("w1", shortLease, 1, "", 0, ""); err != nil || len(claimed) != 1 {
				t.Fatalf("claim: got %d err=%v", len(claimed), err)
			}
			if delivered, err := b.db.DeliverSignal(ctx, "inst-lapsesig", "approval",
				model.ExternalOutcome{Result: map[string]any{"n": 1}}); err != nil || delivered {
				t.Fatalf("signal under a live claim: delivered=%v err=%v, want buffered", delivered, err)
			}

			insertExternalParked(t, b.db, "inst-lapse-plain", 0, nil) // after w1's claim, so it stays unclaimed

			expire(t)
			again, err := b.db.ClaimExternalTasks("w2", claimLease, 10, "", 0, "")
			if err != nil {
				t.Fatalf("claim after the lapse: %v", err)
			}
			for _, inst := range again {
				if inst.ID == "inst-lapsesig" {
					t.Fatal("a claim after the lapse granted the task again; with an answer buffered, FIFO " +
						"would consume it over the new worker's")
				}
			}
			if len(again) != 1 || again[0].ID != "inst-lapse-plain" {
				t.Fatalf("claim after the lapse got %d task(s), want just inst-lapse-plain: an answered row "+
					"must not strand the rest of the batch", len(again))
			}
			assertAnswerGoesToTheEngine(t, b.db, "inst-lapsesig", "a lapsed claim")
		})
	}
}

func assertAnswerGoesToTheEngine(t *testing.T, db *dbpkg.DB, instanceID, how string) {
	t.Helper()
	got, err := db.GetInstance(instanceID)
	if err != nil {
		t.Fatalf("GetInstance: %v", err)
	}
	if got.Phase != model.PhaseNone || got.WakeAt != nil {
		t.Fatalf("after %s: phase=%q wake_at=%v, want un-parked; parked, the buffered answer waits for "+
			"some later resolve, or forever with no deadline", how, got.Phase, got.WakeAt)
	}
	if c, _ := db.CountBufferedSignals(instanceID, "approval"); c != 1 {
		t.Fatalf("after %s: %d buffered, want 1; un-parking must leave the answer for phase 2", how, c)
	}
	if again, err := db.ClaimExternalTasks("w3", claimLease, 10, "", 0, ""); err != nil || len(again) != 0 {
		t.Fatalf("after %s: a worker claim got %d task(s) (err=%v), want none", how, len(again), err)
	}
	engine, err := db.ClaimInstances("engine-1", time.Minute, 10, dbpkg.AllowTakeover())
	if err != nil {
		t.Fatalf("ClaimInstances: %v", err)
	}
	for _, inst := range engine {
		if inst.ID == instanceID {
			return
		}
	}
	t.Fatalf("after %s the engine was not offered %s; nothing would consume the buffered answer", how, instanceID)
}

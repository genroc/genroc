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

// TestClaimExternalTasks_DisjointUnderConcurrency is the property SKIP LOCKED exists for, and
// the one a single-engine test cannot see: two workers claiming at once must partition the
// queue, never overlap. An overlap means two workers run the same task's side effects.
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

// TestClaimExternalTasks_LeavesEngineColumnsAlone is the separate-columns decision as an
// assertion. A claim that touched worker_id/lease_expires_at/lease_epoch would lock the holder
// out of its own answer, delay the external.timeout the engine owes at wake_at, and forge the
// worker_id evidence only_once.interrupted reads. task_epoch must not move either: it numbers
// the ARMING, and bumping it invalidates every handle already given out.
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

// TestClaimExternalTasks_ExpiryReclaimAndFencing covers the lifecycle the design rests on: a
// live claim is not re-claimable, an expired one is, the re-claim bumps the epoch so the first
// holder is fenced out — and, in the other direction, an expiry that nobody took over still
// lets the late holder answer.
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

// TestResolveExternalTask_LateHolderStillAnswers is the other half of "re-claim, not expiry,
// invalidates a handle". A worker that overran its lease and was never taken over must still be
// able to answer: discarding work that was already done is strictly worse, and it is exactly
// how the engine treats its own late writes.
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

// TestResolveExternalTask_UnclaimedHandleVsLiveClaim: the queue hands two-part tokens to any
// caller, so one must not be able to answer over a worker mid-flight — but must still work once
// the claim is gone, which is what keeps the approval-UI path unaffected by claiming.
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

// TestRenewExternalClaims: a renewal extends a grant and must not bump the epoch (that would
// fence the worker out of its own answer), and is scoped to the holder so a worker cannot renew
// a claim it no longer owns.
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

			n, err := b.db.RenewExternalClaims(ctx, "w1", []string{"inst-renew"}, claimLease)
			if err != nil || n != 1 {
				t.Fatalf("renew: n=%d err=%v, want 1", n, err)
			}
			after, _ := b.db.GetInstance("inst-renew")
			if after.ExternalClaimEpoch != epoch {
				t.Fatalf("renew moved the claim epoch %d -> %d; it must extend the grant, not re-grant it",
					epoch, after.ExternalClaimEpoch)
			}
			if after.ExternalWorkerID == nil {
				t.Fatal("renew cleared the holder; an unlisted row must expire with it intact")
			}

			// Scoped to the holder: a stranger's renewal touches nothing.
			if n, err := b.db.RenewExternalClaims(ctx, "w2", []string{"inst-renew"}, claimLease); err != nil || n != 0 {
				t.Fatalf("renew by a non-holder: n=%d err=%v, want 0", n, err)
			}
		})
	}
}

// TestReleaseExternalClaim: the nack returns the task at once and, unlike an expiry, bumps the
// epoch — a release is deliberate, so the releasing worker's handle must stop working
// immediately rather than staying valid until someone else claims.
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

// TestClaimExternalTasks_SkipsTasksPastTheirDeadline: the task's own timeout outranks the
// claim's. Handing out work the engine is about to fail spends a worker on an answer that can
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

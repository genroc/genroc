package dbtest

// The lease fence: lease_epoch is a per-row grant count, bumped only by ClaimInstances and
// bound into every lease-holding write. Each test names the failure it guards.
// specs/lease-fencing.md.

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	dbpkg "genroc/internal/db"
	"genroc/internal/model"
)

func mustEpoch(t *testing.T, db *dbpkg.DB, id string) int64 {
	t.Helper()
	inst, err := db.GetInstance(id)
	if err != nil {
		t.Fatalf("GetInstance %q: %v", id, err)
	}
	return inst.LeaseEpoch
}

func claimOne(t *testing.T, db *dbpkg.DB, worker string, lease time.Duration) *model.ProcessInstance {
	t.Helper()
	claimed, err := db.ClaimInstances(worker, lease, 1, dbpkg.AllowTakeover())
	if err != nil || len(claimed) != 1 {
		t.Fatalf("claim by %q: err=%v, count=%d", worker, err, len(claimed))
	}
	return claimed[0]
}

// §1.1, §1.2, §1.6 — a fresh row starts at epoch 0 and is claimable; every claim bumps
// by exactly 1 and returns the new value. A grant read stale or reused would fence
// nothing: the token only works because it is granted, monotonic, and never skipped.
func TestLeaseEpoch_ClaimGrantsMonotonically(t *testing.T) {
	for _, b := range testBackends(t) {
		t.Run(b.name, func(t *testing.T) {
			insertRunning(t, b.db, "epoch-1")
			if got := mustEpoch(t, b.db, "epoch-1"); got != 0 {
				t.Fatalf("fresh row: epoch %d, want 0", got)
			}

			for want := int64(1); want <= 3; want++ {
				inst := claimOne(t, b.db, fmt.Sprintf("worker-%d", want), 10*time.Millisecond)
				if inst.LeaseEpoch != want {
					t.Errorf("claim %d returned epoch %d on the instance, want %d", want, inst.LeaseEpoch, want)
				}
				if got := mustEpoch(t, b.db, "epoch-1"); got != want {
					t.Errorf("claim %d left epoch %d in the row, want %d", want, got, want)
				}
				dbpkg.AdvanceClock(time.Second) // expire the lease so the next claim can take over
			}
		})
	}
}

// §1.3, §3.5 — the central invariant: a renewal extends a grant, it does not create
// one. If renewal bumped the epoch a worker would fence itself out every few seconds —
// and the gate's repair pass (renewing an already-expired lease nobody took) would
// destroy the very advance it rescues.
func TestLeaseEpoch_RenewalDoesNotBump(t *testing.T) {
	for _, b := range testBackends(t) {
		t.Run(b.name, func(t *testing.T) {
			insertRunning(t, b.db, "renew-1")
			inst := claimOne(t, b.db, "worker-A", 30*time.Millisecond)

			if _, err := b.db.RenewWorkerLeases("worker-A", []string{"renew-1"}, time.Minute); err != nil {
				t.Fatalf("RenewWorkerLeases: %v", err)
			}
			got, err := b.db.GetInstance("renew-1")
			if err != nil {
				t.Fatalf("GetInstance: %v", err)
			}
			if got.LeaseEpoch != inst.LeaseEpoch {
				t.Fatalf("renewal moved the epoch %d -> %d; a renewal extends a grant, it must not create one", inst.LeaseEpoch, got.LeaseEpoch)
			}
			if got.LeaseExpiresAt == nil || !got.LeaseExpiresAt.After(dbpkg.Now().Add(30*time.Second)) {
				t.Fatalf("renewal did not extend the lease: expires %v", got.LeaseExpiresAt)
			}

			// The repair pass: the lease fully lapses, nobody takes the row, and the owner
			// renews it back to life — same grant, so the in-flight advance's write stays valid.
			dbpkg.AdvanceClock(2 * time.Minute)
			if _, err := b.db.RenewWorkerLeases("worker-A", []string{"renew-1"}, time.Minute); err != nil {
				t.Fatalf("repair renewal: %v", err)
			}
			got, err = b.db.GetInstance("renew-1")
			if err != nil {
				t.Fatalf("GetInstance: %v", err)
			}
			if got.LeaseExpiresAt == nil || !got.LeaseExpiresAt.After(dbpkg.Now()) {
				t.Fatal("an expired lease nobody took was not repaired; the gate's repair pass has nothing to work with")
			}
			if got.LeaseEpoch != inst.LeaseEpoch {
				t.Fatalf("the repair bumped the epoch %d -> %d, fencing out the advance it just rescued", inst.LeaseEpoch, got.LeaseEpoch)
			}
		})
	}
}

// §1.4 — many workers race for one expired row: exactly one claim succeeds and the
// epoch moves by exactly 1. Double-granting the same epoch would hand two executors
// writes that both pass the fence.
func TestLeaseEpoch_OneGrantPerRace(t *testing.T) {
	for _, b := range testBackends(t) {
		t.Run(b.name, func(t *testing.T) {
			insertRunning(t, b.db, "race-1")

			var won atomic.Int32
			var wg sync.WaitGroup
			for w := 0; w < 8; w++ {
				wg.Add(1)
				go func(w int) {
					defer wg.Done()
					claimed, err := b.db.ClaimInstances(fmt.Sprintf("racer-%d", w), time.Minute, 1, dbpkg.AllowTakeover())
					if err != nil {
						t.Errorf("racer-%d: %v", w, err)
						return
					}
					won.Add(int32(len(claimed)))
				}(w)
			}
			wg.Wait()

			if won.Load() != 1 {
				t.Fatalf("%d claims won a single row; the grant must be exclusive", won.Load())
			}
			if got := mustEpoch(t, b.db, "race-1"); got != 1 {
				t.Fatalf("a single won race left epoch %d, want 1 (double-granted?)", got)
			}
		})
	}
}

// §1.5 — operator and external verbs are not grants: pause/resume/retry/resolve/deliver
// leave the epoch untouched. Bumping there would fence out a legitimately-running advance.
func TestLeaseEpoch_OperatorVerbsDoNotBump(t *testing.T) {
	for _, b := range testBackends(t) {
		t.Run(b.name, func(t *testing.T) {
			ctx := context.Background()

			insertRunning(t, b.db, "verbs-pause")
			if _, err := b.db.PauseProcess(ctx, "verbs-pause", ""); err != nil {
				t.Fatalf("PauseProcess: %v", err)
			}
			if _, err := b.db.ResumeProcess(ctx, "verbs-pause", ""); err != nil {
				t.Fatalf("ResumeProcess: %v", err)
			}
			if got := mustEpoch(t, b.db, "verbs-pause"); got != 0 {
				t.Errorf("pause+resume moved the epoch to %d; operator verbs are not grants", got)
			}

			insertInst(t, b.db, "verbs-retry", model.StatusFailed, "", nil, "boom")
			if _, err := b.db.RetryProcess(ctx, "verbs-retry", false, ""); err != nil {
				t.Fatalf("RetryProcess: %v", err)
			}
			if got := mustEpoch(t, b.db, "verbs-retry"); got != 0 {
				t.Errorf("retry moved the epoch to %d; revival is not a grant", got)
			}

			insertExternalParked(t, b.db, "verbs-resolve", 0, nil)
			if err := b.db.ResolveExternalTask(ctx, "verbs-resolve", 0, dbpkg.Unclaimed, model.ExternalOutcome{Result: map[string]any{"ok": true}}); err != nil {
				t.Fatalf("ResolveExternalTask: %v", err)
			}
			if got := mustEpoch(t, b.db, "verbs-resolve"); got != 0 {
				t.Errorf("resolve moved the epoch to %d; it acts on a parked row, not a grant", got)
			}

			insertExternalParked(t, b.db, "verbs-deliver", 0, nil)
			if delivered, err := b.db.DeliverSignal(ctx, "verbs-deliver", "approval", "sig-1", model.ExternalOutcome{Result: map[string]any{"n": 1}}); err != nil || !delivered {
				t.Fatalf("DeliverSignal: delivered=%v err=%v", delivered, err)
			}
			if got := mustEpoch(t, b.db, "verbs-deliver"); got != 0 {
				t.Errorf("deliver moved the epoch to %d; it acts on a parked row, not a grant", got)
			}
		})
	}
}

// takeOver expires holder's lease and grants the row to thief, returning the stale
// instance (the holder's view) for the fence tests to write with.
func takeOver(t *testing.T, db *dbpkg.DB, id string) (stale *model.ProcessInstance) {
	t.Helper()
	stale = claimOne(t, db, "victim", 10*time.Millisecond)
	if stale.ID != id {
		t.Fatalf("setup claim took %q, want %q", stale.ID, id)
	}
	dbpkg.AdvanceClock(time.Second)
	fresh := claimOne(t, db, "thief", time.Minute)
	if fresh.ID != id || fresh.LeaseEpoch != stale.LeaseEpoch+1 {
		t.Fatalf("takeover claim: id=%q epoch=%d, want %q at epoch %d", fresh.ID, fresh.LeaseEpoch, id, stale.LeaseEpoch+1)
	}
	return stale
}

// §2.2 — UpdateInstance: a stale terminal write is refused whole. The victim's
// "completed" must not land — the row belongs to the thief, whose view of it is the
// one the tree is being driven from.
func TestFence_UpdateInstance(t *testing.T) {
	for _, b := range testBackends(t) {
		t.Run(b.name, func(t *testing.T) {
			insertInst(t, b.db, "fence-upd", model.StatusRunning, "", nil, "")
			stale := takeOver(t, b.db, "fence-upd")

			stale.Status = model.StatusCompleted
			stale.Task = ""
			stale.ErrorMessage = "stale outcome"
			if err := b.db.UpdateInstance(stale); !errors.Is(err, dbpkg.ErrLeaseLost) {
				t.Fatalf("stale UpdateInstance: err=%v, want ErrLeaseLost", err)
			}

			got, err := b.db.GetInstance("fence-upd")
			if err != nil {
				t.Fatalf("GetInstance: %v", err)
			}
			if got.Status != model.StatusRunning || got.Task != "step1" || got.ErrorMessage != "" {
				t.Fatalf("refused write leaked: status=%q task=%q error=%q", got.Status, got.Task, got.ErrorMessage)
			}
			if got.WorkerID == nil || *got.WorkerID != "thief" {
				t.Fatalf("refused write disturbed the lease: worker=%v", got.WorkerID)
			}

			// The current grant writes normally — and an ordinary write does not move the epoch.
			got.Status = model.StatusCompleted
			got.Task = ""
			if err := b.db.UpdateInstance(got); err != nil {
				t.Fatalf("current-epoch UpdateInstance: %v", err)
			}
			if final, _ := b.db.GetInstance("fence-upd"); final.Status != model.StatusCompleted || final.LeaseEpoch != got.LeaseEpoch {
				t.Fatalf("current grant's write did not land cleanly: status=%q epoch=%d", final.Status, final.LeaseEpoch)
			}
		})
	}
}

// §2.1 — UpdateInstanceProgress: the common checkpoint path is fenced the same way,
// leaving task position, retry counter and wait_state to the row's new owner.
func TestFence_UpdateInstanceProgress(t *testing.T) {
	for _, b := range testBackends(t) {
		t.Run(b.name, func(t *testing.T) {
			insertInst(t, b.db, "fence-prog", model.StatusRunning, "", nil, "")
			stale := takeOver(t, b.db, "fence-prog")

			stale.RetryCount = 7
			stale.WaitState = model.WaitStateCollecting
			if err := b.db.UpdateInstanceProgress(stale); !errors.Is(err, dbpkg.ErrLeaseLost) {
				t.Fatalf("stale UpdateInstanceProgress: err=%v, want ErrLeaseLost", err)
			}

			got, err := b.db.GetInstance("fence-prog")
			if err != nil {
				t.Fatalf("GetInstance: %v", err)
			}
			if got.RetryCount != 0 || got.WaitState != model.WaitStateNone || got.Task != "step1" {
				t.Fatalf("refused checkpoint leaked: retry=%d wait=%q task=%q", got.RetryCount, got.WaitState, got.Task)
			}
		})
	}
}

// §2.3 — FinishChild: the fence sits on the child's write, and the parent wake rolls
// back with it. A stale finish that woke the parent would start a collect over a batch
// whose real state belongs to another worker.
func TestFence_FinishChild(t *testing.T) {
	for _, b := range testBackends(t) {
		t.Run(b.name, func(t *testing.T) {
			insertInstW(t, b.db, "fc-parent", model.StatusRunning, model.WaitStateWaiting, "", nil, "")
			child := &model.ProcessInstance{
				ID: "fc-child", ProcessName: "test", ProcessVersion: 1, Task: "step1",
				State: map[string]any{}, Status: model.StatusRunning,
				ParentID: "fc-parent", SpawnTaskID: "spawn", CallStack: []string{"fc-parent"},
			}
			if err := b.db.SaveInstance(child); err != nil {
				t.Fatalf("SaveInstance child: %v", err)
			}

			stale := takeOver(t, b.db, "fc-child") // the waiting parent is unclaimable, so this takes the child
			stale.Status = model.StatusCompleted
			stale.Task = ""
			if err := b.db.FinishChild(stale); !errors.Is(err, dbpkg.ErrLeaseLost) {
				t.Fatalf("stale FinishChild: err=%v, want ErrLeaseLost", err)
			}

			if got, _ := b.db.GetInstance("fc-child"); got.Status.Terminal() {
				t.Fatalf("refused FinishChild still terminated the child: %q", got.Status)
			}
			if parent, _ := b.db.GetInstance("fc-parent"); parent.WaitState != model.WaitStateWaiting {
				t.Fatalf("refused FinishChild still woke the parent: wait_state=%q", parent.WaitState)
			}
		})
	}
}

// §2.4 — FailInstanceAndAncestors: the worst partial effect. A fence at the wrong
// nesting level would let 'failing' poison the ancestors while the child write rolled
// back — a half-failed tree no verdict was ever written for.
func TestFence_FailInstanceAndAncestors(t *testing.T) {
	for _, b := range testBackends(t) {
		t.Run(b.name, func(t *testing.T) {
			insertInstW(t, b.db, "fa-root", model.StatusRunning, model.WaitStateWaiting, "", nil, "")
			child := &model.ProcessInstance{
				ID: "fa-child", ProcessName: "test", ProcessVersion: 1, Task: "step1",
				State: map[string]any{}, Status: model.StatusRunning,
				ParentID: "fa-root", SpawnTaskID: "spawn", CallStack: []string{"fa-root"},
			}
			if err := b.db.SaveInstance(child); err != nil {
				t.Fatalf("SaveInstance child: %v", err)
			}

			stale := takeOver(t, b.db, "fa-child")
			stale.Status = model.StatusFailed
			stale.ErrorMessage = "stale failure"
			stale.ErrorCode = "http.500"
			if err := b.db.FailInstanceAndAncestors(stale); !errors.Is(err, dbpkg.ErrLeaseLost) {
				t.Fatalf("stale FailInstanceAndAncestors: err=%v, want ErrLeaseLost", err)
			}

			if got, _ := b.db.GetInstance("fa-child"); got.Status == model.StatusFailed {
				t.Fatal("refused write still failed the child")
			}
			root, _ := b.db.GetInstance("fa-root")
			if root.Status != model.StatusRunning || root.ErrorMessage != "" {
				t.Fatalf("no ancestor may flip on a refused child write: status=%q error=%q", root.Status, root.ErrorMessage)
			}
			if root.WaitState != model.WaitStateWaiting {
				t.Fatalf("refused write still woke the parent: wait_state=%q", root.WaitState)
			}
		})
	}
}

// §2.5 — SpawnChildrenAndWait: zero children on a stale spawn. There is no world where
// the children exist and the parent was never parked.
func TestFence_SpawnChildrenAndWait(t *testing.T) {
	for _, b := range testBackends(t) {
		t.Run(b.name, func(t *testing.T) {
			insertRunning(t, b.db, "sp-parent")
			stale := takeOver(t, b.db, "sp-parent")

			child := &model.ProcessInstance{
				ID: "sp-child", ProcessName: "test", ProcessVersion: 1, Task: "step1",
				State: map[string]any{}, Status: model.StatusRunning,
				ParentID: "sp-parent", SpawnTaskID: "step1", CallStack: []string{"sp-parent"},
			}
			err := b.db.SpawnChildrenAndWait(context.Background(), stale, []*model.ProcessInstance{child})
			if !errors.Is(err, dbpkg.ErrLeaseLost) {
				t.Fatalf("stale SpawnChildrenAndWait: err=%v, want ErrLeaseLost", err)
			}

			if _, err := b.db.GetInstance("sp-child"); !errors.Is(err, dbpkg.ErrNotFound) {
				t.Fatalf("a refused spawn left a child behind: err=%v", err)
			}
			if parent, _ := b.db.GetInstance("sp-parent"); parent.WaitState != model.WaitStateNone {
				t.Fatalf("a refused spawn still parked the parent: wait_state=%q", parent.WaitState)
			}
		})
	}
}

// §2.6, §2.7 — ArmExternalOrConsumeSignal. Consume branch: a stale arm must not eat the
// buffered signal — the pop rolls back with the refused write, and the signal is still
// there, at its FIFO position, for whoever owns the row now. Park branch: not parked.
func TestFence_ArmExternal(t *testing.T) {
	for _, b := range testBackends(t) {
		t.Run(b.name, func(t *testing.T) {
			ctx := context.Background()
			insertExternalRunning(t, b.db, "arm-1")
			for i, sig := range []string{"s1", "s2"} {
				if delivered, err := b.db.DeliverSignal(ctx, "arm-1", "approval", sig, model.ExternalOutcome{Result: map[string]any{"n": i + 1}}); err != nil || delivered {
					t.Fatalf("buffer %s: delivered=%v err=%v", sig, delivered, err)
				}
			}

			stale := takeOver(t, b.db, "arm-1")
			if _, err := b.db.ArmExternalUnlessSignalled(ctx, stale, "approval", map[string]any{}, nil); !errors.Is(err, dbpkg.ErrLeaseLost) {
				t.Fatalf("stale not-parking arm: err=%v, want ErrLeaseLost", err)
			}
			if c, _ := b.db.CountBufferedSignals("arm-1", "approval"); c != 2 {
				t.Fatalf("a refused arm disturbed the buffer: %d buffered, want 2", c)
			}

			// Park branch, while arm-1 is still leased by its new owner: no buffered
			// signal, stale epoch — the instance must not park.
			insertExternalRunning(t, b.db, "arm-2")
			stale2 := takeOver(t, b.db, "arm-2")
			if _, err := b.db.ArmExternalUnlessSignalled(ctx, stale2, "approval", map[string]any{}, nil); !errors.Is(err, dbpkg.ErrLeaseLost) {
				t.Fatalf("stale park arm: err=%v, want ErrLeaseLost", err)
			}
			if got, _ := b.db.GetInstance("arm-2"); got.WaitState == model.WaitStateExternal || got.State[model.StateExternal] != nil || got.WakeAt != nil {
				t.Fatalf("a refused arm still parked: wait=%q external=%v wake=%v", got.WaitState, got.State[model.StateExternal], got.WakeAt)
			}

			// The current grant is accepted, and its buffer is untouched: the refused arms above
			// rolled back whole, so the answers are still there in FIFO order for phase 2.
			fresh, err := b.db.GetInstance("arm-1")
			if err != nil {
				t.Fatalf("GetInstance: %v", err)
			}
			armed, err := b.db.ArmExternalUnlessSignalled(ctx, fresh, "approval", map[string]any{}, nil)
			if err != nil || armed {
				t.Fatalf("current-grant arm: armed=%v err=%v, want not parked -- answers are waiting", armed, err)
			}
			if c, _ := b.db.CountBufferedSignals("arm-1", "approval"); c != 2 {
				t.Fatalf("the arm consumed a signal: %d buffered, want 2", c)
			}
			id, outcome, ok, err := b.db.PeekSignal("arm-1", "approval")
			if err != nil || !ok || n(outcome) != 1 {
				t.Fatalf("FIFO head: ok=%v n=%v err=%v, want n=1", ok, n(outcome), err)
			}
			_ = id
			// Every persist ends the work session, including the one that declines to park.
			after, _ := b.db.GetInstance("arm-1")
			if after.WorkerID != nil {
				t.Fatalf("the arm kept the lease held by %q", *after.WorkerID)
			}
		})
	}
}

// §3.1, §3.2, §3.6 — the renewer renews exactly the listed rows this worker still
// owns. Without the list scoping, a skipped self-reclaim is renewed forever and the
// row never hands back; without the worker_id guard, a repair would resurrect a lease
// on a row that has since been freed or taken over.
func TestRenewal_ScopedToHeldSet(t *testing.T) {
	for _, b := range testBackends(t) {
		t.Run(b.name, func(t *testing.T) {
			insertRunning(t, b.db, "scope-a")
			insertRunning(t, b.db, "scope-b")
			claimed, err := b.db.ClaimInstances("worker-A", 30*time.Second, 2, dbpkg.AllowTakeover())
			if err != nil || len(claimed) != 2 {
				t.Fatalf("claim: err=%v count=%d", err, len(claimed))
			}
			before, _ := b.db.GetInstance("scope-b")

			if _, err := b.db.RenewWorkerLeases("worker-A", []string{"scope-a"}, time.Hour); err != nil {
				t.Fatalf("RenewWorkerLeases: %v", err)
			}
			a, _ := b.db.GetInstance("scope-a")
			bRow, _ := b.db.GetInstance("scope-b")
			if !a.LeaseExpiresAt.After(dbpkg.Now().Add(30 * time.Minute)) {
				t.Fatalf("listed row was not renewed: expires %v", a.LeaseExpiresAt)
			}
			if !bRow.LeaseExpiresAt.Equal(*before.LeaseExpiresAt) {
				t.Fatalf("unlisted row was renewed (%v -> %v); the held-set scoping is what hands rows back", before.LeaseExpiresAt, bRow.LeaseExpiresAt)
			}

			// A listed id whose row was freed (worker_id NULL) is not re-stamped.
			freed, _ := b.db.GetInstance("scope-b")
			freed.Status = model.StatusCompleted
			freed.Task = ""
			if err := b.db.UpdateInstance(freed); err != nil {
				t.Fatalf("release scope-b: %v", err)
			}
			if _, err := b.db.RenewWorkerLeases("worker-A", []string{"scope-b"}, time.Hour); err != nil {
				t.Fatalf("RenewWorkerLeases: %v", err)
			}
			if got, _ := b.db.GetInstance("scope-b"); got.WorkerID != nil || got.LeaseExpiresAt != nil {
				t.Fatalf("renewing a freed row resurrected its lease: worker=%v expires=%v", got.WorkerID, got.LeaseExpiresAt)
			}

			// A listed id whose row another worker has since claimed is a no-op.
			dbpkg.AdvanceClock(time.Hour + time.Minute) // expire scope-a
			taken := claimOne(t, b.db, "worker-B", time.Minute)
			if taken.ID != "scope-a" {
				t.Fatalf("takeover claim took %q, want scope-a", taken.ID)
			}
			if _, err := b.db.RenewWorkerLeases("worker-A", []string{"scope-a"}, 2*time.Hour); err != nil {
				t.Fatalf("RenewWorkerLeases: %v", err)
			}
			if got, _ := b.db.GetInstance("scope-a"); *got.WorkerID != "worker-B" || !got.LeaseExpiresAt.Equal(*taken.LeaseExpiresAt) {
				t.Fatalf("a repair re-stamped a row worker-B now owns: worker=%v expires=%v", got.WorkerID, got.LeaseExpiresAt)
			}
		})
	}
}

// The only_once evidence chain at the DB layer: a row dropped from the renewal list expires,
// KEEPS its worker_id, and the next claim reports ReclaimedExpired. Fails if anyone
// reintroduces a release that clears worker_id — the cleanup that re-runs only_once tasks.
func TestRenewal_UnlistedRowHandsBackWithEvidence(t *testing.T) {
	for _, b := range testBackends(t) {
		t.Run(b.name, func(t *testing.T) {
			insertRunning(t, b.db, "evidence-1")
			held := claimOne(t, b.db, "worker-A", 30*time.Millisecond)

			// The worker stops listing the row (a doomed advance finished, the held
			// set dropped it) — renewals no longer cover it.
			if _, err := b.db.RenewWorkerLeases("worker-A", nil, time.Minute); err != nil {
				t.Fatalf("RenewWorkerLeases: %v", err)
			}
			dbpkg.AdvanceClock(time.Second)

			if got, _ := b.db.GetInstance("evidence-1"); got.WorkerID == nil || *got.WorkerID != "worker-A" {
				t.Fatalf("the expired row lost its worker_id (%v); that column is the interruption evidence", got.WorkerID)
			}

			next := claimOne(t, b.db, "worker-A", time.Minute) // self-reclaim: same worker is the common case
			if next.ID != "evidence-1" {
				t.Fatalf("claimed %q, want evidence-1", next.ID)
			}
			if !next.ReclaimedExpired {
				t.Fatal("the takeover was not observed (ReclaimedExpired=false); an interrupted only_once task would silently re-run")
			}
			if next.LeaseEpoch != held.LeaseEpoch+1 {
				t.Fatalf("hand-back claim granted epoch %d, want %d", next.LeaseEpoch, held.LeaseEpoch+1)
			}
		})
	}
}

// The epoch alone is not the grant. A rewind (a DB losing committed transactions to an
// unclean shutdown, or a failover to a lagging replica) un-issues a claim while the
// worker that won it is still running, and the next claim re-issues the SAME number to
// someone else. Both then carry a matching lease_epoch, so worker_id is what separates
// them. specs/durability-levels.md §7.
func TestFence_ReusedEpochBelongsToOneWorker(t *testing.T) {
	for _, b := range testBackends(t) {
		t.Run(b.name, func(t *testing.T) {
			insertInst(t, b.db, "fence-reuse", model.StatusRunning, "", nil, "")
			stale := takeOver(t, b.db, "fence-reuse") // stale holds the victim's grant
			current := mustEpoch(t, b.db, "fence-reuse")
			if stale.LeaseEpoch == current {
				t.Fatalf("setup: victim already holds the current epoch %d", current)
			}

			// What a rewind produces: the victim's own grant, renumbered to the epoch the
			// thief now holds. Every other fence test relies on the numbers differing; this
			// is the one where they do not.
			stale.LeaseEpoch = current
			stale.Status = model.StatusCompleted
			stale.Task = ""
			stale.ErrorMessage = "stale outcome"
			if err := b.db.UpdateInstance(stale); !errors.Is(err, dbpkg.ErrLeaseLost) {
				t.Fatalf("write at a reused epoch: err=%v, want ErrLeaseLost — an epoch is a grant to one worker, not a number", err)
			}

			got, err := b.db.GetInstance("fence-reuse")
			if err != nil {
				t.Fatalf("GetInstance: %v", err)
			}
			if got.Status != model.StatusRunning || got.ErrorMessage != "" {
				t.Fatalf("stale write clobbered the thief: status=%q error=%q", got.Status, got.ErrorMessage)
			}
			if got.WorkerID == nil || *got.WorkerID != "thief" {
				t.Fatalf("refused write disturbed the lease: worker=%v", got.WorkerID)
			}

			// The holder of that same epoch still writes normally — the predicate narrows
			// the grant to one worker, it does not invalidate the epoch.
			got.Status = model.StatusCompleted
			if err := b.db.UpdateInstance(got); err != nil {
				t.Fatalf("thief's own write at the same epoch: %v", err)
			}
		})
	}
}

// The same reuse against the checkpoint path, which is the one a long-running advance
// actually takes: a stale progress write must not resurrect an instance the new owner
// has moved on from.
func TestFence_ReusedEpochProgress(t *testing.T) {
	for _, b := range testBackends(t) {
		t.Run(b.name, func(t *testing.T) {
			insertInst(t, b.db, "fence-reuse-prog", model.StatusRunning, "", nil, "")
			stale := takeOver(t, b.db, "fence-reuse-prog")

			stale.LeaseEpoch = mustEpoch(t, b.db, "fence-reuse-prog")
			stale.RetryCount = 9
			stale.Task = "stale-step"
			if err := b.db.UpdateInstanceProgress(stale); !errors.Is(err, dbpkg.ErrLeaseLost) {
				t.Fatalf("progress write at a reused epoch: err=%v, want ErrLeaseLost", err)
			}
			got, _ := b.db.GetInstance("fence-reuse-prog")
			if got.RetryCount == 9 || got.Task == "stale-step" {
				t.Fatalf("stale progress leaked: retry=%d task=%q", got.RetryCount, got.Task)
			}
		})
	}
}

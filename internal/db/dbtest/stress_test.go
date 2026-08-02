package dbtest

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/lib/pq"

	dbpkg "genroc/internal/db"
	"genroc/internal/model"
)

// pgDeadlock reports whether err is a PostgreSQL deadlock error (SQLSTATE 40P01).
func pgDeadlock(err error) bool {
	var pqErr *pq.Error
	return errors.As(err, &pqErr) && pqErr.Code == "40P01"
}

// TestStress_PauseProcess_vs_FailInstanceAndAncestors runs PauseProcess and
// FailInstanceAndAncestors concurrently many times to confirm that the known
// structural deadlock occurs in practice and that PostgreSQL always resolves it
// without leaving the DB in an inconsistent state.
//
// Each iteration fires both operations against the same parent-child pair:
//   - PauseProcess locks parent (running) then descends to child (running) — top-down.
//   - FailInstanceAndAncestors locks child first (UpdateInstance has no status filter),
//     then tries to lock parent — bottom-up.
//
// This is the only real deadlock in the codebase; the other lock-ordering pairs cannot
// occur in practice because their WHERE conditions are mutually exclusive.
func TestStress_PauseProcess_vs_FailInstanceAndAncestors(t *testing.T) {
	if sharedPgDB == nil {
		t.Skip("PostgreSQL not available (set POSTGRES_DSN)")
	}
	ctx := context.Background()
	db := sharedPgDB

	const iterations = 30
	var deadlockCount, successCount, nothingToPause int

	for i := 0; i < iterations; i++ {
		sharedPgRaw.ExecContext(ctx, "DELETE FROM process_instances")
		insertInst(t, db, "parent", model.StatusRunning, "", nil, "")
		insertInst(t, db, "child", model.StatusRunning, "parent", []string{"parent"}, "")

		child, err := db.GetInstance("child")
		if err != nil {
			t.Fatalf("iteration %d: GetInstance child: %v", i, err)
		}
		child.Status = model.StatusFailed
		child.Error = "stress error"

		var wg sync.WaitGroup
		errs := make(chan error, 2)

		wg.Add(2)
		go func() { defer wg.Done(); errs <- db.PauseProcess(ctx, "parent") }()
		go func() { defer wg.Done(); errs <- db.FailInstanceAndAncestors(child) }()
		wg.Wait()
		close(errs)

		for err := range errs {
			switch {
			case err == nil:
				successCount++
			case pgDeadlock(err):
				deadlockCount++
			// The failure won the race and left nothing running, so the pause had
			// no rows to touch — a legitimate serial outcome, not an inconsistency.
			case strings.Contains(err.Error(), "no running instances to pause"):
				nothingToPause++
			default:
				t.Errorf("iteration %d: unexpected error: %v", i, err)
			}
		}

		for _, id := range []string{"parent", "child"} {
			inst, err := db.GetInstance(id)
			if err != nil {
				t.Errorf("iteration %d: %s not queryable after concurrent ops: %v", i, id, err)
				continue
			}
			if inst.Status == model.StatusRunning {
				t.Errorf("iteration %d: %s still 'running' — inconsistent state", i, id)
			}
		}
	}

	total := iterations * 2
	t.Logf("ran %d iterations (%d total operations)", iterations, total)
	t.Logf("  success:   %d/%d (%.0f%%)", successCount, total, 100*float64(successCount)/float64(total))
	t.Logf("  deadlock:  %d/%d (%.0f%%)", deadlockCount, total, 100*float64(deadlockCount)/float64(total))
	t.Logf("  no-op pause: %d/%d", nothingToPause, total)
	if deadlockCount == 0 {
		t.Log("  note: no deadlocks observed — scheduling may not have produced the exact interleave")
	}
}

// TestStress_ClaimInstances_MultiWorker fires many concurrent workers continuously
// polling ClaimInstances against a small, fixed instance pool. The lease is kept
// very short (100 ms) so instances expire and become reclaimable many times per
// second, keeping workers in constant contention.
//
// Invariant: FOR UPDATE SKIP LOCKED must guarantee that no instance is claimed by
// two workers simultaneously. Workers track their in-flight claims in a shared
// map; any new claim that finds the instance already owned by a different worker
// whose Go-side lease has not expired is flagged as a double-claim.
func TestStress_ClaimInstances_MultiWorker(t *testing.T) {
	if sharedPgDB == nil {
		t.Skip("PostgreSQL not available (set POSTGRES_DSN)")
	}
	ctx := context.Background()
	db := sharedPgDB

	const (
		runFor        = 3 * time.Second
		leaseDur      = 1 * time.Second // must be >= 1s: DB stores lease_expires_at as int64 unix seconds
		instanceCount = 5               // fewer instances than workers — guaranteed contention
		workerCount   = 10
	)

	sharedPgRaw.ExecContext(ctx, "DELETE FROM process_instances")
	for j := 0; j < instanceCount; j++ {
		insertInst(t, db, fmt.Sprintf("inst-%d", j), model.StatusRunning, "", nil, "")
	}

	type lease struct {
		workerID  string
		expiresAt time.Time
	}

	var mu sync.Mutex
	active := map[string]lease{} // instID -> current holder
	var totalClaims int

	deadline := time.Now().Add(runFor)
	var wg sync.WaitGroup
	for w := 0; w < workerCount; w++ {
		wg.Add(1)
		workerID := fmt.Sprintf("worker-%d", w)
		go func(workerID string) {
			defer wg.Done()
			for time.Now().Before(deadline) {
				claimedAt := time.Now()
				instances, err := db.ClaimInstances(workerID, leaseDur, instanceCount, dbpkg.AllowTakeover)
				if err != nil {
					t.Errorf("worker %s: ClaimInstances: %v", workerID, err)
					return
				}
				// Mirror the DB's integer-millisecond truncation so the Go-side expiry
				// matches exactly when the DB considers the lease expired.
				expiry := time.UnixMilli(claimedAt.UnixMilli() + leaseDur.Milliseconds())
				mu.Lock()
				for _, inst := range instances {
					if prev, exists := active[inst.ID]; exists &&
						prev.workerID != workerID &&
						time.Now().Before(prev.expiresAt) {
						t.Errorf("double-claim: instance %s held by %s (exp %v) also claimed by %s",
							inst.ID, prev.workerID, prev.expiresAt.Format(time.RFC3339Nano), workerID)
					}
					active[inst.ID] = lease{workerID, expiry}
					totalClaims++
				}
				mu.Unlock()
			}
		}(workerID)
	}
	wg.Wait()

	if totalClaims == 0 {
		t.Error("no instances were claimed — ClaimInstances may be broken")
	}
	t.Logf("workers: %d, instances: %d, lease: %v, duration: %v", workerCount, instanceCount, leaseDur, runFor)
	t.Logf("total claim events: %d (~%.0f/s)", totalClaims, float64(totalClaims)/runFor.Seconds())
}

// TestStress_ConcurrentFinishChild fires N goroutines each completing one of N siblings
// of the same waiting parent. Invariant: the parent transitions to 'collecting' exactly
// once — the sibling that finishes last is the only one that should find active_count==0
// and trigger the transition.
//
// This tests that the FOR UPDATE lock on the parent in FinishChild correctly serialises
// concurrent completions and prevents the "zero-count check" from running simultaneously
// in two goroutines.
func TestStress_ConcurrentFinishChild(t *testing.T) {
	if sharedPgDB == nil {
		t.Skip("PostgreSQL not available (set POSTGRES_DSN)")
	}
	ctx := context.Background()
	db := sharedPgDB

	const (
		iterations = 20
		siblings   = 5
	)

	for i := 0; i < iterations; i++ {
		sharedPgRaw.ExecContext(ctx, "DELETE FROM process_instances")
		insertInstW(t, db, "parent", model.StatusRunning, model.WaitStateWaiting, "", nil, "")
		for j := 0; j < siblings; j++ {
			insertInst(t, db, fmt.Sprintf("child-%d", j), model.StatusRunning, "parent", []string{"parent"}, "")
		}

		var wg sync.WaitGroup
		for j := 0; j < siblings; j++ {
			wg.Add(1)
			childID := fmt.Sprintf("child-%d", j)
			go func(childID string) {
				defer wg.Done()
				child, err := db.GetInstance(childID)
				if err != nil {
					t.Errorf("iteration %d: GetInstance %s: %v", i, childID, err)
					return
				}
				child.Status = model.StatusCompleted
				if err := db.FinishChild(child); err != nil {
					t.Errorf("iteration %d: FinishChild %s: %v", i, childID, err)
				}
			}(childID)
		}
		wg.Wait()

		// All children must be completed.
		for j := 0; j < siblings; j++ {
			id := fmt.Sprintf("child-%d", j)
			inst, err := db.GetInstance(id)
			if err != nil {
				t.Errorf("iteration %d: %s not found: %v", i, id, err)
				continue
			}
			if inst.Status != model.StatusCompleted {
				t.Errorf("iteration %d: %s status = %q, want completed", i, id, inst.Status)
			}
		}

		// Parent must have transitioned to exactly 'collecting'.
		parent, err := db.GetInstance("parent")
		if err != nil {
			t.Errorf("iteration %d: parent not found: %v", i, err)
			continue
		}
		if parent.WaitState != model.WaitStateCollecting {
			t.Errorf("iteration %d: parent wait_state = %q, want collecting", i, parent.WaitState)
		}
	}
}

// TestStress_PauseProcess_vs_FinishChild fires PauseProcess on a waiting parent
// while N goroutines concurrently call FinishChild on its children.
//
// PauseProcess locks rows top-down via a recursive CTE (parent first, then children).
// FinishChild locks parent first, then updates the child within the same transaction.
// When the CTE locks a child before the parent row within a single execution plan,
// the lock order can invert relative to a concurrent FinishChild — producing a deadlock.
//
// Invariant: all errors must be nil or a PostgreSQL deadlock; no instance may be
// left in 'running' after both sides complete. (The parent is running throughout, so
// the pause always has at least one row to touch and never reports a no-op.)
func TestStress_PauseProcess_vs_FinishChild(t *testing.T) {
	if sharedPgDB == nil {
		t.Skip("PostgreSQL not available (set POSTGRES_DSN)")
	}
	ctx := context.Background()
	db := sharedPgDB

	const (
		iterations = 20
		siblings   = 4
	)

	for i := 0; i < iterations; i++ {
		sharedPgRaw.ExecContext(ctx, "DELETE FROM process_instances")
		insertInstW(t, db, "parent", model.StatusRunning, model.WaitStateWaiting, "", nil, "")
		for j := 0; j < siblings; j++ {
			insertInst(t, db, fmt.Sprintf("child-%d", j), model.StatusRunning, "parent", []string{"parent"}, "")
		}

		children := make([]*model.ProcessInstance, siblings)
		for j := 0; j < siblings; j++ {
			inst, err := db.GetInstance(fmt.Sprintf("child-%d", j))
			if err != nil {
				t.Fatalf("iteration %d: GetInstance child-%d: %v", i, j, err)
			}
			inst.Status = model.StatusCompleted
			children[j] = inst
		}

		var wg sync.WaitGroup
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := db.PauseProcess(ctx, "parent"); err != nil && !pgDeadlock(err) {
				t.Errorf("iteration %d: PauseProcess: %v", i, err)
			}
		}()
		for _, child := range children {
			wg.Add(1)
			child := child
			go func() {
				defer wg.Done()
				if err := db.FinishChild(child); err != nil && !pgDeadlock(err) {
					t.Errorf("iteration %d: FinishChild %s: %v", i, child.ID, err)
				}
			}()
		}
		wg.Wait()

		// No instance may be stuck in 'running' after all operations settle.
		ids := make([]string, 0, 1+siblings)
		ids = append(ids, "parent")
		for j := 0; j < siblings; j++ {
			ids = append(ids, fmt.Sprintf("child-%d", j))
		}
		for _, id := range ids {
			inst, err := db.GetInstance(id)
			if err != nil {
				t.Errorf("iteration %d: %s not found: %v", i, id, err)
				continue
			}
			if inst.Status == model.StatusRunning {
				t.Errorf("iteration %d: %s still 'running' after pause+finish — inconsistent state", i, id)
			}
		}
	}
}

// TestStress_RetryProcess_vs_PauseProcess fires RetryProcess and PauseProcess
// concurrently against the same settled failed tree. Both lock tree rows in id
// order, so they serialize; each iteration must end in one of the two serial
// outcomes:
//
//	retry → pause:  the revived (running) rows were caught by the pause → paused
//	pause → retry:  nothing was running, so the pause was a rejected no-op → running
//
// The fixture is a failed tree because retry now accepts nothing else, which makes
// the second ordering the common one: a failed tree has no running row, so the pause
// reports "no running instances to pause" and changes nothing.
//
// Either way the tree stays internally consistent and the completed child is
// never touched.
func TestStress_RetryProcess_vs_PauseProcess(t *testing.T) {
	if sharedPgDB == nil {
		t.Skip("PostgreSQL not available (set POSTGRES_DSN)")
	}
	ctx := context.Background()
	db := sharedPgDB

	const iterations = 100
	for i := 0; i < iterations; i++ {
		sharedPgRaw.ExecContext(ctx, "DELETE FROM process_instances")
		insertInst(t, db, "root", model.StatusFailed, "", nil, "child failed")
		insertChild(t, db, "c-bad", model.StatusFailed, "root", "step1", []string{"root"}, "boom")
		insertChild(t, db, "c-ok", model.StatusCompleted, "root", "step1", []string{"root"}, "")

		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			if err := db.RetryProcess(ctx, "root", false); err != nil && !pgDeadlock(err) {
				t.Errorf("iteration %d: RetryProcess: %v", i, err)
			}
		}()
		go func() {
			defer wg.Done()
			err := db.PauseProcess(ctx, "root")
			// "nothing to pause" is the expected outcome of the pause → retry
			// ordering: the tree was still failed when the pause ran.
			if err != nil && !pgDeadlock(err) && !strings.Contains(err.Error(), "no running instances to pause") {
				t.Errorf("iteration %d: PauseProcess: %v", i, err)
			}
		}()
		wg.Wait()

		root := mustStatus(t, db, "root")
		bad := mustStatus(t, db, "c-bad")
		ok := mustStatus(t, db, "c-ok")

		// The pause never leases anything, so revived rows it catches go straight
		// to 'paused' (never 'pausing').
		valid := (root == model.StatusRunning && bad == model.StatusRunning) || // pause was a no-op
			(root == model.StatusPaused && bad == model.StatusPaused) || // pause caught the revived rows
			(root == model.StatusFailed && bad == model.StatusFailed) // retry lost to a deadlock
		if !valid {
			t.Errorf("iteration %d: inconsistent tree: root=%s c-bad=%s", i, root, bad)
		}
		if ok != model.StatusCompleted {
			t.Errorf("iteration %d: completed child touched: %s", i, ok)
		}
		// A revived root keeps its reconstructed wait_state through a pause —
		// pausing writes the status column and nothing else.
		if root == model.StatusRunning || root == model.StatusPaused {
			if ws := mustWaitState(t, db, "root"); ws != model.WaitStateWaiting {
				t.Errorf("iteration %d: revived root wait_state = %q, want waiting", i, ws)
			}
		}
	}
}

// TestStress_ConcurrentRetry fires several RetryProcess calls at the same
// settled failed tree. The tree lock serializes them: the first revives the
// tree; later calls either see the pre-tx status check fail ("not retryable")
// or enter the transaction, find a running root, and commit as a no-op.
// The end state must always be a single clean revival.
func TestStress_ConcurrentRetry(t *testing.T) {
	if sharedPgDB == nil {
		t.Skip("PostgreSQL not available (set POSTGRES_DSN)")
	}
	ctx := context.Background()
	db := sharedPgDB

	const (
		iterations = 100
		callers    = 8
	)
	for i := 0; i < iterations; i++ {
		sharedPgRaw.ExecContext(ctx, "DELETE FROM process_instances")
		insertInst(t, db, "root", model.StatusFailed, "", nil, "child failed")
		insertChild(t, db, "c-bad", model.StatusFailed, "root", "step1", []string{"root"}, "boom")
		insertChild(t, db, "c-ok", model.StatusCompleted, "root", "step1", []string{"root"}, "")

		var wg sync.WaitGroup
		var successes atomic.Int64
		for c := 0; c < callers; c++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				err := db.RetryProcess(ctx, "root", false)
				switch {
				case err == nil:
					successes.Add(1)
				case pgDeadlock(err):
				case strings.Contains(err.Error(), "not retryable"):
				default:
					t.Errorf("iteration %d: unexpected retry error: %v", i, err)
				}
			}()
		}
		wg.Wait()

		if successes.Load() == 0 {
			t.Errorf("iteration %d: no retry succeeded", i)
		}
		if got := mustStatus(t, db, "root"); got != model.StatusRunning {
			t.Errorf("iteration %d: root = %s, want running", i, got)
		}
		if got := mustWaitState(t, db, "root"); got != model.WaitStateWaiting {
			t.Errorf("iteration %d: root wait_state = %q, want waiting", i, got)
		}
		if got := mustStatus(t, db, "c-bad"); got != model.StatusRunning {
			t.Errorf("iteration %d: c-bad = %s, want running", i, got)
		}
		if got := mustStatus(t, db, "c-ok"); got != model.StatusCompleted {
			t.Errorf("iteration %d: c-ok = %s, want completed", i, got)
		}
	}
}

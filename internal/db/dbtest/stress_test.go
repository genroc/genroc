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

const deadlockHint = "deadlock: a write took its rows outside the shared id order (lockTree)"

// PauseProcess against FailInstanceAndAncestors: opposite ends of the tree, kept apart by the
// shared id lock order (lockTree).
func TestStress_PauseProcess_vs_FailInstanceAndAncestors(t *testing.T) {
	if sharedPgDB == nil {
		t.Skip("PostgreSQL not available (set POSTGRES_DSN)")
	}
	ctx := context.Background()
	db := sharedPgDB

	const iterations = 30
	var successCount, refused int

	for i := 0; i < iterations; i++ {
		sharedPgRaw.ExecContext(ctx, "DELETE FROM process_instances")
		insertInst(t, db, "parent", model.StatusRunning, "", nil, "")
		insertInst(t, db, "child", model.StatusRunning, "parent", []string{"parent"}, "")

		child, err := db.GetInstance("child")
		if err != nil {
			t.Fatalf("iteration %d: GetInstance child: %v", i, err)
		}
		child.Status = model.StatusFailed
		child.ErrorMessage = "stress error"

		var wg sync.WaitGroup
		errs := make(chan error, 2)

		wg.Add(2)
		go func() { defer wg.Done(); _, err := db.PauseProcess(ctx, "parent", ""); errs <- err }()
		go func() { defer wg.Done(); errs <- db.FailInstanceAndAncestors(child) }()
		wg.Wait()
		close(errs)

		for err := range errs {
			switch {
			case err == nil:
				successCount++
			case pgDeadlock(err):
				t.Errorf("iteration %d: %s: %v", i, deadlockHint, err)
			// The failure won the race: its root is 'failing', a transition, which takes no new
			// verb until it settles. A legitimate serial outcome, not an inconsistency.
			case errors.Is(err, dbpkg.ErrConflict) && strings.Contains(err.Error(), "failing"):
				refused++
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
	t.Logf("  pause refused mid-failure: %d/%d", refused, total)
}

// Many workers polling a small pool with a 100ms lease, so rows expire and are reclaimed
// constantly. Invariant: FOR UPDATE SKIP LOCKED never hands one instance to two workers —
// tracked through a shared in-flight map keyed by worker.
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
				instances, err := db.ClaimInstances(workerID, leaseDur, instanceCount, dbpkg.AllowTakeover())
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

// N goroutines each finishing one of N siblings of the same waiting parent. Invariant: the
// parent transitions to 'collecting' exactly once — the FOR UPDATE on the parent is what
// stops two goroutines both seeing active_count==0.
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
		insertInstW(t, db, "parent", model.StatusRunning, model.PhaseChildren, "", nil, "")
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
		if parent.Phase != model.PhaseCollecting {
			t.Errorf("iteration %d: parent phase = %q, want collecting", i, parent.Phase)
		}
	}
}

// PauseProcess against concurrent FinishChild, both locking in id order (lockTree). Invariant:
// every error is nil, and no instance is left 'running'.
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
		insertInstW(t, db, "parent", model.StatusRunning, model.PhaseChildren, "", nil, "")
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
			if _, err := db.PauseProcess(ctx, "parent", ""); err != nil {
				t.Errorf("iteration %d: PauseProcess: %v", i, err)
			}
		}()
		for _, child := range children {
			wg.Add(1)
			child := child
			go func() {
				defer wg.Done()
				if err := db.FinishChild(child); err != nil {
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

// RetryProcess and PauseProcess against one settled failed tree. Both lock in id order, so
// they serialize into one of two outcomes: retry→pause leaves paused, pause→retry is a
// rejected no-op leaving running. Either way the completed child is never touched.
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
			if _, err := db.RetryProcess(ctx, "root", false, ""); err != nil {
				t.Errorf("iteration %d: RetryProcess: %v", i, err)
			}
		}()
		go func() {
			defer wg.Done()
			_, err := db.PauseProcess(ctx, "root", "")
			// "nothing to pause" is the expected outcome of the pause → retry
			// ordering: the tree was still failed when the pause ran.
			if err != nil && !strings.Contains(err.Error(), "no running instances to pause") {
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
			(root == model.StatusPaused && bad == model.StatusPaused) // pause caught the revived rows
		if !valid {
			t.Errorf("iteration %d: inconsistent tree: root=%s c-bad=%s", i, root, bad)
		}
		if ok != model.StatusCompleted {
			t.Errorf("iteration %d: completed child touched: %s", i, ok)
		}
		// A revived root keeps its reconstructed phase through a pause —
		// pausing writes the status column and nothing else.
		if root == model.StatusRunning || root == model.StatusPaused {
			if ws := mustPhase(t, db, "root"); ws != model.PhaseChildren {
				t.Errorf("iteration %d: revived root phase = %q, want waiting", i, ws)
			}
		}
	}
}

// Concurrent retries of one settled failed tree: the tree lock serializes them, so later
// calls either fail the pre-tx status check or commit as a no-op. The end state must be a
// single clean revival.
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
				_, err := db.RetryProcess(ctx, "root", false, "")
				switch {
				case err == nil:
					successes.Add(1)
				case pgDeadlock(err):
					t.Errorf("iteration %d: %s: %v", i, deadlockHint, err)
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
		if got := mustPhase(t, db, "root"); got != model.PhaseChildren {
			t.Errorf("iteration %d: root phase = %q, want waiting", i, got)
		}
		if got := mustStatus(t, db, "c-bad"); got != model.StatusRunning {
			t.Errorf("iteration %d: c-bad = %s, want running", i, got)
		}
		if got := mustStatus(t, db, "c-ok"); got != model.StatusCompleted {
			t.Errorf("iteration %d: c-ok = %s, want completed", i, got)
		}
	}
}

// CancelProcess against FailInstanceAndAncestors — the same top-down/bottom-up pair as the
// pause test above, because CancelProcess takes the same rows in the same id order. Cancel
// must not introduce a lock order of its own.
func TestStress_CancelProcess_vs_FailInstanceAndAncestors(t *testing.T) {
	if sharedPgDB == nil {
		t.Skip("PostgreSQL not available (set POSTGRES_DSN)")
	}
	ctx := context.Background()
	db := sharedPgDB

	const iterations = 30
	var successCount, refused int

	for i := 0; i < iterations; i++ {
		sharedPgRaw.ExecContext(ctx, "DELETE FROM process_instances")
		insertInst(t, db, "parent", model.StatusRunning, "", nil, "")
		insertInst(t, db, "child", model.StatusRunning, "parent", []string{"parent"}, "")

		child, err := db.GetInstance("child")
		if err != nil {
			t.Fatalf("iteration %d: GetInstance child: %v", i, err)
		}
		child.Status = model.StatusFailed
		child.ErrorMessage = "stress error"

		var wg sync.WaitGroup
		errs := make(chan error, 2)
		wg.Add(2)
		go func() { defer wg.Done(); _, err := db.CancelProcess(ctx, "parent", ""); errs <- err }()
		go func() { defer wg.Done(); errs <- db.FailInstanceAndAncestors(child) }()
		wg.Wait()
		close(errs)

		for err := range errs {
			switch {
			case err == nil:
				successCount++
			case pgDeadlock(err):
				t.Errorf("iteration %d: %s: %v", i, deadlockHint, err)
			// The failure won: a 'failing' root takes no new verb until it settles.
			case errors.Is(err, dbpkg.ErrConflict) && strings.Contains(err.Error(), "failing"):
				refused++
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
	t.Logf("ran %d iterations: %d ok, %d cancels refused mid-failure", iterations, successCount, refused)
}

// Either serial order ends cancelled; an interleave leaving it merely paused would downgrade an
// operator's final stop to a reversible one.
func TestStress_CancelProcess_vs_PauseProcess(t *testing.T) {
	if sharedPgDB == nil {
		t.Skip("PostgreSQL not available (set POSTGRES_DSN)")
	}
	ctx := context.Background()
	db := sharedPgDB

	const iterations = 40
	for i := 0; i < iterations; i++ {
		sharedPgRaw.ExecContext(ctx, "DELETE FROM process_instances")
		insertInst(t, db, "parent", model.StatusRunning, "", nil, "")
		insertInst(t, db, "child", model.StatusRunning, "parent", []string{"parent"}, "")

		var wg sync.WaitGroup
		errs := make(chan error, 2)
		wg.Add(2)
		go func() { defer wg.Done(); _, err := db.CancelProcess(ctx, "parent", ""); errs <- err }()
		go func() { defer wg.Done(); _, err := db.PauseProcess(ctx, "parent", ""); errs <- err }()
		wg.Wait()
		close(errs)

		for err := range errs {
			if err != nil {
				t.Errorf("iteration %d: unexpected error: %v", i, err)
			}
		}

		for _, id := range []string{"parent", "child"} {
			inst, err := db.GetInstance(id)
			if err != nil {
				t.Errorf("iteration %d: %s not queryable: %v", i, id, err)
				continue
			}
			if inst.Status != model.StatusCancelled {
				t.Errorf("iteration %d: %s is %q; a cancel must win either interleave",
					i, id, inst.Status)
			}
		}
	}
	t.Logf("ran %d iterations; cancel won every interleave", iterations)
}

// Postgres-gated: SQLite's single writer passes however it was written. The reader takes the tree
// in ONE statement, or it could not tell an atomicity bug from its own sampling.
func TestStress_CancelProcess_IsAtomic(t *testing.T) {
	if sharedPgDB == nil {
		t.Skip("PostgreSQL not available (set POSTGRES_DSN)")
	}
	ctx := context.Background()
	db := sharedPgDB
	const kids = 8

	for i := 0; i < 20; i++ {
		sharedPgRaw.ExecContext(ctx, "DELETE FROM process_instances")
		insertInst(t, db, "root", model.StatusRunning, "", nil, "")
		for k := 0; k < kids; k++ {
			insertInst(t, db, fmt.Sprintf("kid%d", k), model.StatusRunning, "root", []string{"root"}, "")
		}

		var wg sync.WaitGroup
		var mixed atomic.Value
		stop := make(chan struct{})

		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				rows, err := sharedPgRaw.QueryContext(ctx,
					`SELECT status, COUNT(*) FROM process_instances WHERE root_id = $1 GROUP BY status`, "root")
				if err != nil {
					return
				}
				seen := map[string]int{}
				for rows.Next() {
					var status string
					var n int
					if err := rows.Scan(&status, &n); err == nil {
						seen[status] = n
					}
				}
				rows.Close()
				// One status for the whole tree, whichever it is. Two means a reader caught
				// the cancel part-way through, and a tree whose parent and children disagree
				// about whether the run is over is exactly what the transaction prevents.
				if len(seen) > 1 {
					mixed.Store(fmt.Sprintf("%v", seen))
					return
				}
			}
		}()

		if _, err := db.CancelProcess(ctx, "root", ""); err != nil {
			t.Fatalf("iteration %d: CancelProcess: %v", i, err)
		}
		close(stop)
		wg.Wait()

		if v := mixed.Load(); v != nil {
			t.Fatalf("iteration %d: a reader saw a half-cancelled tree: %s", i, v)
		}
		var n int
		if err := sharedPgRaw.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM process_instances WHERE root_id = $1 AND status = 'cancelled'`,
			"root").Scan(&n); err != nil {
			t.Fatalf("iteration %d: count: %v", i, err)
		}
		if n != kids+1 {
			t.Errorf("iteration %d: %d of %d rows cancelled", i, n, kids+1)
		}
	}
}

// RetryProcess against the instance-retention sweep on one failed tree whose ids sort on both
// sides of the root. Both lock the tree in id order (lockTree), so either may win, never both,
// and neither deadlocks.
func TestStress_RetryProcess_vs_PruneConcludedTrees(t *testing.T) {
	if sharedPgDB == nil {
		t.Skip("PostgreSQL not available (set POSTGRES_DSN)")
	}
	ctx := context.Background()
	db := sharedPgDB

	const iterations = 50
	var retried, pruned int
	for i := 0; i < iterations; i++ {
		sharedPgRaw.ExecContext(ctx, "DELETE FROM process_instances")
		insertInst(t, db, "m-root", model.StatusRunning, "", nil, "")
		insertInst(t, db, "a-child", model.StatusFailed, "m-root", []string{"m-root"}, "boom")
		insertInst(t, db, "z-child", model.StatusFailed, "m-root", []string{"m-root"}, "boom")
		settle(t, db, "m-root", model.StatusFailed)

		var wg sync.WaitGroup
		var retryErr, pruneErr error
		var n int
		wg.Add(2)
		go func() { defer wg.Done(); _, retryErr = db.RetryProcess(ctx, "m-root", false, "stress") }()
		go func() { defer wg.Done(); n, pruneErr = db.PruneConcludedTrees(ctx, nowPlusHours(1), 10) }()
		wg.Wait()

		for _, err := range []error{retryErr, pruneErr} {
			if pgDeadlock(err) {
				t.Errorf("iteration %d: %s: %v", i, deadlockHint, err)
			}
		}
		if pruneErr != nil && !pgDeadlock(pruneErr) {
			t.Errorf("iteration %d: prune: %v", i, pruneErr)
		}
		switch {
		case retryErr == nil && n == 0:
			retried++
			if gone(t, db, "m-root") {
				t.Errorf("iteration %d: the retry succeeded and the tree is gone", i)
			}
		case n == 1:
			pruned++
			if retryErr == nil {
				t.Errorf("iteration %d: both the retry and the prune report success on one tree", i)
			}
			if !gone(t, db, "m-root") || !gone(t, db, "a-child") || !gone(t, db, "z-child") {
				t.Errorf("iteration %d: a pruned tree left rows behind", i)
			}
		case !pgDeadlock(retryErr):
			t.Errorf("iteration %d: neither won: retry=%v pruned=%d", i, retryErr, n)
		}
	}
	t.Logf("%d iterations: retry won %d, prune won %d", iterations, retried, pruned)
}

// How long a retention backlog takes to drain in the engine's 200-tree batches, and the longest
// single batch: that is how long the sweep holds its locks (on SQLite, the one write lock).
func TestStress_RetentionBacklogDrain(t *testing.T) {
	if sharedPgDB == nil {
		t.Skip("PostgreSQL not available (set POSTGRES_DSN)")
	}
	const trees, batch = 2000, 200
	for _, b := range testBackends(t) {
		t.Run(b.name, func(t *testing.T) {
			seedStart := time.Now()
			for i := range trees {
				root := fmt.Sprintf("bk-%05d", i)
				insertInst(t, b.db, root, model.StatusCompleted, "", nil, "")
				insertInst(t, b.db, root+"-a", model.StatusCompleted, root, []string{root}, "")
				insertInst(t, b.db, root+"-b", model.StatusCompleted, root, []string{root}, "")
				for _, id := range []string{root, root + "-a", root + "-b"} {
					appendLog(t, b.db, id, model.LogInfo, "inst_completed", dbpkg.Now().UnixMilli())
				}
			}
			t.Logf("seeded %d trees (%d instances, %d logs) in %s", trees, 3*trees, 3*trees, time.Since(seedStart).Round(time.Millisecond))

			cutoff := nowPlusHours(1)
			var total int
			var longest time.Duration
			start := time.Now()
			for {
				t0 := time.Now()
				n, err := b.db.PruneConcludedTrees(context.Background(), cutoff, batch)
				if err != nil {
					t.Fatalf("PruneConcludedTrees: %v", err)
				}
				longest = max(longest, time.Since(t0))
				total += n
				if n < batch {
					break
				}
			}
			elapsed := time.Since(start)
			t.Logf("drained %d trees in %s (%.0f trees/s); longest %d-tree batch %s",
				total, elapsed.Round(time.Millisecond), float64(total)/elapsed.Seconds(), batch, longest.Round(time.Millisecond))
			if total != trees {
				t.Errorf("drained %d trees, want %d", total, trees)
			}
			// Generous: a batch this slow stalls every writer for seconds, on SQLite the whole engine.
			if longest > 2*time.Second {
				t.Errorf("a %d-tree batch took %s; the sweep would stall writers", batch, longest)
			}
		})
	}
}

// Leaves of two draining parents settling at once. Each sees a draining parent and widens to its
// stack's locks, so the last of each batch, whichever it is, must climb: a settle-up that two
// siblings both skip leaves the tree 'pausing' forever, with nothing left to land.
func TestStress_ConcurrentSettleUp(t *testing.T) {
	if sharedPgDB == nil {
		t.Skip("PostgreSQL not available (set POSTGRES_DSN)")
	}
	ctx := context.Background()
	db := sharedPgDB

	const (
		iterations = 20
		leaves     = 3
	)
	for i := 0; i < iterations; i++ {
		sharedPgRaw.ExecContext(ctx, "DELETE FROM process_instances")
		insertInstW(t, db, "root", model.StatusPausing, model.PhaseChildren, "", nil, "")
		var ids []string
		for _, mid := range []string{"mid1", "mid2"} {
			insertChildW(t, db, mid, model.StatusPausing, model.PhaseChildren, "root", []string{"root"})
			for j := 0; j < leaves; j++ {
				id := fmt.Sprintf("%s-leaf%d", mid, j)
				insertChild(t, db, id, model.StatusPausing, mid, "step1", []string{"root", mid}, "")
				ids = append(ids, id)
			}
		}

		var wg sync.WaitGroup
		for _, id := range ids {
			wg.Add(1)
			go func(id string) {
				defer wg.Done()
				leaf, err := db.GetInstance(id)
				if err != nil {
					t.Errorf("iteration %d: GetInstance %s: %v", i, id, err)
					return
				}
				leaf.Status = model.StatusPaused // what settlePausing writes
				if err := db.FinishChild(leaf); err != nil {
					if pgDeadlock(err) {
						t.Errorf("iteration %d: %s: %v", i, deadlockHint, err)
					} else {
						t.Errorf("iteration %d: FinishChild %s: %v", i, id, err)
					}
				}
			}(id)
		}
		wg.Wait()

		for _, id := range []string{"root", "mid1", "mid2"} {
			if got := mustStatus(t, db, id); got != model.StatusPaused {
				t.Errorf("iteration %d: %s = %q after every leaf settled; a concurrent settle-up was lost", i, id, got)
			}
		}
	}
}

// Siblings failing and completing at once, against a pause. Every failure may be the first, so
// each reads the root under the stack's locks and widens to the tree's; the invariant is that the
// tree settles 'failed' with nothing left busy, whichever order the writes land in.
func TestStress_ConcurrentFailures_vs_Pause(t *testing.T) {
	if sharedPgDB == nil {
		t.Skip("PostgreSQL not available (set POSTGRES_DSN)")
	}
	ctx := context.Background()
	db := sharedPgDB

	const (
		iterations = 20
		children   = 6
	)
	for i := 0; i < iterations; i++ {
		sharedPgRaw.ExecContext(ctx, "DELETE FROM process_instances")
		insertInstW(t, db, "root", model.StatusRunning, model.PhaseChildren, "", nil, "")
		for j := 0; j < children; j++ {
			insertChild(t, db, fmt.Sprintf("c%d", j), model.StatusRunning, "root", "step1", []string{"root"}, "")
		}

		var wg sync.WaitGroup
		errs := make(chan error, children+1)
		wg.Add(1)
		go func() { defer wg.Done(); _, err := db.PauseProcess(ctx, "root", ""); errs <- err }()
		for j := 0; j < children; j++ {
			wg.Add(1)
			go func(j int) {
				defer wg.Done()
				c, err := db.GetInstance(fmt.Sprintf("c%d", j))
				if err != nil {
					errs <- err
					return
				}
				if j%2 == 0 {
					c.Status, c.ErrorCode, c.ErrorMessage = model.StatusFailed, "boom", "boom"
					errs <- db.FailInstanceAndAncestors(c)
					return
				}
				c.Status = model.StatusCompleted
				errs <- db.FinishChild(c)
			}(j)
		}
		wg.Wait()
		close(errs)
		for err := range errs {
			switch {
			case err == nil:
			case pgDeadlock(err):
				t.Errorf("iteration %d: %s: %v", i, deadlockHint, err)
			default:
				t.Errorf("iteration %d: unexpected error: %v", i, err)
			}
		}

		if got := mustStatus(t, db, "root"); got != model.StatusFailed {
			t.Errorf("iteration %d: root = %q, want failed: nothing was in flight, so nothing may hold it open", i, got)
		}
		for j := 0; j < children; j++ {
			id := fmt.Sprintf("c%d", j)
			if got := mustStatus(t, db, id); got.Busy() {
				t.Errorf("iteration %d: %s = %q under a failed root", i, id, got)
			}
		}
	}
}

// Pause racing resume. Whichever wins, the tree agrees with its root: a resume whose lock waited
// on a committing pause must flip all of it or none of it, never the root alone.
func TestStress_PauseVsResume(t *testing.T) {
	if sharedPgDB == nil {
		t.Skip("PostgreSQL not available (set POSTGRES_DSN)")
	}
	ctx := context.Background()
	db := sharedPgDB
	const iterations = 60
	for i := 0; i < iterations; i++ {
		sharedPgRaw.ExecContext(ctx, "DELETE FROM process_instances")
		insertInstW(t, db, "root", model.StatusRunning, model.PhaseChildren, "", nil, "")
		for j := 0; j < 4; j++ {
			insertChild(t, db, fmt.Sprintf("c%d", j), model.StatusRunning, "root", "step1", []string{"root"}, "")
		}
		if i%2 == 1 { // half the runs start paused, so resume has something to flip
			if _, err := db.PauseProcess(ctx, "root", ""); err != nil {
				t.Fatalf("iteration %d: setup pause: %v", i, err)
			}
		}

		var wg sync.WaitGroup
		errs := make(chan error, 2)
		wg.Add(2)
		go func() { defer wg.Done(); _, err := db.PauseProcess(ctx, "root", ""); errs <- err }()
		go func() { defer wg.Done(); _, err := db.ResumeProcess(ctx, "root", ""); errs <- err }()
		wg.Wait()
		close(errs)
		for err := range errs {
			if err != nil {
				t.Errorf("iteration %d: %v", i, err)
			}
		}

		root := mustStatus(t, db, "root")
		for j := 0; j < 4; j++ {
			if got := mustStatus(t, db, fmt.Sprintf("c%d", j)); got != root {
				t.Errorf("iteration %d: c%d is %q under a %q root: the verbs interleaved", i, j, got, root)
			}
		}
	}
}

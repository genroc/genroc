package dbtest

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

// specs/instance-retention.md. Go, not e2e: the prune's tree lock is a dual-engine concern.

func settle(t *testing.T, db *dbpkg.DB, id string, status model.Status) {
	t.Helper()
	inst, err := db.GetInstance(id)
	if err != nil {
		t.Fatalf("GetInstance %s: %v", id, err)
	}
	inst.Status = status
	if err := db.UpdateInstance(inst); err != nil {
		t.Fatalf("settle %s: %v", id, err)
	}
}

// tree seeds a root with one child, a buffered signal and a log row on each, then settles both.
func tree(t *testing.T, db *dbpkg.DB, root string, rootStatus, childStatus model.Status) {
	t.Helper()
	insertInst(t, db, root, model.StatusRunning, "", nil, "")
	spawnInstance(t, db, root+"-child", root)
	if _, err := db.DeliverSignal(context.Background(), root, "approval", model.ExternalOutcome{Result: "ok"}); err != nil {
		t.Fatalf("DeliverSignal: %v", err)
	}
	now := dbpkg.Now().UnixMilli()
	appendLog(t, db, root, model.LogInfo, "inst_created", now)
	appendLog(t, db, root+"-child", model.LogInfo, "inst_created", now)
	settle(t, db, root+"-child", childStatus)
	settle(t, db, root, rootStatus)
}

func gone(t *testing.T, db *dbpkg.DB, id string) bool {
	t.Helper()
	_, err := db.GetInstance(id)
	if err != nil && !errors.Is(err, dbpkg.ErrNotFound) {
		t.Fatalf("GetInstance %s: %v", id, err)
	}
	return err != nil
}

func logCount(t *testing.T, db *dbpkg.DB, root string) int {
	t.Helper()
	logs, _, err := db.LogsFor(root, false, dbpkg.LogQuery{Page: dbpkg.PageReq{Limit: 100}})
	if err != nil {
		t.Fatalf("LogsFor %s: %v", root, err)
	}
	return len(logs)
}

func advance(t *testing.T, d time.Duration) {
	dbpkg.AdvanceClock(d)
	t.Cleanup(func() { dbpkg.AdvanceClock(-d) })
}

func TestRetention_PrunesConcludedTreesWhole(t *testing.T) {
	for _, b := range testBackends(t) {
		t.Run(b.name, func(t *testing.T) {
			ctx := context.Background()
			tree(t, b.db, "old-done", model.StatusCompleted, model.StatusCompleted)
			tree(t, b.db, "old-failed", model.StatusFailed, model.StatusFailed)
			tree(t, b.db, "old-running", model.StatusRunning, model.StatusCompleted)
			// Not reachable through the engine, which concludes a root last; the prune still checks.
			tree(t, b.db, "old-live-child", model.StatusCompleted, model.StatusRunning)
			advance(t, 2*time.Hour)
			tree(t, b.db, "recent-done", model.StatusCompleted, model.StatusCompleted)

			n, err := b.db.PruneConcludedTrees(ctx, dbpkg.Now().Add(-time.Hour).UnixMilli(), 100)
			if err != nil {
				t.Fatalf("PruneConcludedTrees: %v", err)
			}
			if n != 2 {
				t.Errorf("pruned %d trees, want 2 (old-done, old-failed)", n)
			}
			for _, root := range []string{"old-done", "old-failed"} {
				if !gone(t, b.db, root) || !gone(t, b.db, root+"-child") {
					t.Errorf("%s: a concluded tree must go whole, root and children", root)
				}
				if c := logCount(t, b.db, root); c != 0 {
					t.Errorf("%s: %d log rows outlived their tree", root, c)
				}
				if _, _, ok, _ := b.db.PeekSignal(root, "approval"); ok {
					t.Errorf("%s: a buffered signal outlived its instance", root)
				}
			}
			for _, root := range []string{"old-running", "old-live-child", "recent-done"} {
				if gone(t, b.db, root) || gone(t, b.db, root+"-child") {
					t.Errorf("%s was pruned; only trees concluded before the cutoff, all rows settled, may go", root)
				}
				if c := logCount(t, b.db, root); c != 2 {
					t.Errorf("%s: %d log rows, want 2 kept", root, c)
				}
			}
		})
	}
}

func TestRetention_ABacklogDrainsInBatches(t *testing.T) {
	for _, b := range testBackends(t) {
		t.Run(b.name, func(t *testing.T) {
			for _, root := range []string{"b1", "b2", "b3", "b4", "b5"} {
				tree(t, b.db, root, model.StatusCompleted, model.StatusCompleted)
			}
			cutoff := dbpkg.Now().Add(time.Hour).UnixMilli()
			var got []int
			for range 4 {
				n, err := b.db.PruneConcludedTrees(context.Background(), cutoff, 2)
				if err != nil {
					t.Fatalf("PruneConcludedTrees: %v", err)
				}
				got = append(got, n)
			}
			if want := []int{2, 2, 1, 0}; !equalInts(got, want) {
				t.Errorf("batches pruned %v, want %v: a pass must stop at its limit and resume", got, want)
			}
		})
	}
}

func TestRetention_ReleasesWhatAPrunedTreeHeld(t *testing.T) {
	for _, b := range testBackends(t) {
		t.Run(b.name, func(t *testing.T) {
			b.db.SetObjectGrace(time.Hour)
			saveWithOutput(t, b.db, "big-done", bigString("held"))
			hash := outputRef(t, b.db, "big-done").Ref
			settle(t, b.db, "big-done", model.StatusCompleted)

			if n, err := b.db.PruneConcludedTrees(context.Background(), nowPlusHours(1), 100); err != nil || n != 1 {
				t.Fatalf("PruneConcludedTrees: n=%d err=%v", n, err)
			}
			if _, err := b.db.CollectObjects(nowPlusHours(0)); err != nil {
				t.Fatalf("CollectObjects (mark): %v", err)
			}
			if _, _, err := b.db.GetObjectContent(hash); err != nil {
				t.Fatalf("content went before --object-grace: %v", err)
			}
			if _, err := b.db.CollectObjects(nowPlusHours(2)); err != nil {
				t.Fatalf("CollectObjects (collect): %v", err)
			}
			if _, _, err := b.db.GetObjectContent(hash); !errors.Is(err, dbpkg.ErrNotFound) {
				t.Errorf("a pruned instance's value is still stored (err=%v); its claim was never released, so it leaks forever", err)
			}
		})
	}
}

func equalInts(a, b []int) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// Every concluded status, and only those: the SQL spells the list out twice (the index and the
// candidate query), apart from Status.Terminal().
func TestRetention_PrunesExactlyTheTerminalStatuses(t *testing.T) {
	all := []model.Status{
		model.StatusRunning, model.StatusCompleted, model.StatusFailing, model.StatusFailed,
		model.StatusRaised, model.StatusPausing, model.StatusPaused, model.StatusCancelling,
		model.StatusCancelled,
	}
	for _, b := range testBackends(t) {
		t.Run(b.name, func(t *testing.T) {
			for _, s := range all {
				insertInst(t, b.db, "st-"+string(s), model.StatusRunning, "", nil, "")
				settle(t, b.db, "st-"+string(s), s)
			}
			if _, err := b.db.PruneConcludedTrees(context.Background(), nowPlusHours(1), 100); err != nil {
				t.Fatalf("PruneConcludedTrees: %v", err)
			}
			for _, s := range all {
				if got := gone(t, b.db, "st-"+string(s)); got != s.Terminal() {
					t.Errorf("status %s: pruned=%v, want %v (Status.Terminal)", s, got, s.Terminal())
				}
			}
		})
	}
}

// AppendLog buffers; a row flushed after its instance is deleted would be an orphan nothing prunes
// when --log-retention is 0.
func TestRetention_ABufferedLogRowDoesNotOutliveItsTree(t *testing.T) {
	for _, b := range testBackends(t) {
		t.Run(b.name, func(t *testing.T) {
			insertInst(t, b.db, "buffered", model.StatusRunning, "", nil, "")
			settle(t, b.db, "buffered", model.StatusCompleted)
			appendLog(t, b.db, "buffered", model.LogInfo, "late", dbpkg.Now().UnixMilli())
			if n, err := b.db.PruneConcludedTrees(context.Background(), nowPlusHours(1), 100); err != nil || n != 1 {
				t.Fatalf("PruneConcludedTrees: n=%d err=%v", n, err)
			}
			if c := logCount(t, b.db, "buffered"); c != 0 {
				t.Errorf("%d buffered log rows were written after their instance was pruned", c)
			}
		})
	}
}

// Several servers sweep the same database: each tree goes exactly once, and none of them fails.
func TestRetention_ConcurrentSweepsDeleteEachTreeOnce(t *testing.T) {
	for _, b := range testBackends(t) {
		t.Run(b.name, func(t *testing.T) {
			const trees = 20
			for i := range trees {
				tree(t, b.db, fmt.Sprintf("con-%02d", i), model.StatusCompleted, model.StatusCompleted)
			}
			var wg sync.WaitGroup
			var total atomic.Int64
			errs := make(chan error, 4)
			for range 4 {
				wg.Add(1)
				go func() {
					defer wg.Done()
					n, err := b.db.PruneConcludedTrees(context.Background(), nowPlusHours(1), trees)
					if err != nil {
						errs <- err
					}
					total.Add(int64(n))
				}()
			}
			wg.Wait()
			close(errs)
			for err := range errs {
				t.Errorf("a concurrent sweep failed (%v); a fleet would log this every pass", err)
			}
			if got := total.Load(); got != trees {
				t.Errorf("sweeps reported %d trees pruned, want %d: each exactly once", got, trees)
			}
		})
	}
}

// Postgres only: SQLite's single writer cannot interleave. A retry holding the tree lock revives
// the root while the sweep, which read it as failed, waits on that lock.
func TestRetention_ASweepWaitingOnARetryKeepsTheRevivedTree(t *testing.T) {
	if sharedPgRaw == nil {
		t.Skip("POSTGRES_DSN not set; the interleaving needs a second writer")
	}
	for _, b := range testBackends(t) {
		if b.name != "postgres" {
			continue
		}
		ctx := context.Background()
		tree(t, b.db, "racing", model.StatusFailed, model.StatusFailed)

		retry, err := sharedPgRaw.BeginTx(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		defer retry.Rollback()
		if _, err := retry.ExecContext(ctx, `SELECT id FROM process_instances WHERE root_id = $1 ORDER BY id FOR UPDATE`, "racing"); err != nil {
			t.Fatalf("lock tree: %v", err)
		}
		if _, err := retry.ExecContext(ctx, `UPDATE process_instances SET status = 'running' WHERE root_id = $1`, "racing"); err != nil {
			t.Fatalf("revive: %v", err)
		}

		type result struct {
			n   int
			err error
		}
		done := make(chan result, 1)
		go func() {
			n, err := b.db.PruneConcludedTrees(ctx, nowPlusHours(1), 10)
			done <- result{n, err}
		}()
		select {
		case r := <-done:
			t.Fatalf("the sweep returned (n=%d err=%v) while the retry held the tree; it must wait on the lock", r.n, r.err)
		case <-time.After(300 * time.Millisecond):
		}
		if err := retry.Commit(); err != nil {
			t.Fatalf("commit retry: %v", err)
		}
		r := <-done
		if r.err != nil {
			t.Fatalf("PruneConcludedTrees: %v", r.err)
		}
		if r.n != 0 || gone(t, b.db, "racing") || gone(t, b.db, "racing-child") {
			t.Fatal("the sweep deleted a tree a retry revived while it waited; it must re-check under the lock")
		}
	}
}

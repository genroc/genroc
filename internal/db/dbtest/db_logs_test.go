package dbtest

import (
	"testing"
	"time"

	dbpkg "genroc/internal/db"
	"genroc/internal/model"
)

// appendLog writes one entry with an explicit timestamp so ordering and
// time-window assertions are deterministic.
func appendLog(t *testing.T, db *dbpkg.DB, instanceID string, level model.LogLevel, event string, atMillis int64) {
	t.Helper()
	err := db.AppendLog(&model.LogEntry{
		InstanceID: instanceID,
		Level:      level,
		Event:      event,
		TaskID:     "s1",
		Message:    event + " message",
		Data:       event,
		CreatedAt:  time.UnixMilli(atMillis),
	})
	if err != nil {
		t.Fatalf("AppendLog(%s): %v", event, err)
	}
}

// spawnInstance saves a bare instance row with the given parent, so the subtree
// recursive CTE in ListTreeLogs has a real parent_id chain to walk.
func spawnInstance(t *testing.T, db *dbpkg.DB, id, parentID string) {
	t.Helper()
	if err := db.SaveInstance(&model.ProcessInstance{
		ID:             id,
		ProcessName:    "test",
		ProcessVersion: 1,
		Task:           "",
		State:          map[string]any{},
		ParentID:       parentID,
		Status:         model.StatusRunning,
	}); err != nil {
		t.Fatalf("SaveInstance(%s): %v", id, err)
	}
}

func TestListLogs_OrderAndFilters(t *testing.T) {
	for _, b := range testBackends(t) {
		t.Run(b.name, func(t *testing.T) {
			appendLog(t, b.db, "inst-1", model.LogInfo, model.EventActionStarted, 1000)
			appendLog(t, b.db, "inst-1", model.LogWarn, model.EventRetryScheduled, 2000)
			appendLog(t, b.db, "inst-1", model.LogInfo, model.EventActionSucceeded, 3000)
			appendLog(t, b.db, "inst-1", model.LogInfo, model.EventInstanceDone, 4000)
			// A different instance must not leak into inst-1's logs.
			appendLog(t, b.db, "inst-2", model.LogInfo, model.EventActionStarted, 1500)

			all, _, err := b.db.ListLogs("inst-1", dbpkg.LogQuery{})
			if err != nil {
				t.Fatalf("ListLogs: %v", err)
			}
			if len(all) != 4 {
				t.Fatalf("expected 4 logs for inst-1, got %d", len(all))
			}
			// Newest first, like every list endpoint: a bare read is "the most recent",
			// and a caller wanting the trail in reading order asks for order=asc.
			wantOrder := []string{
				model.EventInstanceDone, model.EventActionSucceeded,
				model.EventRetryScheduled, model.EventActionStarted,
			}
			for i, w := range wantOrder {
				if all[i].Event != w {
					t.Errorf("entry %d: want %q, got %q", i, w, all[i].Event)
				}
			}
			// The raw data string round-trips unchanged.
			if all[0].Data != model.EventInstanceDone {
				t.Errorf("data not preserved: %q", all[0].Data)
			}
			// Ascending is the same four rows read the other way — the direction is the
			// caller's, not the storage's.
			asc, _, err := b.db.ListLogs("inst-1", dbpkg.LogQuery{Page: dbpkg.PageReq{Desc: new(bool)}})
			if err != nil {
				t.Fatalf("ListLogs(asc): %v", err)
			}
			if len(asc) != 4 || asc[0].Event != model.EventActionStarted || asc[3].Event != model.EventInstanceDone {
				t.Errorf("ascending order = %+v", asc)
			}

			// Level filter.
			warns, _, err := b.db.ListLogs("inst-1", dbpkg.LogQuery{Level: string(model.LogWarn)})
			if err != nil {
				t.Fatalf("ListLogs(level): %v", err)
			}
			if len(warns) != 1 || warns[0].Event != model.EventRetryScheduled {
				t.Fatalf("level filter: want 1 retry_scheduled, got %+v", warns)
			}

			// created_after bound (inclusive).
			recent, _, err := b.db.ListLogs("inst-1", dbpkg.LogQuery{Created: dbpkg.Window{After: 3000}})
			if err != nil {
				t.Fatalf("ListLogs(created_after): %v", err)
			}
			if len(recent) != 2 {
				t.Fatalf("since filter: want 2, got %d", len(recent))
			}
		})
	}
}

func TestListLogs_CursorPagination(t *testing.T) {
	for _, b := range testBackends(t) {
		t.Run(b.name, func(t *testing.T) {
			for i := int64(1); i <= 5; i++ {
				appendLog(t, b.db, "inst-1", model.LogInfo, model.EventTaskCompleted, i*1000)
			}

			page1, info1, err := b.db.ListLogs("inst-1", dbpkg.LogQuery{Page: dbpkg.PageReq{Limit: 2}})
			if err != nil {
				t.Fatalf("page1: %v", err)
			}
			if len(page1) != 2 {
				t.Fatalf("page1: want 2, got %d", len(page1))
			}
			// 5 rows total, page of 2: 0 before, 3 after.
			if info1.ItemsBefore != 0 || info1.ItemsAfter != 3 {
				t.Errorf("page1 position: before=%d after=%d, want 0/3", info1.ItemsBefore, info1.ItemsAfter)
			}
			if info1.After == "" {
				t.Fatal("page1: expected an after cursor")
			}
			last := page1[len(page1)-1]
			page2, info2, err := b.db.ListLogs("inst-1", dbpkg.LogQuery{
				Page: dbpkg.PageReq{Limit: 2, After: info1.After},
			})
			if err != nil {
				t.Fatalf("page2: %v", err)
			}
			if len(page2) != 2 {
				t.Fatalf("page2: want 2, got %d", len(page2))
			}
			// Pages must not overlap and must stay ordered. The default is newest-first,
			// so "forward" walks backward in time — the cursor advances in display order,
			// whichever direction that is.
			if !page2[0].CreatedAt.Before(last.CreatedAt) {
				t.Errorf("cursor did not advance: page1 last=%v page2 first=%v",
					last.CreatedAt, page2[0].CreatedAt)
			}
			if info2.ItemsBefore != 2 {
				t.Errorf("page2 items_before = %d, want 2", info2.ItemsBefore)
			}
			// Page backward from page2 returns page1's rows again.
			back, _, err := b.db.ListLogs("inst-1", dbpkg.LogQuery{
				Page: dbpkg.PageReq{Limit: 2, Before: info2.Before},
			})
			if err != nil {
				t.Fatalf("back: %v", err)
			}
			if len(back) != 2 || back[0].ID != page1[0].ID || back[1].ID != page1[1].ID {
				t.Errorf("backward page did not return page1: got %d rows", len(back))
			}
		})
	}
}

// TestListLogs_CursorTiebreaker pages through rows that all share one timestamp,
// forcing the id tiebreaker to carry the keyset. It must return every row exactly
// once, in order, on both engines — the property that distinguishes keyset
// pagination from a naive ORDER BY created_at LIMIT/OFFSET.
func TestListLogs_CursorTiebreaker(t *testing.T) {
	for _, b := range testBackends(t) {
		t.Run(b.name, func(t *testing.T) {
			const n = 5
			for i := 0; i < n; i++ {
				// Same created_at for all; UUIDv7 ids stay monotonic, so the (created_at,
				// id) keyset is a total order with insertion == id order.
				appendLog(t, b.db, "inst-1", model.LogInfo, model.EventTaskCompleted, 1000)
			}

			var collected []string
			seen := map[string]bool{}
			after := ""
			for pages := 0; ; pages++ {
				if pages > n+2 {
					t.Fatal("pagination did not terminate")
				}
				logs, info, err := b.db.ListLogs("inst-1", dbpkg.LogQuery{
					Page: dbpkg.PageReq{Limit: 2, After: after},
				})
				if err != nil {
					t.Fatalf("page %d: %v", pages, err)
				}
				for _, l := range logs {
					if seen[l.ID] {
						t.Fatalf("duplicate id %s across pages", l.ID)
					}
					seen[l.ID] = true
					collected = append(collected, l.ID)
				}
				// next_cursor is always set now (it points past the last row even on the
				// final page), so terminate on items_after instead.
				if info.ItemsAfter == 0 {
					break
				}
				after = info.After
			}
			if len(collected) != n {
				t.Fatalf("collected %d ids, want %d (no skips/dupes)", len(collected), n)
			}
			// Newest-first is the default, and every row shares a created_at, so the id
			// tiebreaker alone carries the order — strictly descending, with no row
			// repeated or skipped at a page boundary.
			for i := 1; i < len(collected); i++ {
				if collected[i-1] <= collected[i] {
					t.Errorf("ids not strictly descending at %d: %s <= %s", i, collected[i-1], collected[i])
				}
			}
		})
	}
}

func TestListTreeLogs_AggregatesSubtree(t *testing.T) {
	for _, b := range testBackends(t) {
		t.Run(b.name, func(t *testing.T) {
			// Build a real parent chain so the recursive CTE has edges to walk:
			//   root → child-a → grandchild
			//   root → child-b
			// plus an unrelated tree (other) that must never leak in.
			spawnInstance(t, b.db, "root", "")
			spawnInstance(t, b.db, "child-a", "root")
			spawnInstance(t, b.db, "child-b", "root")
			spawnInstance(t, b.db, "grandchild", "child-a")
			spawnInstance(t, b.db, "other", "")

			appendLog(t, b.db, "root", model.LogInfo, model.EventChildrenSpawned, 1000)
			appendLog(t, b.db, "child-a", model.LogInfo, model.EventChildrenSpawned, 2000)
			appendLog(t, b.db, "child-b", model.LogInfo, model.EventActionSucceeded, 3000)
			appendLog(t, b.db, "grandchild", model.LogInfo, model.EventActionSucceeded, 4000)
			appendLog(t, b.db, "other", model.LogInfo, model.EventActionStarted, 2500)

			// Subtree from the root: root + a + b + grandchild = 4 (not "other").
			fromRoot, _, err := b.db.ListTreeLogs("root", dbpkg.LogQuery{})
			if err != nil {
				t.Fatalf("ListTreeLogs(root): %v", err)
			}
			if len(fromRoot) != 4 {
				t.Fatalf("subtree(root): want 4 entries, got %d", len(fromRoot))
			}
			// Depth is the instance's distance from the queried root.
			wantDepth := map[string]int{"root": 0, "child-a": 1, "child-b": 1, "grandchild": 2}
			for _, e := range fromRoot {
				if e.Depth != wantDepth[e.InstanceID] {
					t.Errorf("depth for %s: want %d, got %d", e.InstanceID, wantDepth[e.InstanceID], e.Depth)
				}
			}

			// Subtree from a mid-tree node: child-a + grandchild = 2. Works from any
			// node, not just the root — the win over the old root_id column.
			fromChildA, _, err := b.db.ListTreeLogs("child-a", dbpkg.LogQuery{})
			if err != nil {
				t.Fatalf("ListTreeLogs(child-a): %v", err)
			}
			if len(fromChildA) != 2 {
				t.Fatalf("subtree(child-a): want 2 entries, got %d", len(fromChildA))
			}
			// Depth is relative to the queried node: child-a is now the root (0).
			wantChildADepth := map[string]int{"child-a": 0, "grandchild": 1}
			for _, e := range fromChildA {
				if e.Depth != wantChildADepth[e.InstanceID] {
					t.Errorf("depth from child-a for %s: want %d, got %d", e.InstanceID, wantChildADepth[e.InstanceID], e.Depth)
				}
			}

			// Per-instance view stays scoped to one instance.
			single, _, err := b.db.ListLogs("child-a", dbpkg.LogQuery{})
			if err != nil {
				t.Fatalf("ListLogs(child-a): %v", err)
			}
			if len(single) != 1 {
				t.Fatalf("child-a: want 1 entry, got %d", len(single))
			}
		})
	}
}

func TestPruneLogs_DeletesOlderThanCutoff(t *testing.T) {
	for _, b := range testBackends(t) {
		t.Run(b.name, func(t *testing.T) {
			appendLog(t, b.db, "inst-1", model.LogInfo, model.EventActionStarted, 1000)
			appendLog(t, b.db, "inst-1", model.LogInfo, model.EventActionSucceeded, 2000)
			appendLog(t, b.db, "inst-1", model.LogInfo, model.EventInstanceDone, 3000)

			n, err := b.db.PruneLogs(2500)
			if err != nil {
				t.Fatalf("PruneLogs: %v", err)
			}
			if n != 2 {
				t.Fatalf("PruneLogs: want 2 deleted, got %d", n)
			}
			remaining, _, err := b.db.ListLogs("inst-1", dbpkg.LogQuery{})
			if err != nil {
				t.Fatalf("ListLogs: %v", err)
			}
			if len(remaining) != 1 || remaining[0].Event != model.EventInstanceDone {
				t.Fatalf("after prune: want only instance_completed, got %+v", remaining)
			}
		})
	}
}

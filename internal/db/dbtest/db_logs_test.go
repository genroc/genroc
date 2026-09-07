package dbtest

import (
	"fmt"
	"slices"
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

			// These rows have no instance row at all -- a trail outliving what wrote it.
			// LogsFor must still answer with them: an id it cannot place is not a root, so
			// its own rows are the most that can be said about it.
			orphan, _, err := b.db.LogsFor("inst-1", false, dbpkg.LogQuery{})
			if err != nil {
				t.Fatalf("LogsFor(inst-1): %v", err)
			}
			if len(orphan) != 4 {
				t.Fatalf("logs whose instance is gone: want 4, got %d", len(orphan))
			}

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

// TestListLogs_CursorTiebreaker pages through rows that all share one timestamp, so the keyset
// rests entirely on the tiebreaker. It must return every row exactly once, in order, on both
// engines — the property that distinguishes keyset pagination from ORDER BY + LIMIT/OFFSET.
func TestListLogs_CursorTiebreaker(t *testing.T) {
	for _, b := range testBackends(t) {
		t.Run(b.name, func(t *testing.T) {
			const n = 5
			written := make([]string, 0, n)
			for i := range n {
				// One created_at for all of them, so (created_at, seq, id) is carried by seq.
				event := fmt.Sprintf("row_%d", i)
				appendLog(t, b.db, "inst-1", model.LogInfo, event, 1000)
				written = append(written, event)
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
					collected = append(collected, l.Event)
				}
				// next_cursor is always set now (it points past the last row even on the
				// final page), so terminate on items_after instead.
				if info.ItemsAfter == 0 {
					break
				}
				after = info.After
			}
			if len(collected) != n {
				t.Fatalf("collected %d rows, want %d (no skips/dupes)", len(collected), n)
			}
			// Newest-first is the default, so the pages must come back as the reverse of the
			// order they were written -- with nothing repeated or skipped at a boundary.
			slices.Reverse(collected)
			if !slices.Equal(collected, written) {
				t.Errorf("paging reordered rows sharing a timestamp:\n got %v\nwant %v", collected, written)
			}
		})
	}
}

// Everything the id scheme rests on. created_at is millisecond-granular and a single advance
// writes its whole trail inside one, so `seq` -- the minting counter stored beside the id -- is
// what keeps the events in the order they happened. The ids deliberately do NOT sort (`2-9`
// follows `2-10` as text), so if the sort key ever loses seq this reverses in exactly the case
// an operator reads a trail for: what did this task do, in what order.
func TestLogsWrittenInOneMillisecondComeBackInOrder(t *testing.T) {
	for _, b := range testBackends(t) {
		t.Run(b.name, func(t *testing.T) {
			const sameMs = 1_700_000_000_000
			written := []string{
				model.EventActionStarted, model.EventRetryScheduled,
				model.EventActionSucceeded, model.EventInstanceDone,
			}
			for _, event := range written {
				appendLog(t, b.db, "one-ms", model.LogInfo, event, sameMs)
			}
			// Ten more rows in the same millisecond, so the answer cannot be luck: with no
			// tie-break the engines return these in an order nothing controls.
			for i := range 10 {
				appendLog(t, b.db, "one-ms", model.LogInfo, fmt.Sprintf("filler_%d", i), sameMs)
				written = append(written, fmt.Sprintf("filler_%d", i))
			}

			// Newest-first, like every list endpoint; the CLI flips it for display.
			got, _, err := b.db.ListLogs("one-ms", dbpkg.LogQuery{Page: dbpkg.PageReq{Limit: 100}})
			if err != nil {
				t.Fatalf("ListLogs: %v", err)
			}
			events := make([]string, len(got))
			for i, e := range got {
				events[i] = e.Event
			}
			slices.Reverse(events)
			if !slices.Equal(events, written) {
				t.Errorf("a trail written inside one millisecond came back reordered:\n got %v\nwant %v",
					events, written)
			}
		})
	}
}

func TestListTreeLogs_AggregatesSubtree(t *testing.T) {
	for _, b := range testBackends(t) {
		t.Run(b.name, func(t *testing.T) {
			// Build a real parent chain, since root_id is derived from parent_id at insert:
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

			// The tree from its root: root + a + b + grandchild = 4 (not "other").
			fromRoot, _, err := b.db.ListTreeLogs("root", dbpkg.LogQuery{})
			if err != nil {
				t.Fatalf("ListTreeLogs(root): %v", err)
			}
			if len(fromRoot) != 4 {
				t.Fatalf("subtree(root): want 4 entries, got %d", len(fromRoot))
			}
			// Every instance in the tree is represented, and nothing outside it.
			seen := map[string]bool{}
			for _, e := range fromRoot {
				seen[e.InstanceID] = true
			}
			for _, want := range []string{"root", "child-a", "child-b", "grandchild"} {
				if !seen[want] {
					t.Errorf("tree(root): %s is missing", want)
				}
			}

			// A tree is addressed by its ROOT. LogsFor is what the endpoint calls, and a
			// mid-tree id is a question about that instance: its own rows, not the subtree
			// under it and not its siblings'. There is no walk left that could answer more.
			fromChildA, _, err := b.db.LogsFor("child-a", false, dbpkg.LogQuery{})
			if err != nil {
				t.Fatalf("LogsFor(child-a): %v", err)
			}
			if len(fromChildA) != 1 || fromChildA[0].InstanceID != "child-a" {
				t.Fatalf("child-a: want its own 1 entry, got %d entries", len(fromChildA))
			}
			// flat asks a root for its own rows, which is the only way to get them alone.
			ownRows, _, err := b.db.LogsFor("root", true, dbpkg.LogQuery{})
			if err != nil {
				t.Fatalf("LogsFor(root, flat): %v", err)
			}
			if len(ownRows) != 1 || ownRows[0].InstanceID != "root" {
				t.Fatalf("root --flat: want its own 1 entry, got %d entries", len(ownRows))
			}

			// A root id takes the tree branch without being told which it is.
			whole, _, err := b.db.LogsFor("root", false, dbpkg.LogQuery{})
			if err != nil {
				t.Fatalf("LogsFor(root): %v", err)
			}
			if len(whole) != 4 {
				t.Fatalf("LogsFor(root): want the whole tree (4), got %d", len(whole))
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

// The level filter is a FLOOR: it selects that level and everything above it. On Postgres it is
// also the only filter binding several placeholders in one condition, so the ? → $N rewrite has
// to keep the placeholders and the args in step -- which is why this runs on both engines.
func TestListLogs_LevelIsAFloor(t *testing.T) {
	for _, b := range testBackends(t) {
		t.Run(b.name, func(t *testing.T) {
			appendLog(t, b.db, "inst-lvl", model.LogDebug, model.EventActionStarted, 1000)
			appendLog(t, b.db, "inst-lvl", model.LogInfo, model.EventWorkStarted, 2000)
			appendLog(t, b.db, "inst-lvl", model.LogWarn, model.EventRetryScheduled, 3000)
			appendLog(t, b.db, "inst-lvl", model.LogError, model.EventInstanceFailed, 4000)

			for _, tc := range []struct {
				level string
				want  int
			}{
				{"", 4},
				{string(model.LogDebug), 4},
				{string(model.LogInfo), 3},
				{string(model.LogWarn), 2},
				{string(model.LogError), 1},
				// Unknown: matches only itself. It must never widen to the whole trail --
				// the API refuses it, and this is the floor under that check.
				{"critical", 0},
			} {
				got, info, err := b.db.ListLogs("inst-lvl", dbpkg.LogQuery{Level: tc.level})
				if err != nil {
					t.Fatalf("ListLogs(level=%q): %v", tc.level, err)
				}
				if len(got) != tc.want {
					t.Errorf("level %q: want %d rows, got %d -- a floor keeps every level above it",
						tc.level, tc.want, len(got))
				}
				for _, l := range got {
					if tc.level != "" && !slices.Contains(model.LogLevelsAtLeast(model.LogLevel(tc.level)), l.Level) {
						t.Errorf("level %q: got a %s row, which is below the floor", tc.level, l.Level)
					}
				}
				// The counts run the same filters through a SECOND query -- which is where a
				// placeholder that lost its argument to the rewrite surfaces.
				if info.ItemsBefore != 0 || info.ItemsAfter != 0 {
					t.Errorf("level %q: one page of %d reported %d before and %d after",
						tc.level, len(got), info.ItemsBefore, info.ItemsAfter)
				}
			}
		})
	}
}

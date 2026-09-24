package dbtest

import (
	"testing"
	"time"

	dbpkg "genroc/internal/db"
	"genroc/internal/model"
)

// saveInstance persists a running instance with a non-trivial context (which the
// list projection must NOT carry) and returns it.
func saveInstance(t *testing.T, db *dbpkg.DB, process string) *model.ProcessInstance {
	t.Helper()
	inst := &model.ProcessInstance{
		ID:             db.NextID(),
		ProcessName:    process,
		ProcessVersion: 1,
		Task:           "",
		State: map[string]any{
			"input":   map[string]any{"secret": "do-not-leak-in-list"},
			"outputs": map[string]any{},
		},
		Status: model.StatusRunning,
	}
	if err := db.SaveInstance(inst); err != nil {
		t.Fatalf("SaveInstance: %v", err)
	}
	return inst
}

func summaryIDs(items []*model.InstanceSummary) []string {
	out := make([]string, len(items))
	for i, s := range items {
		out[i] = s.ID
	}
	return out
}

// TestListInstances_SortAndSummary covers the listing's two index-backed sorts and
// confirms the summary projection carries the scalar fields. The default sort is
// created (newest first, a stable/immutable key); updated is opt-in.
func TestListInstances_SortAndSummary(t *testing.T) {
	for _, b := range testBackends(t) {
		t.Run(b.name, func(t *testing.T) {
			a := saveInstance(t, b.db, "alpha")
			dbpkg.AdvanceClock(time.Second)
			bb := saveInstance(t, b.db, "beta")
			dbpkg.AdvanceClock(time.Second)
			c := saveInstance(t, b.db, "gamma")

			// Touch 'a' last so it becomes the most recently *updated* (but still the
			// oldest *created*).
			dbpkg.AdvanceClock(time.Second)
			a.Status = model.StatusCompleted
			if err := b.db.UpdateInstance(a); err != nil {
				t.Fatalf("UpdateInstance: %v", err)
			}

			// Default: created desc -> newest created first: c, b, a.
			got, info, err := b.db.ListInstances(dbpkg.InstanceQuery{})
			if err != nil {
				t.Fatalf("ListInstances: %v", err)
			}
			if info.Sort != "created" || info.Order != "desc" {
				t.Errorf("default sort/order = %q/%q, want created/desc", info.Sort, info.Order)
			}
			if want := []string{c.ID, bb.ID, a.ID}; !equalStrs(summaryIDs(got), want) {
				t.Errorf("created-desc order = %v, want %v", summaryIDs(got), want)
			}

			// The head row carries its summary scalar fields.
			head := got[0]
			if head.ProcessName != "gamma" || head.Status != model.StatusRunning || head.ProcessVersion != 1 {
				t.Errorf("summary head = %+v", head)
			}
			if head.UpdatedAt.Before(head.CreatedAt) {
				t.Errorf("head updated_at %v before created_at %v", head.UpdatedAt, head.CreatedAt)
			}

			// updated desc -> most recently active first: a (just updated), c, b.
			byUpdated, info, err := b.db.ListInstances(dbpkg.InstanceQuery{Page: dbpkg.PageReq{Sort: "updated"}})
			if err != nil {
				t.Fatalf("ListInstances updated: %v", err)
			}
			if info.Sort != "updated" {
				t.Errorf("sort echo = %q, want updated", info.Sort)
			}
			if want := []string{a.ID, c.ID, bb.ID}; !equalStrs(summaryIDs(byUpdated), want) {
				t.Errorf("updated-desc order = %v, want %v", summaryIDs(byUpdated), want)
			}

			// Status filter narrows the page.
			completed, _, err := b.db.ListInstances(dbpkg.InstanceQuery{Status: "completed"})
			if err != nil {
				t.Fatalf("ListInstances completed: %v", err)
			}
			if len(completed) != 1 || completed[0].ID != a.ID {
				t.Errorf("status filter = %v, want [%s]", summaryIDs(completed), a.ID)
			}

			// The two bounds are independent columns, and 'a' (created first, updated last) is the row
			// that tells them apart. Read the bound off a DB row, not saveInstance's argument — that
			// struct never carries the stamped timestamps, so a zero would skip the filter entirely.
			at := got[1].CreatedAt.UnixMilli() // b's created_at (got is c, b, a)
			byCreated, _, err := b.db.ListInstances(dbpkg.InstanceQuery{Created: dbpkg.Window{After: at}})
			if err != nil {
				t.Fatalf("ListInstances created_after: %v", err)
			}
			// created_at >= b's: c and b. 'a' was created before it.
			if want := []string{c.ID, bb.ID}; !equalStrs(summaryIDs(byCreated), want) {
				t.Errorf("created_after = %v, want %v", summaryIDs(byCreated), want)
			}
			byUpdatedAfter, _, err := b.db.ListInstances(dbpkg.InstanceQuery{Updated: dbpkg.Window{After: at}, Page: dbpkg.PageReq{Sort: "updated"}})
			if err != nil {
				t.Fatalf("ListInstances updated_after: %v", err)
			}
			// updated_at >= the same instant: all three, because 'a' was touched after.
			if want := []string{a.ID, c.ID, bb.ID}; !equalStrs(summaryIDs(byUpdatedAfter), want) {
				t.Errorf("updated_after = %v, want %v", summaryIDs(byUpdatedAfter), want)
			}
			// Both at once intersect rather than one winning — they are plain filters, and
			// only the one matching the sort is the point a forward walk starts from.
			both, _, err := b.db.ListInstances(dbpkg.InstanceQuery{Created: dbpkg.Window{After: at}, Updated: dbpkg.Window{After: at}, Page: dbpkg.PageReq{Sort: "updated"}})
			if err != nil {
				t.Fatalf("ListInstances both bounds: %v", err)
			}
			if want := []string{c.ID, bb.ID}; !equalStrs(summaryIDs(both), want) {
				t.Errorf("both bounds = %v, want %v (a fails created_at)", summaryIDs(both), want)
			}

			// A window is half-open: Before excludes a row sitting exactly on it, so
			// [a.created, b.created) is 'a' alone and adjacent windows never double-count
			// the boundary row.
			half, _, err := b.db.ListInstances(dbpkg.InstanceQuery{
				Created: dbpkg.Window{After: got[2].CreatedAt.UnixMilli(), Before: at}})
			if err != nil {
				t.Fatalf("ListInstances half-open: %v", err)
			}
			if want := []string{a.ID}; !equalStrs(summaryIDs(half), want) {
				t.Errorf("[a, b) = %v, want %v — Before must exclude its own instant", summaryIDs(half), want)
			}
		})
	}
}

// TestUpdatedAt_Advances documents the guarantee the updated sort relies on: every
// state-changing write bumps updated_at while created_at stays fixed.
func TestUpdatedAt_Advances(t *testing.T) {
	for _, b := range testBackends(t) {
		t.Run(b.name, func(t *testing.T) {
			inst := saveInstance(t, b.db, "p")
			saved, err := b.db.GetInstance(inst.ID)
			if err != nil {
				t.Fatalf("GetInstance: %v", err)
			}
			if !saved.UpdatedAt.Equal(saved.CreatedAt) {
				t.Errorf("on insert updated_at %v != created_at %v", saved.UpdatedAt, saved.CreatedAt)
			}

			dbpkg.AdvanceClock(time.Second)
			saved.Status = model.StatusCompleted
			if err := b.db.UpdateInstance(saved); err != nil {
				t.Fatalf("UpdateInstance: %v", err)
			}

			after, err := b.db.GetInstance(inst.ID)
			if err != nil {
				t.Fatalf("GetInstance after: %v", err)
			}
			if !after.UpdatedAt.After(saved.CreatedAt) {
				t.Errorf("updated_at %v did not advance past created_at %v", after.UpdatedAt, after.CreatedAt)
			}
			if !after.CreatedAt.Equal(saved.CreatedAt) {
				t.Errorf("created_at changed: %v -> %v", saved.CreatedAt, after.CreatedAt)
			}
		})
	}
}

func equalStrs(a, b []string) bool {
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

// TestListInstances_ProcessFilter covers the process-name filter and, with it, that the
// filters intersect rather than the last one winning. The empty-string case is the one
// that matters most: it must mean "every process", not "a process named empty".
func TestListInstances_ProcessFilter(t *testing.T) {
	for _, b := range testBackends(t) {
		t.Run(b.name, func(t *testing.T) {
			a1 := saveInstance(t, b.db, "alpha")
			dbpkg.AdvanceClock(time.Second)
			a2 := saveInstance(t, b.db, "alpha")
			dbpkg.AdvanceClock(time.Second)
			beta := saveInstance(t, b.db, "beta")

			all, _, err := b.db.ListInstances(dbpkg.InstanceQuery{})
			if err != nil {
				t.Fatalf("ListInstances unfiltered: %v", err)
			}
			if want := []string{beta.ID, a2.ID, a1.ID}; !equalStrs(summaryIDs(all), want) {
				t.Errorf("empty process = %v, want %v — an empty filter must not narrow the page", summaryIDs(all), want)
			}

			alphas, _, err := b.db.ListInstances(dbpkg.InstanceQuery{Process: "alpha"})
			if err != nil {
				t.Fatalf("ListInstances process=alpha: %v", err)
			}
			if want := []string{a2.ID, a1.ID}; !equalStrs(summaryIDs(alphas), want) {
				t.Errorf("process=alpha = %v, want %v — the filter spans every row of the name, and only that name", summaryIDs(alphas), want)
			}

			dbpkg.AdvanceClock(time.Second)
			a2.Status = model.StatusCompleted
			if err := b.db.UpdateInstance(a2); err != nil {
				t.Fatalf("UpdateInstance: %v", err)
			}
			// beta stays running and a1 is the other alpha, so a filter that dropped either
			// half of the pair returns a different row than a2.
			both, _, err := b.db.ListInstances(dbpkg.InstanceQuery{Status: "running", Process: "alpha"})
			if err != nil {
				t.Fatalf("ListInstances status+process: %v", err)
			}
			if want := []string{a1.ID}; !equalStrs(summaryIDs(both), want) {
				t.Errorf("status=running,process=alpha = %v, want %v — status and process must intersect", summaryIDs(both), want)
			}
		})
	}
}

// TestListInstances_VersionAndRootFilters covers the two filters an upgrade sweep iterates
// on: instances of one process still on the version being moved from, and only the roots --
// a child is not a unit of upgrade, so a sweep that returned them would collect refusals.
func TestListInstances_VersionAndRootFilters(t *testing.T) {
	for _, b := range testBackends(t) {
		t.Run(b.name, func(t *testing.T) {
			mk := func(id string, version int, parent string) {
				t.Helper()
				if err := b.db.SaveInstance(&model.ProcessInstance{
					ID: id, ProcessName: "sweep", ProcessVersion: version, Task: "t",
					State: map[string]any{}, Status: model.StatusRunning, ParentID: parent,
				}); err != nil {
					t.Fatalf("SaveInstance %s: %v", id, err)
				}
				dbpkg.AdvanceClock(time.Second)
			}
			mk("r1", 1, "")   // root on v1
			mk("c1", 1, "r1") // its child, also v1
			mk("r2", 2, "")   // root already on v2

			onV1, _, err := b.db.ListInstances(dbpkg.InstanceQuery{Process: "sweep", Version: 1})
			if err != nil {
				t.Fatalf("ListInstances by version: %v", err)
			}
			if want := []string{"c1", "r1"}; !equalStrs(summaryIDs(onV1), want) {
				t.Errorf("version filter = %v, want %v — it must select every instance on that version, children included", summaryIDs(onV1), want)
			}

			roots, _, err := b.db.ListInstances(dbpkg.InstanceQuery{Process: "sweep", Version: 1, RootsOnly: true})
			if err != nil {
				t.Fatalf("ListInstances roots: %v", err)
			}
			if want := []string{"r1"}; !equalStrs(summaryIDs(roots), want) {
				t.Errorf("root+version filter = %v, want %v — the child shares the version but is not a unit of upgrade", summaryIDs(roots), want)
			}

			all, _, err := b.db.ListInstances(dbpkg.InstanceQuery{Process: "sweep", RootsOnly: true})
			if err != nil {
				t.Fatalf("ListInstances all roots: %v", err)
			}
			if want := []string{"r2", "r1"}; !equalStrs(summaryIDs(all), want) {
				t.Errorf("roots across versions = %v, want %v — version 0 must mean no filter, not version zero", summaryIDs(all), want)
			}
		})
	}
}

// TestListInstances_WaitStateFilter covers the filter the external-task listing is built on.
// wait_state is orthogonal to status — every row here is running — so a filter that fell back
// to status would return all four.
func TestListInstances_WaitStateFilter(t *testing.T) {
	for _, b := range testBackends(t) {
		t.Run(b.name, func(t *testing.T) {
			park := func(process string, wait model.WaitState) *model.ProcessInstance {
				t.Helper()
				inst := saveInstance(t, b.db, process)
				inst.WaitState = wait
				if err := b.db.UpdateInstance(inst); err != nil {
					t.Fatalf("UpdateInstance: %v", err)
				}
				dbpkg.AdvanceClock(time.Second)
				return inst
			}
			running := park("alpha", model.WaitStateNone)
			ext1 := park("alpha", model.WaitStateExternal)
			kids := park("alpha", model.WaitStateWaiting)
			ext2 := park("beta", model.WaitStateExternal)

			external, _, err := b.db.ListInstances(dbpkg.InstanceQuery{WaitState: "external"})
			if err != nil {
				t.Fatalf("ListInstances wait_state=external: %v", err)
			}
			if want := []string{ext2.ID, ext1.ID}; !equalStrs(summaryIDs(external), want) {
				t.Errorf("wait_state=external = %v, want %v — the filter must select on wait_state alone, not on status",
					summaryIDs(external), want)
			}

			// The empty string is the unfiltered case, not "parked on nothing": it has to
			// return the unparked row too, or the default listing loses every idle instance.
			all, _, err := b.db.ListInstances(dbpkg.InstanceQuery{WaitState: ""})
			if err != nil {
				t.Fatalf("ListInstances wait_state empty: %v", err)
			}
			if want := []string{ext2.ID, kids.ID, ext1.ID, running.ID}; !equalStrs(summaryIDs(all), want) {
				t.Errorf("wait_state='' = %v, want %v — an empty filter must not narrow the page", summaryIDs(all), want)
			}

			both, _, err := b.db.ListInstances(dbpkg.InstanceQuery{WaitState: "external", Process: "alpha"})
			if err != nil {
				t.Fatalf("ListInstances wait_state+process: %v", err)
			}
			if want := []string{ext1.ID}; !equalStrs(summaryIDs(both), want) {
				t.Errorf("wait_state=external,process=alpha = %v, want %v — wait_state and process must intersect",
					summaryIDs(both), want)
			}
		})
	}
}

// TestListInstances_TaskFilter covers the task filter. A task id is a position, not an
// identity: the same spelling in two definitions is two different tasks, so the filter spans
// them until --process narrows it — which is the behaviour the pairing advice rests on.
func TestListInstances_TaskFilter(t *testing.T) {
	for _, b := range testBackends(t) {
		t.Run(b.name, func(t *testing.T) {
			at := func(process, task string) *model.ProcessInstance {
				t.Helper()
				inst := saveInstance(t, b.db, process)
				inst.Task = task
				if err := b.db.UpdateInstance(inst); err != nil {
					t.Fatalf("UpdateInstance: %v", err)
				}
				dbpkg.AdvanceClock(time.Second)
				return inst
			}
			alphaReview := at("alpha", "review")
			alphaPay := at("alpha", "pay")
			betaReview := at("beta", "review")

			review, _, err := b.db.ListInstances(dbpkg.InstanceQuery{Task: "review"})
			if err != nil {
				t.Fatalf("ListInstances task=review: %v", err)
			}
			if want := []string{betaReview.ID, alphaReview.ID}; !equalStrs(summaryIDs(review), want) {
				t.Errorf("task=review = %v, want %v — a task id spans every definition that spells it",
					summaryIDs(review), want)
			}

			scoped, _, err := b.db.ListInstances(dbpkg.InstanceQuery{Task: "review", Process: "alpha"})
			if err != nil {
				t.Fatalf("ListInstances task+process: %v", err)
			}
			if want := []string{alphaReview.ID}; !equalStrs(summaryIDs(scoped), want) {
				t.Errorf("task=review,process=alpha = %v, want %v — process is what makes a task id mean one task",
					summaryIDs(scoped), want)
			}

			// Exact, not a prefix — "pay" must not be reachable by a fragment of another id.
			if got, _, err := b.db.ListInstances(dbpkg.InstanceQuery{Task: "pa"}); err != nil {
				t.Fatalf("ListInstances task=pa: %v", err)
			} else if len(got) != 0 {
				t.Errorf("task=pa = %v, want none — the match is exact, and %s is the only near miss",
					summaryIDs(got), alphaPay.ID)
			}
		})
	}
}

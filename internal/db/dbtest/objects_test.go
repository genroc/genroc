package dbtest

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	dbpkg "genroc/internal/db"
	"genroc/internal/model"
	"genroc/internal/numeric"
)

// bigString returns a value larger than the externalization threshold (2 KiB) so it
// is stored in the object store rather than inline.
func bigString(tag string) string {
	return tag + ":" + strings.Repeat("x", 10*1024)
}

// TestObjects_BigValueRoundTrip verifies that a large value-slot is externalized,
// resolves back to the same value slot by slot, and that a small value stays
// inline.
func TestObjects_BigValueRoundTrip(t *testing.T) {
	for _, b := range testBackends(t) {
		t.Run(b.name, func(t *testing.T) {
			big := bigString("input")
			inst := &model.ProcessInstance{
				ID:          "inst-big",
				ProcessName: "test",
				Task:        "",
				State: map[string]any{
					"input":   big,
					"outputs": map[string]any{"small": "tiny", "huge": bigString("out")},
				},
				Status: model.StatusRunning,
			}
			if err := b.db.SaveInstance(inst); err != nil {
				t.Fatalf("SaveInstance: %v", err)
			}

			got, err := b.db.GetInstance("inst-big")
			if err != nil {
				t.Fatalf("GetInstance: %v", err)
			}
			// Big slots come back as lazy markers, small ones inline.
			if _, isRef := got.State["input"].(*model.ObjectRef); !isRef {
				t.Fatalf("expected input to be an *ObjectRef marker, got %T", got.State["input"])
			}
			outs := got.State["outputs"].(map[string]any)
			if outs["small"] != "tiny" {
				t.Errorf("small output: got %v, want tiny", outs["small"])
			}
			if _, isRef := outs["huge"].(*model.ObjectRef); !isRef {
				t.Fatalf("expected huge output to be a marker, got %T", outs["huge"])
			}

			resolveAll(t, b.db, got)
			if got.State["input"] != big {
				t.Errorf("hydrated input mismatch")
			}
			if got.State["outputs"].(map[string]any)["huge"] != bigString("out") {
				t.Errorf("hydrated huge output mismatch")
			}
		})
	}
}

// TestObjects_DerefKeepsItForTheGraceWindow is the REVERSE of what this test asserted before
// the object store was re-architected. It used to pin "a dereferenced object is deleted
// immediately", which existed so a replaced secret did not linger. That property was given up
// deliberately: reading now hands out references and fetching them is a second call, so
// deleting at dereference means a client 404s on a reference the server gave it moments earlier.
// Secret protection moved to recording and, ultimately, encryption at rest.
// specs/object-store.md §Collection.
func TestObjects_DerefKeepsItForTheGraceWindow(t *testing.T) {
	for _, b := range testBackends(t) {
		t.Run(b.name, func(t *testing.T) {
			b.db.SetObjectGrace(time.Hour)

			inst := &model.ProcessInstance{
				ID:          "inst-deref",
				ProcessName: "test",
				State:       map[string]any{"outputs": map[string]any{"loop": bigString("v1")}},
				Status:      model.StatusRunning,
			}
			if err := b.db.SaveInstance(inst); err != nil {
				t.Fatalf("SaveInstance: %v", err)
			}

			// Reload (so the output is a marker carrying its hash), capture the old ref, then
			// recompute it with a different big value and persist progress.
			r1, err := b.db.GetInstance("inst-deref")
			if err != nil {
				t.Fatalf("GetInstance: %v", err)
			}
			oldRef, ok := r1.State["outputs"].(map[string]any)["loop"].(*model.ObjectRef)
			if !ok {
				t.Fatalf("expected loop output to be a marker")
			}
			r1.State["outputs"].(map[string]any)["loop"] = bigString("v2")
			if err := b.db.UpdateInstanceProgress(r1); err != nil {
				t.Fatalf("UpdateInstanceProgress: %v", err)
			}

			// Released, but still fetchable: this is the contract a client holding a reference
			// relies on.
			if _, err := b.db.ResolveObject(context.Background(), oldRef); err != nil {
				t.Fatalf("a released object was not fetchable inside its grace window: %v", err)
			}
			// A sweep inside the window leaves it.
			if n, err := b.db.CollectObjects(nowPlusHours(0)); err != nil || n != 0 {
				t.Fatalf("swept inside the grace window: n=%d err=%v", n, err)
			}
			// Past the window it goes.
			if n, err := b.db.CollectObjects(nowPlusHours(2)); err != nil || n != 1 {
				t.Fatalf("expected the released object swept past its window: n=%d err=%v", n, err)
			}
			if _, err := b.db.ResolveObject(context.Background(), oldRef); err == nil {
				t.Fatal("a released object outlived its grace window")
			}

			// The new value is untouched by any of it.
			r2, err := b.db.GetInstance("inst-deref")
			if err != nil {
				t.Fatalf("GetInstance after overwrite: %v", err)
			}
			resolveAll(t, b.db, r2)
			if r2.State["outputs"].(map[string]any)["loop"] != bigString("v2") {
				t.Errorf("v2 output not preserved")
			}
		})
	}
}

// TestObjects_LogReferencedSurvivesDeref verifies that an object a log references is NOT deleted
// when the context slot sharing it is dereferenced — it stays fetchable while the log ROW that
// claims it exists, and is reclaimed once that row is pruned and the grace window passes.
func TestObjects_LogReferencedSurvivesDeref(t *testing.T) {
	for _, b := range testBackends(t) {
		t.Run(b.name, func(t *testing.T) {
			b.db.SetObjectGrace(time.Hour)

			// One value reached by two paths: the context slot's cut and the log payload's
			// cut produce the same leaf, so both claims land on one row.
			val := bigString("shared")
			content, _ := json.Marshal(val) // exactly what the context encoder stores

			// Context-reference it via an instance output...
			inst := &model.ProcessInstance{
				ID:          "inst-shared",
				ProcessName: "test",
				Task:        "",
				State:       map[string]any{"outputs": map[string]any{"out": val}},
				Status:      model.StatusRunning,
			}
			if err := b.db.SaveInstance(inst); err != nil {
				t.Fatalf("SaveInstance: %v", err)
			}
			// ...and log a payload whose cut produces the identical leaf. Same bytes, same
			// hash, one row -- which is the whole reason the log reuses the context's cut.
			entry := &model.LogEntry{InstanceID: "inst-shared", Level: model.LogInfo, Event: "probe"}
			if err := b.db.AppendLogValue(entry, map[string]any{"input": val}, 128); err != nil {
				t.Fatalf("AppendLogValue: %v", err)
			}
			if len(entry.Objects) != 1 {
				t.Fatalf("expected the log payload to externalize one leaf, got %d", len(entry.Objects))
			}
			ref := entry.Objects[0]

			// Dereference the context slot (replace the output with a small value).
			r, err := b.db.GetInstance("inst-shared")
			if err != nil {
				t.Fatalf("GetInstance: %v", err)
			}
			r.State["outputs"].(map[string]any)["out"] = "small"
			if err := b.db.UpdateInstanceProgress(r); err != nil {
				t.Fatalf("UpdateInstanceProgress: %v", err)
			}

			// The shared object survives — the log still needs it — and serves its payload.
			got, _, err := b.db.GetObjectContent(ref.Ref)
			if err != nil {
				t.Fatalf("log object missing after context dereference: %v", err)
			}
			if got != string(content) {
				t.Errorf("log object content mismatch")
			}

			// While the log row stands the sweep leaves it; once the row is pruned the claim's
			// owner is gone, and the object goes after the grace window.
			if n, err := b.db.CollectObjects(nowPlusHours(0)); err != nil || n != 0 {
				t.Fatalf("premature sweep: n=%d err=%v", n, err)
			}
			if _, err := b.db.PruneLogs(nowPlusHours(1)); err != nil {
				t.Fatalf("PruneLogs: %v", err)
			}
			// This sweep retires the orphaned claim and stamps grace, so it collects nothing --
			// a reference handed out before the prune still resolves.
			if n, err := b.db.CollectObjects(nowPlusHours(2)); err != nil || n != 0 {
				t.Fatalf("the grace window did not open: n=%d err=%v", n, err)
			}
			if n, err := b.db.CollectObjects(nowPlusHours(5)); err != nil || n != 1 {
				t.Fatalf("expected 1 object swept once the grace window closed, got n=%d err=%v", n, err)
			}
			if _, _, err := b.db.GetObjectContent(ref.Ref); err == nil {
				t.Fatalf("expected log object to be swept after horizon")
			}
		})
	}
}

// nowPlusHours returns a unix-ms cutoff h hours from the DB clock, for the GC sweep.
func nowPlusHours(h int) int64 {
	return dbpkg.Now().Add(time.Duration(h) * time.Hour).UnixMilli()
}

// saveWithOutput stores an instance whose single output slot holds v.
func saveWithOutput(t *testing.T, db *dbpkg.DB, id string, v any) {
	t.Helper()
	inst := &model.ProcessInstance{
		ID: id, ProcessName: "test", Status: model.StatusRunning,
		State: map[string]any{"outputs": map[string]any{"out": v}},
	}
	if err := db.SaveInstance(inst); err != nil {
		t.Fatalf("SaveInstance %s: %v", id, err)
	}
}

// replaceOutput swaps the output slot, releasing whatever it held.
func replaceOutput(t *testing.T, db *dbpkg.DB, id string, v any) {
	t.Helper()
	inst, err := db.GetInstance(id)
	if err != nil {
		t.Fatalf("GetInstance %s: %v", id, err)
	}
	inst.State["outputs"].(map[string]any)["out"] = v
	if err := db.UpdateInstanceProgress(inst); err != nil {
		t.Fatalf("UpdateInstanceProgress %s: %v", id, err)
	}
}

func outputRef(t *testing.T, db *dbpkg.DB, id string) *model.ObjectRef {
	t.Helper()
	inst, err := db.GetInstance(id)
	if err != nil {
		t.Fatalf("GetInstance %s: %v", id, err)
	}
	ref, ok := inst.State["outputs"].(map[string]any)["out"].(*model.ObjectRef)
	if !ok {
		t.Fatalf("%s: output was not externalized (%T)", id, inst.State["outputs"].(map[string]any)["out"])
	}
	return ref
}

// TestObjects_ContentIsSharedAcrossInstances is what the global store exists for: the same bytes
// in two instances are ONE object with two claims, where the old (instance_id, hash) key made
// them two rows. The measurement in specs/object-store.md, as an assertion.
func TestObjects_ContentIsSharedAcrossInstances(t *testing.T) {
	for _, b := range testBackends(t) {
		t.Run(b.name, func(t *testing.T) {
			big := bigString("shared")
			saveWithOutput(t, b.db, "inst-a", big)
			saveWithOutput(t, b.db, "inst-b", big)

			ref := outputRef(t, b.db, "inst-a")
			n, err := b.db.CountObjectRefs(ref.Ref)
			if err != nil {
				t.Fatalf("CountObjectRefs: %v", err)
			}
			if n != 2 {
				t.Fatalf("got %d claims on the shared object, want 2 — identical content in two instances must be one object held twice", n)
			}
		})
	}
}

// TestObjects_ReleaseKeepsContentAnotherInstanceHolds is the failure this design is most able to
// cause, and the reason deletion is "no claim remains" rather than "my claim is gone": under a
// shared store the wrong rule destroys an unrelated instance's value.
func TestObjects_ReleaseKeepsContentAnotherInstanceHolds(t *testing.T) {
	for _, b := range testBackends(t) {
		t.Run(b.name, func(t *testing.T) {
			b.db.SetObjectGrace(time.Hour)
			big := bigString("survivor")
			saveWithOutput(t, b.db, "inst-keep", big)
			saveWithOutput(t, b.db, "inst-drop", big)
			ref := outputRef(t, b.db, "inst-keep")

			// One instance moves on. Its claim goes; the content must not — and must not go at
			// the sweep either, which is the version a grace window could hide.
			replaceOutput(t, b.db, "inst-drop", "small")
			if n, err := b.db.CollectObjects(nowPlusHours(2)); err != nil {
				t.Fatalf("CollectObjects: %v", err)
			} else if n != 0 {
				t.Fatalf("the sweep collected %d objects while an instance still held one", n)
			}

			v, err := b.db.ResolveObject(context.Background(), ref)
			if err != nil || v != big {
				t.Fatalf("a release destroyed content another instance still holds: %v (err=%v)", v, err)
			}

			// Once the LAST claim goes it becomes collectable — after its window, not before.
			// The window starts when a sweep NOTICES nothing claims it, so the first sweep marks
			// and a later one collects.
			replaceOutput(t, b.db, "inst-keep", "small")
			if _, err := b.db.ResolveObject(context.Background(), ref); err != nil {
				t.Fatalf("released content vanished inside its grace window: %v", err)
			}
			if n, err := b.db.CollectObjects(nowPlusHours(2)); err != nil || n != 0 {
				t.Fatalf("the marking sweep collected it immediately: n=%d err=%v", n, err)
			}
			if _, err := b.db.ResolveObject(context.Background(), ref); err != nil {
				t.Fatalf("released content vanished inside its grace window: %v", err)
			}
			if n, err := b.db.CollectObjects(nowPlusHours(4)); err != nil || n != 1 {
				t.Fatalf("expected 1 object collected past the window, got n=%d err=%v", n, err)
			}
		})
	}
}

// TestObjects_ReleasedObjectIsResurrectedByAReWrite: an object on nothing but a grace claim is
// unclaimed, not dead. Writing the same bytes again claims the row that is already there, and no
// copy is made — the ordinary case for a task looping over two alternating values.
func TestObjects_ReleasedObjectIsResurrectedByAReWrite(t *testing.T) {
	for _, b := range testBackends(t) {
		t.Run(b.name, func(t *testing.T) {
			b.db.SetObjectGrace(time.Hour)
			v1, v2 := bigString("alt1"), bigString("alt2")
			saveWithOutput(t, b.db, "inst-alt", v1)
			ref1 := outputRef(t, b.db, "inst-alt")

			replaceOutput(t, b.db, "inst-alt", v2) // v1 released, in grace
			replaceOutput(t, b.db, "inst-alt", v1) // and claimed again

			if got := outputRef(t, b.db, "inst-alt"); got.Ref != ref1.Ref {
				t.Fatalf("re-writing identical content produced a different object: %s vs %s", got.Ref, ref1.Ref)
			}
			// The proof it is alive rather than merely still inside its window: sweeps well past
			// the window leave it, because a real claim is holding it now. The first marks v2
			// (nothing claims it), the second collects it; v1 survives both.
			if n, err := b.db.CollectObjects(nowPlusHours(48)); err != nil || n != 0 {
				t.Fatalf("the marking sweep collected something: n=%d err=%v", n, err)
			}
			if n, err := b.db.CollectObjects(nowPlusHours(50)); err != nil {
				t.Fatalf("CollectObjects: %v", err)
			} else if n != 1 {
				// v2 is the one that should go; v1 is claimed.
				t.Fatalf("expected only the other value collected, got n=%d", n)
			}
			if v, err := b.db.ResolveObject(context.Background(), ref1); err != nil || v != v1 {
				t.Fatalf("a resurrected object did not survive the sweep: %v (err=%v)", v, err)
			}
		})
	}
}

// TestObjects_ResurrectionAgainstALiveSweeper drives release-then-resurrect concurrently with the
// collector and asserts the invariant that matters: no instance is left holding a claim on
// content that is gone.
//
// What it does NOT prove, stated so the coverage is not overread: that `PutObject` must be
// ON CONFLICT DO UPDATE rather than DO NOTHING. That rule guards a specific interleaving — the
// sweep deleting between a writer taking the conflict path and its claim becoming visible — and
// swapping DO UPDATE for DO NOTHING does not fail this test, on either engine. The lock is
// reasoned about in specs/object-store.md and would be pinned only by a deterministic
// two-transaction interleaving, which needs a hook this package does not have. Kept because it
// exercises the path and catches grosser breakage, not because it catches that.
func TestObjects_ResurrectionAgainstALiveSweeper(t *testing.T) {
	for _, b := range testBackends(t) {
		t.Run(b.name, func(t *testing.T) {
			b.db.SetObjectGrace(0) // no window: every release is immediately collectable
			const workers, rounds = 4, 40
			big, small := bigString("raced"), "small"
			for i := range workers {
				saveWithOutput(t, b.db, fmt.Sprintf("inst-race-%d", i), big)
			}

			stop := make(chan struct{})
			var sweeps atomic.Int64
			go func() {
				for {
					select {
					case <-stop:
						return
					default:
						if _, err := b.db.CollectObjects(dbpkg.Now().UnixMilli()); err == nil {
							sweeps.Add(1)
						}
					}
				}
			}()

			var wg sync.WaitGroup
			for i := range workers {
				wg.Add(1)
				go func() {
					defer wg.Done()
					id := fmt.Sprintf("inst-race-%d", i)
					for range rounds {
						// Release the shared content, then resurrect it. The resurrect is the
						// half that races: the row is usually still there, so the writer takes
						// the conflict path and must hold it against the sweeper.
						replaceOutput(t, b.db, id, small)
						replaceOutput(t, b.db, id, big)
					}
				}()
			}
			wg.Wait()
			close(stop)

			// Every instance ended holding the big value. Each claim must resolve: a claim on
			// missing content is the dangling ref DO NOTHING produces.
			for i := range workers {
				id := fmt.Sprintf("inst-race-%d", i)
				ref := outputRef(t, b.db, id)
				v, err := b.db.ResolveObject(context.Background(), ref)
				if err != nil {
					t.Fatalf("%s holds a claim on content that is gone (%d sweeps): %v", id, sweeps.Load(), err)
				}
				if v != big {
					t.Fatalf("%s resolved to the wrong value", id)
				}
			}
		})
	}
}

// resolveAll materializes every marker in a context, which is what a CLIENT now does with the
// objects section a response lists. HydrateContext used to do it server-side behind
// ?resolve=true; both are gone, and a test doing it by hand is closer to the real path than a
// server-side convenience was.
func resolveAll(t *testing.T, db *dbpkg.DB, inst *model.ProcessInstance) {
	t.Helper()
	var walk func(v any) any
	walk = func(v any) any {
		switch t2 := v.(type) {
		case *model.ObjectRef:
			got, err := db.ResolveObject(context.Background(), t2)
			if err != nil {
				t.Fatalf("ResolveObject %s: %v", t2.Ref, err)
			}
			return got
		case map[string]any:
			for k, val := range t2 {
				t2[k] = walk(val)
			}
			return t2
		}
		return v
	}
	for k, v := range inst.State {
		inst.State[k] = walk(v)
	}
}

// TestObjects_ExternalInputClaimIsReleased: a task input's externalized bundle is claimed while
// the task is parked and released when it resolves. The release depends on the ref being in the
// instance's LOADED set when the row is read -- the write path drops what it loaded and no
// longer references, so a ref missing from that set is a claim nothing can ever drop, and the
// object is held for the life of the database by an instance that finished with it.
func TestObjects_ExternalInputClaimIsReleased(t *testing.T) {
	for _, b := range testBackends(t) {
		t.Run(b.name, func(t *testing.T) {
			b.db.SetObjectGrace(time.Hour)
			bundle := bigString("bundle")
			inst := &model.ProcessInstance{
				ID: "inst-extobj", ProcessName: "test", Task: "run", Status: model.StatusRunning,
				WaitState: model.WaitStateExternal,
				State: map[string]any{
					model.StateExternal: map[string]any{
						"task_id": "run",
						"input":   map[string]any{"code": bundle, "n": 1},
					},
				},
			}
			if err := b.db.SaveInstance(inst); err != nil {
				t.Fatalf("SaveInstance: %v", err)
			}

			parked, err := b.db.GetInstance("inst-extobj")
			if err != nil {
				t.Fatalf("GetInstance: %v", err)
			}
			ext := parked.State[model.StateExternal].(map[string]any)
			ref, ok := ext["input"].(map[string]any)["code"].(*model.ObjectRef)
			if !ok {
				t.Fatalf("the bundle was not externalized: %T", ext["input"].(map[string]any)["code"])
			}
			if n, err := b.db.CountObjectRefs(ref.Ref); err != nil || n != 1 {
				t.Fatalf("claims while parked = %d (err=%v), want 1", n, err)
			}

			// The task resolves: _external goes, and the claim must go with it.
			delete(parked.State, model.StateExternal)
			parked.WaitState = model.WaitStateNone
			if err := b.db.UpdateInstanceProgress(parked); err != nil {
				t.Fatalf("UpdateInstanceProgress: %v", err)
			}

			n, err := b.db.CountObjectRefs(ref.Ref)
			if err != nil {
				t.Fatalf("CountObjectRefs: %v", err)
			}
			// No claim left at all: the instance released it, and the grace window is a mark the
			// sweep keeps rather than a claim anyone stamps.
			if n != 0 {
				t.Fatalf("claims after resolution = %d, want 0 — the instance claim leaked", n)
			}
			if _, err := b.db.ResolveObject(context.Background(), ref); err != nil {
				t.Fatalf("released bundle vanished before any sweep ran: %v", err)
			}
			if swept, err := b.db.CollectObjects(nowPlusHours(2)); err != nil || swept != 0 {
				t.Fatalf("the marking sweep collected it immediately: swept=%d err=%v", swept, err)
			}
			if _, err := b.db.ResolveObject(context.Background(), ref); err != nil {
				t.Fatalf("released bundle vanished inside its grace window: %v", err)
			}
			if swept, err := b.db.CollectObjects(nowPlusHours(4)); err != nil || swept != 1 {
				t.Fatalf("past the window: swept=%d err=%v, want 1", swept, err)
			}
		})
	}
}

// A resurrected object gets a FRESH window when it is released again -- the mark from its previous
// release must not survive the re-claim.
//
// Without the clear, release -> sweep (marks) -> re-claim -> release leaves a mark already older
// than the window, so the next sweep collects the content on the spot and a reference handed out
// moments earlier resolves to nothing. The object is safe while it is CLAIMED (the delete checks
// claims first), which is exactly why a stale mark stays invisible until the second release.
//
// Two things clear it, deliberately: PutObject's conflict path, which is the moment a writer
// re-claims content and already writes the row to take its lock, and the sweep's own pass for a
// claim added without re-writing content. specs/object-store.md.
func TestObjects_ResurrectionClearsTheReleaseMark(t *testing.T) {
	for _, b := range testBackends(t) {
		t.Run(b.name, func(t *testing.T) {
			b.db.SetObjectGrace(time.Hour)
			v1, v2 := bigString("churn1"), bigString("churn2")
			saveWithOutput(t, b.db, "inst-churn", v1)
			ref := outputRef(t, b.db, "inst-churn")

			// Release v1, and let a sweep notice: it is now marked.
			replaceOutput(t, b.db, "inst-churn", v2)
			if _, err := b.db.CollectObjects(nowPlusHours(0)); err != nil {
				t.Fatalf("CollectObjects: %v", err)
			}

			// Re-claim it, well past the point where that first mark would have expired.
			replaceOutput(t, b.db, "inst-churn", v1)
			if _, err := b.db.CollectObjects(nowPlusHours(5)); err != nil {
				t.Fatalf("CollectObjects: %v", err)
			}
			if _, err := b.db.ResolveObject(context.Background(), ref); err != nil {
				t.Fatalf("a re-claimed object was collected on its old mark: %v", err)
			}

			// Release it again: the window must start now, not at the first release.
			replaceOutput(t, b.db, "inst-churn", v2)
			if _, err := b.db.CollectObjects(nowPlusHours(6)); err != nil {
				t.Fatalf("CollectObjects: %v", err)
			}
			if _, err := b.db.ResolveObject(context.Background(), ref); err != nil {
				t.Fatalf("a re-released object lost its grace window to a stale mark: %v", err)
			}

			// And it is still bounded: past the fresh window it goes.
			if _, err := b.db.CollectObjects(nowPlusHours(8)); err != nil {
				t.Fatalf("CollectObjects: %v", err)
			}
			if _, err := b.db.ResolveObject(context.Background(), ref); err == nil {
				t.Fatal("the object outlived its fresh window")
			}
		})
	}
}

// The claim the sweeper never sees: marked, re-claimed and released again all BETWEEN two sweeps.
//
// Only the content upsert can catch this one. The sweep's own clear runs when it observes a live
// claim, and here there is no observation to make — by the time the next sweep looks, the object
// is unclaimed again and carrying a mark from before the claim it never saw. Left alone that mark
// is already older than the window, so the content goes with no grace at all.
//
// PutObject's conflict path clears it because that is the instant the claim is made, and the row
// is being written anyway to take the sweep's lock. specs/object-store.md.
func TestObjects_AClaimBetweenTwoSweepsStillEarnsAWindow(t *testing.T) {
	for _, b := range testBackends(t) {
		t.Run(b.name, func(t *testing.T) {
			b.db.SetObjectGrace(time.Hour)
			v1, v2 := bigString("blink1"), bigString("blink2")
			saveWithOutput(t, b.db, "inst-blink", v1)
			ref := outputRef(t, b.db, "inst-blink")

			replaceOutput(t, b.db, "inst-blink", v2)
			if _, err := b.db.CollectObjects(nowPlusHours(0)); err != nil {
				t.Fatalf("CollectObjects: %v", err)
			}

			// Claimed and released again with NO sweep in between: the sweeper sees neither event.
			replaceOutput(t, b.db, "inst-blink", v1)
			replaceOutput(t, b.db, "inst-blink", v2)

			// Long after the FIRST mark would have expired. The claim in between must have reset
			// the clock, or this collects content whose window never ran.
			if _, err := b.db.CollectObjects(nowPlusHours(5)); err != nil {
				t.Fatalf("CollectObjects: %v", err)
			}
			if _, err := b.db.ResolveObject(context.Background(), ref); err != nil {
				t.Fatalf("a claim the sweeper never saw left the object on a stale mark, and it was collected with no window: %v", err)
			}
		})
	}
}

// TestState_RoundTripsWhole pins that state is RECONSTRUCTIBLE: everything an instance holds
// comes back from a write and a read unchanged -- the slots a definition declares, the slots
// only the engine writes, and the values too large to sit inline.
//
// The set is CLOSED: encodeState handles these keys and no others, so a slot added to storage
// without being added here has nothing asserting it survives, and a key outside the set is
// dropped rather than stored. Both halves are checked, because the second is what makes the
// first exhaustive rather than merely long.
//
// Values go in through the SAME decoder storage uses, so the comparison is of what was stored
// against what came back, not of Go literals against decoded JSON.
func TestState_RoundTripsWhole(t *testing.T) {
	// Past the 2 KiB cutoff, so at least one slot is reconstructed FROM THE OBJECT STORE rather
	// than from the row -- a round trip that never externalizes anything proves the easy half.
	blob := strings.Repeat("b", 4*1024)
	raw := fmt.Sprintf(`{
	  "input":   {"n": 9007199254740993, "amount": 123456789.123456789, "nested": {"deep": [1, null, "x"]}},
	  "outputs": {"first": {"v": 1}, "huge": {"blob": %q}, "nothing": null},
	  "output":  {"done": true},
	  "error":   {"task": "t", "code": "boom", "message": "m", "data": {"why": "x"}, "child_index": 2},
	  "_error_data": {"retry_after": 3600},
	  "_external": {"input": {"k": 1}},
	  "_spawn_child_key": "out",
	  "_spawn_index": 0
	}`, blob)

	for _, b := range testBackends(t) {
		t.Run(b.name, func(t *testing.T) {
			var stored map[string]any
			if err := numeric.Decode([]byte(raw), &stored); err != nil {
				t.Fatalf("decode fixture: %v", err)
			}
			var written map[string]any
			if err := numeric.Decode([]byte(raw), &written); err != nil {
				t.Fatalf("decode fixture: %v", err)
			}
			// Dropped rather than stored: the closed set has an outside, and this is it.
			// _children is in it deliberately -- a parent's children are DERIVED from the child
			// rows (db.ChildrenOfInstance), and a copy on the parent would be a second source.
			written["invented"] = "not part of state"
			written["_children"] = map[string]any{"spawn": "derived, not stored"}
			written["_spawn_action_type"] = "child_map" // the PARENT's task says this, not the child

			inst := &model.ProcessInstance{
				ID: "roundtrip", ProcessName: "test", ProcessVersion: 1, Task: "step1",
				Status: model.StatusPaused, State: written,
			}
			if err := b.db.SaveInstance(inst); err != nil {
				t.Fatalf("SaveInstance: %v", err)
			}
			got, err := b.db.GetInstance("roundtrip")
			if err != nil {
				t.Fatalf("GetInstance: %v", err)
			}
			// The object-store half is only exercised if something actually left the row. Without
			// this the fixture could shrink under the cutoff and the test would still pass, having
			// quietly stopped reconstructing anything from the store.
			outs := got.State["outputs"].(map[string]any)["huge"].(map[string]any)
			if _, isRef := outs["blob"].(*model.ObjectRef); !isRef {
				t.Fatalf("fixture no longer externalizes: blob came back as %T, so the store is untested", outs["blob"])
			}
			resolveAll(t, b.db, got)

			for _, outside := range []string{"invented", "_children", "_spawn_action_type"} {
				if _, ok := got.State[outside]; ok {
					t.Errorf("%q is outside the closed set but was stored; encodeState must drop what it does not name", outside)
				}
				delete(got.State, outside)
			}

			want, _ := json.Marshal(stored)
			have, _ := json.Marshal(got.State)
			if string(want) != string(have) {
				t.Errorf("state did not survive the round trip\n  stored: %s\n  read:   %s", want, have)
			}
		})
	}
}

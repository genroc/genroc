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
				ContextData: map[string]any{
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
			if _, isRef := got.ContextData["input"].(*model.ObjectRef); !isRef {
				t.Fatalf("expected input to be an *ObjectRef marker, got %T", got.ContextData["input"])
			}
			outs := got.ContextData["outputs"].(map[string]any)
			if outs["small"] != "tiny" {
				t.Errorf("small output: got %v, want tiny", outs["small"])
			}
			if _, isRef := outs["huge"].(*model.ObjectRef); !isRef {
				t.Fatalf("expected huge output to be a marker, got %T", outs["huge"])
			}

			resolveAll(t, b.db, got)
			if got.ContextData["input"] != big {
				t.Errorf("hydrated input mismatch")
			}
			if got.ContextData["outputs"].(map[string]any)["huge"] != bigString("out") {
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
			b.db.SetObjectRetention(time.Hour)
			b.db.SetObjectGrace(time.Hour)

			inst := &model.ProcessInstance{
				ID:          "inst-deref",
				ProcessName: "test",
				ContextData: map[string]any{"outputs": map[string]any{"loop": bigString("v1")}},
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
			oldRef, ok := r1.ContextData["outputs"].(map[string]any)["loop"].(*model.ObjectRef)
			if !ok {
				t.Fatalf("expected loop output to be a marker")
			}
			r1.ContextData["outputs"].(map[string]any)["loop"] = bigString("v2")
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
			if r2.ContextData["outputs"].(map[string]any)["loop"] != bigString("v2") {
				t.Errorf("v2 output not preserved")
			}
		})
	}
}

// TestObjects_LogReferencedSurvivesDeref verifies that an object a log references is
// NOT deleted when the context slot sharing it is dereferenced — it stays fetchable
// via the log endpoint until the retention horizon, then the GC sweep reclaims it.
func TestObjects_LogReferencedSurvivesDeref(t *testing.T) {
	for _, b := range testBackends(t) {
		t.Run(b.name, func(t *testing.T) {
			b.db.SetObjectRetention(time.Hour)

			// A secret-free value: its context object and its (pre-redacted) log object
			// are byte-identical, so they share one row.
			val := bigString("shared")
			content, _ := json.Marshal(val) // exactly what the context encoder stores

			// Context-reference it via an instance output...
			inst := &model.ProcessInstance{
				ID:          "inst-shared",
				ProcessName: "test",
				Task:        "",
				ContextData: map[string]any{"outputs": map[string]any{"out": val}},
				Status:      model.StatusRunning,
			}
			if err := b.db.SaveInstance(inst); err != nil {
				t.Fatalf("SaveInstance: %v", err)
			}
			// ...and log-reference the identical content (shares the same row).
			ref, err := b.db.WriteLogObject("inst-shared", string(content))
			if err != nil {
				t.Fatalf("WriteLogObject: %v", err)
			}

			// Dereference the context slot (replace the output with a small value).
			r, err := b.db.GetInstance("inst-shared")
			if err != nil {
				t.Fatalf("GetInstance: %v", err)
			}
			r.ContextData["outputs"].(map[string]any)["out"] = "small"
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

			// Before the horizon the sweep leaves it; past the horizon it is reclaimed.
			if n, err := b.db.CollectObjects(nowPlusHours(0)); err != nil || n != 0 {
				t.Fatalf("premature sweep: n=%d err=%v", n, err)
			}
			if n, err := b.db.CollectObjects(nowPlusHours(2)); err != nil || n != 1 {
				t.Fatalf("expected 1 object swept after horizon, got n=%d err=%v", n, err)
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
		ContextData: map[string]any{"outputs": map[string]any{"out": v}},
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
	inst.ContextData["outputs"].(map[string]any)["out"] = v
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
	ref, ok := inst.ContextData["outputs"].(map[string]any)["out"].(*model.ObjectRef)
	if !ok {
		t.Fatalf("%s: output was not externalized (%T)", id, inst.ContextData["outputs"].(map[string]any)["out"])
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
			replaceOutput(t, b.db, "inst-keep", "small")
			if _, err := b.db.ResolveObject(context.Background(), ref); err != nil {
				t.Fatalf("released content vanished inside its grace window: %v", err)
			}
			if n, err := b.db.CollectObjects(nowPlusHours(2)); err != nil || n != 1 {
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
			// The proof it is alive rather than merely still inside its window: a sweep well past
			// the window leaves it, because a real claim is holding it now.
			if n, err := b.db.CollectObjects(nowPlusHours(48)); err != nil {
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
	for k, v := range inst.ContextData {
		inst.ContextData[k] = walk(v)
	}
}

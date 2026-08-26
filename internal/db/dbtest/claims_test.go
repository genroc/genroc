package dbtest

import (
	"strings"
	"testing"

	"genroc/internal/model"
)

// An instance claims what its slots REFERENCE, not what its write produced.
//
// The two differ as soon as a marker can be copied rather than loaded: an expression carries a
// reference from one slot into another, or a value crosses onto a second instance's row. Claiming
// only newly written objects leaves such a row pointing at content nothing there holds, and the
// sweep deletes content when no claim remains -- so the row survives and its value does not.
// specs/lazy-context.md, specs/object-store.md.
func TestClaims_FollowReferencesNotWrites(t *testing.T) {
	for _, b := range testBackends(t) {
		t.Run(b.name, func(t *testing.T) {
			big := strings.Repeat("R", 8*1024)

			// The first instance writes the object and claims it.
			first := &model.ProcessInstance{
				ID: "claim-writer", ProcessName: "test", Status: model.StatusRunning,
				State: map[string]any{"input": map[string]any{"blob": big}},
			}
			if err := b.db.SaveInstance(first); err != nil {
				t.Fatalf("SaveInstance(first): %v", err)
			}
			loaded, err := b.db.GetInstance("claim-writer")
			if err != nil {
				t.Fatalf("GetInstance: %v", err)
			}
			ref, ok := loaded.State["input"].(map[string]any)["blob"].(*model.ObjectRef)
			if !ok {
				t.Fatalf("setup: blob is %T, want a marker", loaded.State["input"].(map[string]any)["blob"])
			}
			if n, err := b.db.CountObjectRefs(ref.Ref); err != nil || n != 1 {
				t.Fatalf("after the writing instance: %d claims (err=%v), want 1", n, err)
			}

			// The second writes the MARKER -- it produces no object, so a claim keyed to what
			// this write created would never appear.
			second := &model.ProcessInstance{
				ID: "claim-copier", ProcessName: "test", Status: model.StatusRunning,
				State: map[string]any{"input": map[string]any{"blob": ref}},
			}
			if err := b.db.SaveInstance(second); err != nil {
				t.Fatalf("SaveInstance(second): %v", err)
			}
			n, err := b.db.CountObjectRefs(ref.Ref)
			if err != nil {
				t.Fatalf("CountObjectRefs: %v", err)
			}
			if n != 2 {
				t.Fatalf("%d claims, want 2: the copying instance references the object and must hold it, or the writer's release leaves it reading content that is gone", n)
			}

			// And the proof that the claim is load-bearing: the writer lets go, the copier does
			// not, and the sweep must leave the content alone.
			loaded.State["input"] = map[string]any{"blob": "small"}
			if err := b.db.UpdateInstanceProgress(loaded); err != nil {
				t.Fatalf("UpdateInstanceProgress: %v", err)
			}
			if _, err := b.db.CollectObjects(nowPlusHours(2)); err != nil {
				t.Fatalf("CollectObjects: %v", err)
			}
			if _, _, err := b.db.GetObjectContent(ref.Ref); err != nil {
				t.Fatalf("the object was swept while a second instance still referenced it: %v", err)
			}
		})
	}
}

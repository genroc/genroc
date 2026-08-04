package api

import (
	"encoding/json"
	"testing"
)

func batchApply(h *Handlers, channel string, defs ...any) Reply {
	payload, _ := json.Marshal(map[string]any{
		"channel":     channel,
		"definitions": defs,
	})
	return h.Handle(Envelope{Action: "put_definitions_batch", Payload: payload})
}

// TestApplyBatch_VersionedSelfRefCreatesDep verifies that a child_process entry that
// names the same process but with an explicit version is stored as a dependency row,
// not silently dropped as a self-reference.
//
// This stays a Go test because it asserts dependency baking via GetDependencyVersion,
// which no HTTP endpoint exposes. The rest of the channel/apply behavior is covered
// end-to-end by tests/integration/channels_test.ts and tests/cli/definitions_test.ts.
func TestApplyBatch_VersionedSelfRefCreatesDep(t *testing.T) {
	h, cleanup := newTestHandlers(t)
	defer cleanup()

	// v1: plain recursive process (no versioned self-ref).
	v1 := map[string]any{
		"name": "recursive",
		"tasks": []any{
			map[string]any{"id": "recurse", "action": map[string]any{
				"type": "child_map",
				"children": map[string]any{
					"self": map[string]any{"name": "recursive"},
				},
			}, "switch": []any{map[string]any{"goto": "end"}}},
		},
	}
	batchApply(h, "latest", v1)

	// v2: references recursive@v1 explicitly via child_map — both self-ref variants.
	v2 := map[string]any{
		"name": "recursive",
		"tasks": []any{
			map[string]any{"id": "recurse", "action": map[string]any{
				"type": "child_map",
				"children": map[string]any{
					"pinned": map[string]any{"name": "recursive", "version": 1},
					"latest": map[string]any{"name": "recursive"},
				},
			}, "switch": []any{map[string]any{"goto": "end"}}},
		},
	}
	r := batchApply(h, "latest", v2)
	if !r.OK {
		t.Fatalf("apply v2 failed: %s", r.Error)
	}

	// The pinned reference is baked as a dependency on recursive@v1.
	pinnedV, err := h.db.GetDependencyVersion("recursive", 2, "recurse", "pinned")
	if err != nil {
		t.Fatalf("GetDependencyVersion(pinned): %v", err)
	}
	if pinnedV != 1 {
		t.Errorf("expected pinned dep on recursive@v1, got recursive@v%d", pinnedV)
	}
	// The unpinned "latest" reference is resolved dynamically, not baked as a dep row.
	if _, err := h.db.GetDependencyVersion("recursive", 2, "recurse", "latest"); err == nil {
		t.Errorf("expected no baked dep row for unpinned \"latest\" reference")
	}
}

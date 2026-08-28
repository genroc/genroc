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
	// In-process callers supply their own identity: the transports attach one, and Handle
	// refuses an envelope without it rather than defaulting to open.
	return h.Handle(Envelope{Action: "put_definitions_batch", Payload: payload, principal: anonymousAdmin()})
}

// A versioned self-reference must be stored as a dependency row, not dropped as a self-ref.
// Go rather than e2e because it asserts baking via GetDependencyVersion, which no endpoint
// exposes.
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

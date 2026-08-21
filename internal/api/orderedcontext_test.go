package api

import (
	"encoding/json"
	"strings"
	"testing"
)

// A completed instance persisted before output_order was unique keeps its duplicates for
// good — nothing appends to it again to heal them. Emitting one twice would put a repeated
// key in the JSON object, which every parser resolves by silently keeping the last, so the
// render has to be tolerant of what the writer no longer produces.
func TestOrderedContext_ARepeatedOrderEntryIsSerialisedOnce(t *testing.T) {
	ctx := map[string]any{
		"outputs": map[string]any{
			"first": map[string]any{"n": 1},
			"call":  map[string]any{"n": 2},
		},
		"output_order": []any{"first", "call", "call", "call"},
	}

	raw, ok := orderedContext(ctx)["outputs"].(json.RawMessage)
	if !ok {
		t.Fatalf("outputs is %T, want json.RawMessage", orderedContext(ctx)["outputs"])
	}
	if n := strings.Count(string(raw), `"call":`); n != 1 {
		t.Errorf(`"call" serialised %dx: %s`, n, raw)
	}
	// Order is still the contract: the surviving position is where the task first appeared.
	if !strings.HasPrefix(string(raw), `{"first":`) {
		t.Errorf("dedup reordered the outputs: %s", raw)
	}
}

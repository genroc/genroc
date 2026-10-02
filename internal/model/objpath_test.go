package model

import (
	"reflect"
	"testing"
)

func ref(h string, n int64) *ObjectRef { return &ObjectRef{Ref: h, Size: n} }

// Extraction discriminates on the Go type, never the shape — the property the wire design
// rests on.
func TestExtract_LeavesUserDataThatLooksLikeAReference(t *testing.T) {
	mimic := map[string]any{"ref": "not-a-handle", "size": float64(7)}
	ctx := map[string]any{"outputs": map[string]any{"decoy": mimic}}

	var got []*ObjectRef
	out := Extract(ctx, []any{"context"}, &got)

	if len(got) != 0 {
		t.Fatalf("listed %d objects for user data shaped like a reference: %+v", len(got), got)
	}
	outputs := out.(map[string]any)["outputs"].(map[string]any)
	if !reflect.DeepEqual(outputs["decoy"], mimic) {
		t.Fatalf("user data was altered: %+v", outputs["decoy"])
	}
}

// Appending to the parent's path in place would give siblings one backing array, so one value
// overwrites another when a client splices.
func TestExtract_SiblingsGetDistinctPaths(t *testing.T) {
	// Three deep, deliberately: append() aliases only once the parent slice has spare
	// capacity, which Go's growth gives at length three.
	ctx := map[string]any{
		"outputs": map[string]any{
			"group": map[string]any{"a": ref("aaa", 10), "b": ref("bbb", 20)},
		},
		"input": ref("ccc", 30),
	}
	var got []*ObjectRef
	Extract(ctx, []any{"context"}, &got)

	if len(got) != 3 {
		t.Fatalf("got %d entries, want 3", len(got))
	}
	seen := map[string]string{}
	for _, e := range got {
		key := ""
		for _, p := range e.Path {
			key += "/" + p.(string)
		}
		if prev, dup := seen[key]; dup {
			t.Fatalf("two objects claim path %s (%s and %s) — sibling paths share a backing array", key, prev, e.Ref)
		}
		seen[key] = e.Ref
	}
	if seen["/context/outputs/group/a"] != "aaa" || seen["/context/outputs/group/b"] != "bbb" || seen["/context/input"] != "ccc" {
		t.Fatalf("paths do not name their own values: %+v", seen)
	}
}

// A client ignoring the section must see a MISSING value, not plausible data.
func TestExtract_RemovesRatherThanMarks(t *testing.T) {
	ctx := map[string]any{"input": ref("aaa", 10), "small": "kept"}
	var got []*ObjectRef
	out := Extract(ctx, []any{"context"}, &got).(map[string]any)

	if _, present := out["input"]; present {
		t.Fatalf("the externalized slot is still present: %+v", out["input"])
	}
	if out["small"] != "kept" {
		t.Fatalf("an inline sibling was disturbed: %+v", out["small"])
	}
	if len(got) != 1 || got[0].Ref != "aaa" || got[0].Size != 10 {
		t.Fatalf("entry is wrong: %+v", got)
	}
}

// Nothing reaches this branch yet: only whole slots externalize today. An index is a number in
// the path, not the decimal string a JSON Pointer would force.
func TestExtract_InsideAnArray(t *testing.T) {
	ctx := map[string]any{"outputs": map[string]any{"list": []any{"small", ref("aaa", 10)}}}
	var got []*ObjectRef
	out := Extract(ctx, []any{"context"}, &got)

	if len(got) != 1 {
		t.Fatalf("got %d entries, want 1", len(got))
	}
	want := []any{"context", "outputs", "list", 1}
	if !reflect.DeepEqual(got[0].Path, want) {
		t.Fatalf("path = %#v, want %#v (an index is a NUMBER, not a string)", got[0].Path, want)
	}
	list := out.(map[string]any)["outputs"].(map[string]any)["list"].([]any)
	if list[0] != "small" || list[1] != nil {
		t.Fatalf("array element handling is wrong: %+v", list)
	}
}

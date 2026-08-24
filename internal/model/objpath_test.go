package model

import (
	"reflect"
	"testing"
)

func ref(h string, n int64) *ObjectRef { return &ObjectRef{Ref: h, Size: n} }

// TestExtract_LeavesUserDataThatLooksLikeAReference is the property the whole wire design
// rests on. The old shape put {"ref": …, "size": …} INSIDE a context value, indistinguishable
// from a process whose output legitimately has those two keys. Extraction discriminates on the
// Go type, never on the shape, so a user value that mimics a marker is untouched and unlisted.
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

// TestExtract_SiblingsGetDistinctPaths catches a shared backing array: appending to the
// parent's path in place gives every sibling the same slice, so two entries name one location and
// the second value silently overwrites the first when a client splices.
func TestExtract_SiblingsGetDistinctPaths(t *testing.T) {
	// Nested three deep before the siblings, deliberately. append() only aliases once the
	// parent slice has SPARE capacity, and Go's growth gives that at length three — so a
	// shallower fixture passes with the bug present, which this test did on its first writing.
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

// TestExtract_RemovesRatherThanMarks: the slot is gone, not null and not a marker. A
// client that ignores the section must see a MISSING value rather than a plausible object it
// will treat as data.
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

// TestExtract_InsideAnArray pins the branch nothing reaches yet: a whole value-slot is
// what externalizes today, so no path currently carries an index. ObjectRef.Path is reserved for
// granular externalization, and this is what that would produce — a number in the path, not the
// decimal string a JSON Pointer would force.
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

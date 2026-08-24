package model

import (
	"fmt"
	"reflect"
	"testing"
)

// countingCtx builds a Context over data whose only externalized value is `hash`, recording
// every load so a test can assert on the count rather than on the value it got back.
func countingCtx(data map[string]any, hash string, content any) (*Context, *int) {
	loads := 0
	c := NewContext(data, func(h string) (any, error) {
		loads++
		if h != hash {
			return nil, fmt.Errorf("unexpected load of %s", h)
		}
		return content, nil
	}, nil)
	return c, &loads
}

// A slot cut in one place decodes with a marker AT that path, so the three cases are decided by
// where the walk stops -- no path comparison, and nothing loads unless a step must pass through.
func TestAt_LoadsOnlyWhenTheWalkStepsThroughAReference(t *testing.T) {
	ref := &ObjectRef{Ref: "abc", Size: 1000, Path: []any{"code"}}
	newData := func() map[string]any {
		return map[string]any{"outputs": map[string]any{
			"x": map[string]any{"code": ref, "meta": map[string]any{"n": 1}},
		}}
	}

	t.Run("disjoint path loads nothing", func(t *testing.T) {
		c, loads := countingCtx(newData(), "abc", "BODY")
		got, err := c.At("outputs", "x", "meta", "n")
		if err != nil {
			t.Fatalf("At: %v", err)
		}
		if got != 1 {
			t.Errorf("got %v, want 1 -- reading a sibling of the reference", got)
		}
		if *loads != 0 {
			t.Errorf("loaded %d objects reading a path that never meets one; the leaf-level cut buys nothing if a sibling read pulls the object in", *loads)
		}
	})

	t.Run("stopping above a reference keeps the marker", func(t *testing.T) {
		c, loads := countingCtx(newData(), "abc", "BODY")
		got, err := c.At("outputs", "x")
		if err != nil {
			t.Fatalf("At: %v", err)
		}
		m, ok := got.(map[string]any)
		if !ok {
			t.Fatalf("got %T, want the slot map", got)
		}
		if _, isRef := m["code"].(*ObjectRef); !isRef {
			t.Errorf("code = %T, want the marker intact: whoever copies this subtree must copy the reference, which is how an untouched value reaches the next write unloaded", m["code"])
		}
		if *loads != 0 {
			t.Errorf("loaded %d objects returning a subtree nobody read into", *loads)
		}
	})

	t.Run("stepping through a reference loads it once", func(t *testing.T) {
		c, loads := countingCtx(newData(), "abc", map[string]any{"body": "B"})
		got, err := c.At("outputs", "x", "code", "body")
		if err != nil {
			t.Fatalf("At: %v", err)
		}
		if got != "B" {
			t.Errorf("got %v, want B", got)
		}
		if _, err := c.At("outputs", "x", "code", "body"); err != nil {
			t.Fatalf("At (second): %v", err)
		}
		if *loads != 1 {
			t.Errorf("loaded %d times; the memo must make a second read free", *loads)
		}
	})
}

// The reason Context never writes back: the write path re-emits a marker with no new object, so
// destroying one costs a re-marshal and a re-hash to arrive at the hash it came off disk with.
func TestContext_NeverWritesLoadedValuesBackIntoTheData(t *testing.T) {
	ref := &ObjectRef{Ref: "abc", Size: 1000, Path: []any{"code"}}
	data := map[string]any{"outputs": map[string]any{
		"x": map[string]any{"code": ref},
	}}
	c, _ := countingCtx(data, "abc", "BODY")

	if _, err := c.At("outputs", "x", "code"); err != nil {
		t.Fatalf("At: %v", err)
	}
	if _, err := c.MaterializeAt("outputs", "x"); err != nil {
		t.Fatalf("MaterializeAt: %v", err)
	}

	slot := data["outputs"].(map[string]any)["x"].(map[string]any)
	if got, isRef := slot["code"].(*ObjectRef); !isRef || got.Ref != "abc" {
		t.Fatalf("the context data lost its marker (code = %#v); the next write would re-marshal and re-hash the value to reach the hash it already had", slot["code"])
	}
}

// Materialize copies. The argument is part of the live context, whose markers the write path
// still needs; an in-place walk was how the old resolveNested destroyed them.
func TestMaterialize_CopiesRatherThanFillingInTheContext(t *testing.T) {
	ref := &ObjectRef{Ref: "abc", Size: 10, Path: []any{"code"}}
	inner := map[string]any{"code": ref, "n": 1}
	data := map[string]any{"input": inner}
	c, _ := countingCtx(data, "abc", "BODY")

	got, err := c.Materialize(inner)
	if err != nil {
		t.Fatalf("Materialize: %v", err)
	}
	want := map[string]any{"code": "BODY", "n": 1}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Materialize = %#v, want %#v", got, want)
	}
	if _, isRef := inner["code"].(*ObjectRef); !isRef {
		t.Errorf("Materialize wrote through to its argument: code = %#v", inner["code"])
	}
}

// A path into a slot that has not been produced yet is a miss, not an error: a definition may
// legitimately name an output of a task that has not run.
func TestAt_MissingStepIsNilNotAnError(t *testing.T) {
	c, loads := countingCtx(map[string]any{"outputs": map[string]any{}}, "abc", nil)
	got, err := c.At("outputs", "never_ran", "field")
	if err != nil {
		t.Fatalf("At: %v", err)
	}
	if got != nil {
		t.Errorf("got %v, want nil", got)
	}
	if *loads != 0 {
		t.Errorf("loaded %d objects walking a path that does not exist", *loads)
	}
}

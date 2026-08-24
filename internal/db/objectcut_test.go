package db

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"genroc/internal/model"
)

func big(n int) string { return strings.Repeat("x", n) }

func encoded(t *testing.T, v any, refs []*model.ObjectRef) int64 {
	t.Helper()
	d, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	total := int64(len(d))
	for _, r := range refs {
		total += entrySize(r.Path)
	}
	return total
}

// A value already under the target is not touched at all: the question is whether the SLOT is
// too big, not whether some piece of it is large.
func TestCut_SmallValueIsUntouched(t *testing.T) {
	v := map[string]any{"a": "hello", "b": 42}
	out, refs, objs, err := cutForSize(v, 2048)
	if err != nil || len(refs) != 0 || len(objs) != 0 {
		t.Fatalf("refs=%d objs=%d err=%v, want none", len(refs), len(objs), err)
	}
	if fmt.Sprint(out) != fmt.Sprint(v) {
		t.Fatalf("value was altered: %v", out)
	}
}

// The motivating case, and the one a coarser cut would silently break: a bundle beside
// per-instance data must cut the BUNDLE, leaving the small field inline, or two instances hash
// different values and share nothing.
func TestCut_TakesTheBigLeafAndLeavesItsSibling(t *testing.T) {
	v := map[string]any{"code": big(50_000), "input": map[string]any{"n": 1}}
	out, refs, _, err := cutForSize(v, 2048)
	if err != nil {
		t.Fatalf("cut: %v", err)
	}
	if len(refs) != 1 {
		t.Fatalf("got %d refs, want 1: %+v", len(refs), refs)
	}
	if fmt.Sprint(refs[0].Path) != "[code]" {
		t.Fatalf("cut at %v, want [code]", refs[0].Path)
	}
	m := out.(map[string]any)
	if _, present := m["code"]; present {
		t.Fatal("the externalized leaf is still in the value")
	}
	if fmt.Sprint(m["input"]) != "map[n:1]" {
		t.Fatalf("the per-instance sibling was disturbed: %v", m["input"])
	}
}

// The failure the per-leaf threshold has: many pieces, none individually large, adding up to far
// more than the target. Every one is under the old 2 KiB line, so nothing moved.
func TestCut_ManySmallLeavesStillGetUnderTarget(t *testing.T) {
	v := map[string]any{}
	for i := range 100 {
		v[fmt.Sprintf("k%02d", i)] = big(1000)
	}
	out, refs, _, err := cutForSize(v, 2048)
	if err != nil {
		t.Fatalf("cut: %v", err)
	}
	if got := encoded(t, out, refs); got > 2048 {
		t.Fatalf("stored size %d is still over the 2048 target", got)
	}
}

// Fewest, largest: with one leaf big enough to do the job alone, its smaller siblings stay put.
func TestCut_StopsAsSoonAsItFits(t *testing.T) {
	v := map[string]any{"huge": big(50_000), "med1": big(900), "med2": big(900)}
	out, refs, _, err := cutForSize(v, 4096)
	if err != nil {
		t.Fatalf("cut: %v", err)
	}
	if len(refs) != 1 || fmt.Sprint(refs[0].Path) != "[huge]" {
		t.Fatalf("cut %+v, want only [huge]", refs)
	}
	m := out.(map[string]any)
	if m["med1"] == nil || m["med2"] == nil {
		t.Fatal("a sibling was externalized although the value already fit without it")
	}
}

// Coarsening: enough leaves that even cutting all of them leaves a skeleton over target, because
// each ref costs an entry. The cut must move UP, and a chosen parent must un-choose everything
// under it -- an object's content is opaque, so a ref nested inside would never resolve.
func TestCut_CoarsensAndNeverNestsARefInsideAnObject(t *testing.T) {
	group := map[string]any{}
	for i := range 60 {
		group[fmt.Sprintf("k%02d", i)] = big(200)
	}
	v := map[string]any{"group": group, "small": "keep"}

	out, refs, objs, err := cutForSize(v, 1024)
	if err != nil {
		t.Fatalf("cut: %v", err)
	}
	if got := encoded(t, out, refs); got > 1024 {
		t.Fatalf("stored size %d is still over the 1024 target", got)
	}
	// Whatever it chose, no object's content may contain a reference.
	for _, o := range objs {
		if strings.Contains(o.Content, `"ref"`) {
			t.Fatalf("an object's content carries a reference: %s", o.Content[:80])
		}
	}
	// And no two cuts may be nested in one another.
	for i, a := range refs {
		for j, b := range refs {
			if i != j && len(a.Path) < len(b.Path) && fmt.Sprint(b.Path)[:len(fmt.Sprint(a.Path))-1] == fmt.Sprint(a.Path)[:len(fmt.Sprint(a.Path))-1] {
				t.Fatalf("cut %v contains cut %v", a.Path, b.Path)
			}
		}
	}
}

// Dedup depends on two processes choosing the SAME cut for the same value. Go's map iteration is
// randomized, so a tie resolved by iteration order would produce different objects for identical
// content and share nothing -- quietly, since both still work.
//
// There are TWO independent defences and this catches losing both, not either: children are
// built in sorted key order, and equal candidates break their tie on path. Verified by removing
// each in turn (still deterministic) and then both (fails here). Keep both -- a single defence
// with no second is one edit from silent unsharing.
func TestCut_IsDeterministicAcrossEqualCandidates(t *testing.T) {
	build := func() map[string]any {
		m := map[string]any{}
		for i := range 30 {
			m[fmt.Sprintf("k%02d", i)] = big(900) // all the same size: every choice is a tie
		}
		return m
	}
	var first string
	for range 20 {
		_, refs, _, err := cutForSize(build(), 4096)
		if err != nil {
			t.Fatalf("cut: %v", err)
		}
		// By VALUE. fmt.Sprint over a slice of pointers prints addresses, which differ every
		// run and would make this pass on nothing at all.
		parts := make([]string, 0, len(refs))
		for _, r := range refs {
			parts = append(parts, fmt.Sprintf("%v=%s/%d", r.Path, r.Ref, r.Size))
		}
		got := strings.Join(parts, ",")
		if first == "" {
			first = got
		} else if got != first {
			t.Fatalf("the same value cut two different ways:\n%s\n%s", first, got)
		}
	}
}

// The floor: a value that is over target only by a hair must not be "fixed" by externalizing
// something smaller than the entry that replaces it, which would make the slot bigger.
func TestCut_DoesNotTakeLeavesSmallerThanTheirEntry(t *testing.T) {
	v := map[string]any{"a": big(30), "b": big(30), "c": big(30)}
	_, refs, _, err := cutForSize(v, 32) // unreachably small on purpose
	if err != nil {
		t.Fatalf("cut: %v", err)
	}
	for _, r := range refs {
		if len(r.Path) > 0 && r.Size < minExternalizeBytes {
			t.Fatalf("took a %d-byte leaf, under the %d floor: %v", r.Size, minExternalizeBytes, r.Path)
		}
	}
}

// TestCut_DoesNotMutateTheCallerValue: encoding produces column strings and must not reach back
// into the value the caller is still using. Cutting in place gutted a live instance context --
// the advance carried on with the leaves removed, and what surfaced first was an audit line
// reading "{}" where an input should have been.
func TestCut_DoesNotMutateTheCallerValue(t *testing.T) {
	inner := map[string]any{"code": big(50_000), "n": 1}
	v := map[string]any{"input": inner}

	out, refs, _, err := cutForSize(v, 2048)
	if err != nil || len(refs) == 0 {
		t.Fatalf("refs=%d err=%v, want a cut", len(refs), err)
	}
	if got, ok := inner["code"].(string); !ok || len(got) != 50_000 {
		t.Fatalf("the caller's value was gutted: code is now %#v", inner["code"])
	}
	// And the copy really was cut.
	if _, present := out.(map[string]any)["input"].(map[string]any)["code"]; present {
		t.Fatal("the returned value still carries the leaf that was externalized")
	}
}

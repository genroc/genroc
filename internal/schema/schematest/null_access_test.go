package schematest

import (
	"strings"
	"testing"
	"time"

	"genroc/internal/schema"
)

// Reading a property THROUGH a null is null, not an access error — matching the evaluator,
// which is what lets `a.x ?? b.x` fall through. Path-partitioned output inference depends on
// it: an absent-on-this-terminal output is typed null and ?? must take the other arm.

func TestNullAccess_PropertyOfNullIsNull(t *testing.T) {
	ctx := schema.Object().WithProperty("a", schema.Type("null"), true)
	got, err := ctx.Infer("a.v")
	if err != nil {
		t.Fatalf("reading through a null must yield null, not fail: %v", err)
	}
	if !got.IsNull() {
		t.Errorf("a.v = %s, want {\"type\":\"null\"}; a null propagates through access", jsonOf(t, got))
	}
}

func TestNullAccess_NestedPropertyOfNullIsStillNull(t *testing.T) {
	ctx := schema.Object().WithProperty("a", schema.Type("null"), true)
	got, err := ctx.Infer("a.v.deeper")
	if err != nil {
		t.Fatalf("null must propagate through a whole path, not just one step: %v", err)
	}
	if !got.IsNull() {
		t.Errorf("a.v.deeper = %s, want null", jsonOf(t, got))
	}
}

func TestNullAccess_CoalescingThroughAnAbsentSideTakesTheOther(t *testing.T) {
	// The exact shape a per-terminal context builds: one side present, the other null.
	ctx := schema.Object().
		WithProperty("present", schema.Object().WithProperty("v", schema.Type("boolean"), true), true).
		WithProperty("gone", schema.Type("null"), true)

	assertJSONEq(t, inferIn(t, ctx, "gone.v ?? present.v"), `{"type":"boolean"}`,
		"a null left operand must resolve to the right operand's type exactly")
	assertJSONEq(t, inferIn(t, ctx, "present.v ?? gone.v"), `{"type":"boolean"}`,
		"a non-null left operand must win, and the null right must not re-introduce null")
}

func TestNullAccess_PropertyOfAScalarIsStillAnError(t *testing.T) {
	// The change is narrow: only a null yields null. Reading a property of a string is
	// still an author error, and must keep saying so.
	ctx := schema.Object().WithProperty("s", schema.Type("string"), true)
	if _, err := ctx.Infer("s.v"); err == nil {
		t.Fatal("reading a property of a string must remain an error; only null propagates")
	}
}

func TestNullAccess_UndeclaredPropertyOfAnObjectIsStillAnError(t *testing.T) {
	ctx := schema.Object().WithProperty("o", schema.Object().WithProperty("v", schema.Type("integer"), true), true)
	_, err := ctx.Infer("o.nope")
	if err == nil {
		t.Fatal("a typo in a property name must stay an error; the null rule must not swallow it")
	}
	if !strings.Contains(err.Error(), "nope") {
		t.Errorf("error %q must name the missing field so the author can find the typo", err)
	}
}

func TestNullAccess_UnknownTopTypeIsStillRefused(t *testing.T) {
	// {} is opaque on purpose; it must not be confused with null, which is readable-as-null.
	ctx := schema.Object().WithProperty("u", schema.Schema{}, true)
	if _, err := ctx.Infer("u.v"); err == nil {
		t.Fatal("reading into the unknown type ({}) must stay an error — that is the point of unknown")
	}
}

// ── StripNull / HasNull symmetry ──────────────────────────────────────────────
// StripNull's contract is that HasNull is false afterwards. It used to drop only whole null
// arms, so a null inside an arm's type list survived and HasNull disagreed with it.

func TestStripNull_RemovesNullHidingInsideAUnionArm(t *testing.T) {
	s := schema.OneOf(schema.Type("boolean"), schema.Type("boolean").WithNull())
	if !s.HasNull() {
		t.Fatal("precondition: the union does carry a null inside its second arm")
	}
	if got := s.StripNull(); got.HasNull() {
		t.Errorf("StripNull left a null behind: %s; StripNull must guarantee !HasNull", jsonOf(t, got))
	}
}

func TestStripNull_RemovesAWholeNullArm(t *testing.T) {
	obj := schema.Object().WithProperty("v", schema.Type("integer"), true)
	s := schema.OneOf(obj, schema.Type("null"))
	if got := s.StripNull(); got.HasNull() {
		t.Errorf("StripNull left a null arm: %s", jsonOf(t, got))
	}
}

func TestStripNull_LeavesANonNullableUnionUntouched(t *testing.T) {
	s := schema.OneOf(schema.Type("boolean"), schema.Type("string"))
	if got := s.StripNull(); !got.Equal(s) {
		t.Errorf("StripNull altered a union with no null in it: %s", jsonOf(t, got))
	}
}

func TestHasNull_SeesThroughNestedUnions(t *testing.T) {
	// A one-level scan reported false here, which is the dangerous direction: a caller
	// would treat a value that can be null as non-null.
	inner := schema.OneOf(schema.Type("boolean"), schema.Type("boolean").WithNull())
	outer := schema.OneOf(inner, schema.Type("boolean"))
	if !outer.HasNull() {
		t.Errorf("HasNull missed a null nested two unions deep in %s; under-reporting null is unsound", jsonOf(t, outer))
	}
}

func TestHasNull_TerminatesOnASelfReferentialUnion(t *testing.T) {
	// Recursing through variants needs a cycle guard. A recursive type with no null must
	// answer false rather than looping forever.
	node := schema.OneOf(schema.Ref("node"), schema.Type("string"))
	s := node.WithDef("node", node)
	done := make(chan bool, 1)
	go func() { done <- s.HasNull() }()
	select {
	case got := <-done:
		if got {
			t.Error("a recursive union of $ref and string carries no null")
		}
	case <-timeoutAfter():
		t.Fatal("HasNull did not terminate on a self-referential union — the cycle guard is missing")
	}
}

func TestHasNull_FindsNullInsideARecursiveDefinition(t *testing.T) {
	node := schema.OneOf(schema.Ref("node"), schema.Type("null"))
	s := node.WithDef("node", node)
	if !s.HasNull() {
		t.Error("the cycle guard must not cause a real null arm to be missed")
	}
}

// timeoutAfter bounds the recursion guard tests: a missing guard hangs rather than fails,
// and a hung test is much harder to read than a failed one.
func timeoutAfter() <-chan time.Time { return time.After(5 * time.Second) }

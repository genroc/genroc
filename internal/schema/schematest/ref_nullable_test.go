package schematest

import (
	"testing"
	"time"

	"genroc/internal/schema"
)

// A `$ref` pointing AT a nullable puts the null INSIDE the target, where StripNull cannot see
// it — refs ride through untouched on purpose, because leaving them symbolic is what keeps
// recursive types finite. So `HasNull` says true and `StripNull` is a no-op, and any caller
// that NARROWS rather than describes must materialize. Every guard on a whole task output hits
// this, since an output is carried as a ref. specs/guard-narrowing.md.
const refToNullable = `{
 "$defs": {"N": {"oneOf": [{"type":"null"}, {"type":"integer"}]}},
 "properties": {"x": {"$ref": "#/$defs/N"}, "y": {"type": ["integer","null"]}},
 "required": ["x","y"]
}`

func TestNarrowingThroughARefToNullable(t *testing.T) {
	ctx := mustSchema(t, refToNullable)
	at, _ := ctx.At("x")
	t.Logf("At(x).HasNull=%v  StripNull=%s", at.HasNull(), jsonOf(t, at.StripNull()))

	// The control: an inline nullable narrows.
	if _, err := ctx.Infer(`y != null && y > 2`); err != nil {
		t.Errorf("inline nullable should narrow: %v", err)
	}
	// The question: does a ref-to-nullable narrow the same way?
	if _, err := ctx.Infer(`x != null && x > 2`); err != nil {
		t.Errorf("ref-to-nullable did NOT narrow: %v", err)
	}
	if _, err := ctx.Infer(`x == null ? 0 : x + 1`); err != nil {
		t.Errorf("ternary over a ref-to-nullable did NOT narrow: %v", err)
	}
}

// Everything that reads THROUGH a nullable needs the null actually gone, not just the ones
// that narrow. A ref pointing at a nullable object described itself as `unknown` and offered
// no members — the same symptom `Summary` was written to cure for `anyOf[$ref, null]`, one
// level deeper.
func TestReadingThroughARefToNullable(t *testing.T) {
	ctx := mustSchema(t, `{
	 "$defs": {"N": {"oneOf": [{"type":"null"}, {"type":"object","properties":{"a":{"type":"string"}},"required":["a"]}]}},
	 "properties": {"x": {"$ref": "#/$defs/N"}}, "required": ["x"]}`)
	at, err := ctx.At("x")
	if err != nil {
		t.Fatalf("At(x): %v", err)
	}

	if n := len(at.StripNull().Properties()); n != 1 {
		t.Errorf("members through a ref-to-nullable = %d, want 1 — completion offers these", n)
	}
	// `Summary` needs no materializing call of its own: it resolves before it reaches the
	// strip. Asserted so a reordering there does not quietly reintroduce `unknown`.
	if got := at.Summary(); got != "object{a}|null" {
		t.Errorf("Summary = %q, want %q", got, "object{a}|null")
	}
}

// A value that is exactly null must describe itself. Stripping the null leaves the empty
// node, which reads as `unknown` — so `null` used to summarise as `unknown|null`, which is
// what a hover showed wherever a guard proved a value null.
func TestSummaryOfExactlyNull(t *testing.T) {
	if got := schema.Type("null").Summary(); got != "null" {
		t.Errorf("Summary of an exactly-null schema = %q, want %q", got, "null")
	}
}

// `deref` follows a CHAIN, so a ref pointing at a ref pointing at a nullable materializes in
// one step. Reachable from a definition: a task whose `output` is another task's re-exports it,
// and `b_output` becomes a `$ref` to `a_output`, which is where the null is declared.
func TestNarrowingThroughARefChain(t *testing.T) {
	ctx := mustSchema(t, `{
	 "$defs": {"A": {"$ref": "#/$defs/B"},
	           "B": {"oneOf": [{"type":"null"}, {"type":"integer"}]}},
	 "properties": {"x": {"$ref": "#/$defs/A"}}, "required": ["x"]}`)
	if _, err := ctx.Infer(`x != null && x > 2`); err != nil {
		t.Errorf("a chain of refs did NOT narrow: %v", err)
	}
	at, _ := ctx.At("x")
	if got := at.Summary(); got != "integer|null" {
		t.Errorf("Summary through a chain = %q, want %q", got, "integer|null")
	}
}

// A RECURSIVE nullable is the shape refs are kept symbolic for, so materializing one must
// resolve a single level and stop. Narrowing two links deep is the real test: each `!= null`
// materializes the node it guards, and nothing inlines the type into itself.
func TestNarrowingThroughARecursiveNullable(t *testing.T) {
	ctx := mustSchema(t, `{
	 "$defs": {"T": {"type":"object",
	                 "properties": {"v": {"type":"integer"},
	                                "next": {"oneOf": [{"type":"null"}, {"$ref":"#/$defs/T"}]}},
	                 "required": ["v","next"]}},
	 "properties": {"root": {"$ref": "#/$defs/T"}}, "required": ["root"]}`)
	if _, err := ctx.Infer(`root.next != null && root.next.v > 1`); err != nil {
		t.Errorf("one link deep did NOT narrow: %v", err)
	}
	if _, err := ctx.Infer(`root.next != null && root.next.next != null && root.next.next.v > 1`); err != nil {
		t.Errorf("two links deep did NOT narrow: %v", err)
	}
	at, _ := ctx.At("root.next")
	if got := at.Summary(); got != "object{next, v}|null" {
		t.Errorf("Summary of a recursive nullable = %q, want %q", got, "object{next, v}|null")
	}
}

// A null one level deeper still: inside a `$ref` that is an ARM of a union, where no wrapper can
// strip it. `outputs.a ?? outputs.b` over two nullable outputs builds exactly this — so the
// strip follows references until it reaches a type rather than resolving a fixed number of hops.
func TestUnionArmHoldingARefToNullable(t *testing.T) {
	ctx := mustSchema(t, `{
	 "$defs": {"A": {"anyOf": [{"$ref": "#/$defs/B"}, {"type":"integer"}]},
	           "B": {"type": ["integer","null"]}},
	 "properties": {"x": {"$ref": "#/$defs/A"}}, "required": ["x"]}`)
	at, err := ctx.At("x")
	if err != nil {
		t.Fatalf("At(x): %v", err)
	}

	// The union's ROOT must carry the pool, or nothing can deref the arm: `HasNull` answered
	// false about a value that is plainly nullable, and every reader below it inherited that.
	if !at.HasNull() {
		t.Error("a union with a ref-to-nullable arm reported itself non-null")
	}
	// StripNull cannot reach into the arm, so the strip makes NO progress — a summary that
	// recursed on it appended `|null` once per level down to the depth bound.
	if got := at.Summary(); got != "integer|null" {
		t.Errorf("Summary = %q, want %q", got, "integer|null")
	}
	// And the guard reaches it: the arm is resolved because it is where the null is, and the
	// integer arm beside it is left alone.
	if _, err := ctx.Infer(`x != null && x + 1 > 2`); err != nil {
		t.Errorf("a null inside a union arm did NOT narrow: %v", err)
	}
}

// Following references has to stop somewhere, and the bound is the CYCLE rather than a hop
// count: A holds B, B holds A or null, and every link is visited once on the way down.
//
// `CheckDoc` refuses this shape — a `$defs` cycle with no structural progress — so nothing a
// definition can register reaches it, and what it RENDERS as is deliberately not asserted. The
// claim is termination: a guard removed here costs a hung language server rather than a wrong
// answer, and nothing else in the suite would notice.
func TestStripNullThroughAReferenceCycle(t *testing.T) {
	const cyclic = `{
	 "$defs": {"A": {"anyOf": [{"$ref": "#/$defs/B"}, {"type":"integer"}]},
	           "B": {"anyOf": [{"$ref": "#/$defs/A"}, {"type":"null"}]}},
	 "properties": {"x": {"$ref": "#/$defs/A"}}, "required": ["x"]}`
	ctx := mustSchema(t, cyclic)
	if err := ctx.CheckDoc(); err == nil {
		t.Error("the cycle is registrable now, so its rendering IS a contract — assert one")
	}
	at, err := ctx.At("x")
	if err != nil {
		t.Fatalf("At(x): %v", err)
	}
	done := make(chan string, 1)
	go func() { done <- at.StripNull().Summary() }()
	select {
	case got := <-done:
		t.Logf("cycle strips to %q", got)
	case <-time.After(5 * time.Second):
		t.Fatal("the walk did not terminate: the cycle guard is gone")
	}
}

// The other cycle shape, and the one a definition actually produces: a recursive OBJECT whose
// nullable link points back at itself. Materializing the link must resolve it once and leave
// the refs inside symbolic — inlining is what stopped the output solver converging.
func TestStripNullThroughARecursiveObject(t *testing.T) {
	ctx := mustSchema(t, `{
	 "$defs": {"T": {"type":"object",
	                 "properties": {"v": {"type":"integer"},
	                                "next": {"oneOf": [{"type":"null"}, {"$ref":"#/$defs/T"}]}},
	                 "required": ["v","next"]}},
	 "properties": {"root": {"$ref": "#/$defs/T"}}, "required": ["root"]}`)
	at, err := ctx.At("root.next")
	if err != nil {
		t.Fatalf("At(root.next): %v", err)
	}
	stripped := at.StripNull()
	if stripped.HasNull() {
		t.Error("the nullable link did not materialize")
	}
	// Symbolic: the link resolves to the recursive definition, and its own `next` is still a
	// ref. A size check is the honest test — an inlining walk grows without bound.
	if n := len(jsonOf(t, stripped)); n > 1000 {
		t.Errorf("stripped form is %d bytes: the recursion is being inlined", n)
	}
}

package schematest

import "testing"

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

	if n := len(at.StripNullMaterialized().Properties()); n != 1 {
		t.Errorf("members through a ref-to-nullable = %d, want 1 — completion offers these", n)
	}
	// `Summary` needs no materializing call of its own: it resolves before it reaches the
	// strip. Asserted so a reordering there does not quietly reintroduce `unknown`.
	if got := at.Summary(); got != "object{a}|null" {
		t.Errorf("Summary = %q, want %q", got, "object{a}|null")
	}
}

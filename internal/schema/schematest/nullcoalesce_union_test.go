package schematest

import (
	"encoding/json"
	"testing"

	"genroc/internal/schema"
)

// `a ?? b` builds a union when the two sides do not reduce to one type. That union is
// canonicalized, and these tests pin down why it has to be: oneOf means EXACTLY one, so a
// union whose arms overlap describes a type that NO value satisfies. Before
// canonicalization `boolean ?? boolean|null` produced oneOf[{boolean},{boolean|null}],
// which rejected both `true` and `null` — every value it was supposed to describe.

// coalesceCtx is a context with two independently-optional properties of the given type,
// mirroring the shape two optional task outputs take at the process-output boundary.
func coalesceCtx(t string) schema.Schema {
	return schema.Object().
		WithProperty("a", schema.Type(t), false).
		WithProperty("b", schema.Type(t), false)
}

func inferIn(t *testing.T, ctx schema.Schema, expr string) schema.Schema {
	t.Helper()
	got, err := ctx.Infer(expr)
	if err != nil {
		t.Fatalf("Infer(%q): %v", expr, err)
	}
	return got
}

func jsonOf(t *testing.T, s schema.Schema) string {
	t.Helper()
	b, err := json.Marshal(s)
	if err != nil {
		t.Fatalf("marshal schema: %v", err)
	}
	return string(b)
}

func TestNullCoalesce_TwoOptionalsMergeIntoOneNullableType(t *testing.T) {
	got := inferIn(t, coalesceCtx("boolean"), "a ?? b")
	assertJSONEq(t, got, `{"type":["boolean","null"]}`,
		"an un-merged oneOf[{boolean},{boolean|null}] overlaps on `true`, and oneOf requires "+
			"exactly one match — so the un-merged form describes a type no value satisfies")
}

func TestNullCoalesce_MergedUnionAcceptsEveryValueItDescribes(t *testing.T) {
	// The property the shape assertion above exists to protect. Inferring a type and then
	// rejecting its own inhabitants is the actual defect; the JSON shape is just evidence.
	got := inferIn(t, coalesceCtx("boolean"), "a ?? b")
	for _, v := range []any{true, false, nil} {
		if _, err := got.Validate(v); err != nil {
			t.Errorf("inferred type %s rejects %v, a value it must describe: %v", jsonOf(t, got), v, err)
		}
	}
}

func TestNullCoalesce_DistinctTypesMergeToATypeArray(t *testing.T) {
	ctx := schema.Object().
		WithProperty("count", schema.Type("integer"), false).
		WithProperty("label", schema.Type("string"), false)
	got := inferIn(t, ctx, "count ?? label")
	assertJSONEq(t, got, `{"type":["integer","null","string"]}`,
		"distinct scalar arms must still merge into one type array; two separate oneOf arms "+
			"cannot both be 'exactly one' for a value that matches either")
}

func TestNullCoalesce_ChainedDefaultRecoversNonNull(t *testing.T) {
	// The authoring idiom that used to be impossible: once the left had become a raw
	// union, no number of trailing defaults could strip its null, because stripNull only
	// dropped whole {"type":"null"} arms and never looked inside one.
	got := inferIn(t, coalesceCtx("boolean"), "a ?? b ?? false")
	assertJSONEq(t, got, `{"type":"boolean"}`,
		"a non-null literal at the end of a ?? chain must make the whole chain non-null")
}

func TestNullCoalesce_SingleDefaultStillNonNull(t *testing.T) {
	got := inferIn(t, coalesceCtx("boolean"), "a ?? false")
	assertJSONEq(t, got, `{"type":"boolean"}`,
		"the one-operand case already worked and must not regress")
}

func TestNullCoalesce_NullableLeftAloneStaysNullable(t *testing.T) {
	got := inferIn(t, coalesceCtx("boolean"), "a")
	assertJSONEq(t, got, `{"type":["boolean","null"]}`,
		"?? is what removes the null; merely reading an optional property must not")
}

func TestNullCoalesce_NonNullLeftIsANoOp(t *testing.T) {
	ctx := schema.Object().WithProperty("a", schema.Type("string"), true)
	got := inferIn(t, ctx, `a ?? "fallback"`)
	assertJSONEq(t, got, `{"type":"string"}`,
		"?? on a value that can never be null must return the left type unchanged")
}

func TestNullCoalesce_ObjectArmsDoNotMergeIntoATypeArray(t *testing.T) {
	// Only primitive arms fold into a type array. Objects keep the union, and the null
	// arm is dropped by stripNull rather than merged — a different route to the same
	// non-overlapping result.
	obj := schema.Object().WithProperty("v", schema.Type("integer"), true)
	ctx := schema.Object().
		WithProperty("a", obj, false).
		WithProperty("b", obj, false)
	got := inferIn(t, ctx, "a ?? b")
	for _, v := range []any{map[string]any{"v": 1}, nil} {
		if _, err := got.Validate(v); err != nil {
			t.Errorf("inferred object union %s rejects %v: %v", jsonOf(t, got), v, err)
		}
	}
}

func TestNullCoalesce_EmptyArrayDefaultStillAbsorbs(t *testing.T) {
	// absorbEmptyArray predates this change and must survive it: `xs ?? []` has to keep
	// the informative arm, or child_list's `over` loses its element type.
	ctx := schema.Object().
		WithProperty("xs", schema.Array(schema.Type("string")), false)
	got := inferIn(t, ctx, "xs ?? []")
	if got.Items().IsZero() {
		t.Fatalf("`xs ?? []` lost its element type (%s); child_list's `over` reads Items() directly", jsonOf(t, got))
	}
}

func TestNullCoalesce_NumericWideningStillApplies(t *testing.T) {
	ctx := schema.Object().WithProperty("n", schema.Type("integer"), false)
	assertJSONEq(t, inferIn(t, ctx, "n ?? 0.5"), `{"type":"number"}`,
		"integer ?? number must widen to number so arithmetic on the result works")
	assertJSONEq(t, inferIn(t, ctx, "n ?? 0"), `{"type":"integer"}`,
		"integer ?? integer must stay integer")
}

// assertJSONEq compares a schema against expected JSON, reporting why the shape matters.
func assertJSONEq(t *testing.T, got schema.Schema, wantJSON, why string) {
	t.Helper()
	var gotAny, wantAny any
	if err := json.Unmarshal([]byte(jsonOf(t, got)), &gotAny); err != nil {
		t.Fatalf("unmarshal got: %v", err)
	}
	if err := json.Unmarshal([]byte(wantJSON), &wantAny); err != nil {
		t.Fatalf("wantJSON invalid: %v", err)
	}
	ga, _ := json.Marshal(gotAny)
	wa, _ := json.Marshal(wantAny)
	if string(ga) != string(wa) {
		t.Errorf("schema mismatch\n got:  %s\n want: %s\n why:  %s", ga, wa, why)
	}
}

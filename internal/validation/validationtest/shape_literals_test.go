package validationtest

import "testing"

// End to end through Generate: an array joins its element types, a whole number types as
// integer, null as null.

func TestGenerate_ShapeArrayLiteral_ProcessOutput(t *testing.T) {
	input := `{"type":"object","properties":{"a":{"type":"integer"},"b":{"type":"integer"}},"required":["a","b"]}`
	out := runGenerate(t, mapProcessOutputDef("shape-array-out", input,
		`["$: input.a", "$: input.b", 7]`))
	assertJSON(t, out.ProcessOutput, `{"$ref": "#/$defs/output"}`)
	assertJSON(t, defOf(out, "output"), `{"type":"array","items":{"type":"integer"}}`)
}

func TestGenerate_ShapeArrayLiteral_JoinsElementTypes(t *testing.T) {
	input := `{"type":"object","properties":{"a":{"type":"string"}},"required":["a"]}`
	out := runGenerate(t, mapProcessOutputDef("shape-array-join", input,
		`[1, "$: input.a"]`))
	// integer joined with string canonicalizes to a multi-type item schema.
	assertJSON(t, defOf(out, "output"),
		`{"type":"array","items":{"type":["integer","string"]}}`)
}

func TestGenerate_ShapeScalarsAndNested(t *testing.T) {
	input := `{"type":"object","properties":{"a":{"type":"string"}},"required":["a"]}`
	out := runGenerate(t, mapProcessOutputDef("shape-scalars", input,
		`{"n": 5, "pi": 3.5, "flag": true, "nothing": null, "tags": ["$: input.a"]}`))
	o := defOf(out, "output")
	assertJSON(t, o.Properties()["n"], `{"type":"integer"}`)
	assertJSON(t, o.Properties()["pi"], `{"type":"number"}`)
	assertJSON(t, o.Properties()["flag"], `{"type":"boolean"}`)
	assertJSON(t, o.Properties()["nothing"], `{"type":"null"}`)
	assertJSON(t, o.Properties()["tags"], `{"type":"array","items":{"type":"string"}}`)
}

func TestGenerate_ExprMarkerLeaf_PreservesTypeThroughShape(t *testing.T) {
	input := `{"type":"object","properties":{"list":{"type":"array","items":{"type":"string"}}},"required":["list"]}`
	out := runGenerate(t, mapProcessOutputDef("expr-marker-leaf", input, `{"tags": "$: input.list"}`))
	// The $: leaf preserves the array type through inferShape (a stringifying template
	// would not carry it), proving the marker flows through the Shape pipeline.
	assertJSON(t, defOf(out, "output").Properties()["tags"], `{"type":"array","items":{"type":"string"}}`)
}

func TestGenerate_ShapeEmptyArrayLiteral(t *testing.T) {
	input := `{"type":"object","properties":{"a":{"type":"string"}},"required":["a"]}`
	out := runGenerate(t, mapProcessOutputDef("shape-empty-array", input, `{"tags": []}`))
	// A literal [] is the provably-empty array (maxItems 0), a subset of any array<T>.
	assertJSON(t, defOf(out, "output").Properties()["tags"], `{"type":"array","maxItems":0}`)
}

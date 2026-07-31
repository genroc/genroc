package expressiontest

import "testing"

// Edge cases for computed keys: the container shapes a[expr] can land on, the
// shapes the key expression itself can take, and the scoping of the guards that
// narrowing installs.

const computedKeyEdgeJSON = `{
	"type": "object",
	"properties": {
		"maybeMap": {
			"anyOf": [
				{ "type": "object", "additionalProperties": { "type": "string" } },
				{ "type": "null" }
			]
		},
		"nullableTypeForm": {
			"type": ["object", "null"],
			"additionalProperties": { "type": "string" }
		},
		"mapOfMaps": {
			"type": "object",
			"additionalProperties": { "type": "object", "additionalProperties": { "type": "integer" } }
		},
		"grid": { "type": "array", "items": { "type": "array", "items": { "type": "integer" } } },
		"counts": { "type": "object", "additionalProperties": { "type": "integer" } },
		"nums": { "type": "array", "items": { "type": "integer" } },
		"openValues": { "type": "object", "additionalProperties": {} },
		"unionBase": {
			"anyOf": [
				{ "type": "object", "additionalProperties": { "type": "string" } },
				{ "type": "array", "items": { "type": "string" } }
			]
		},
		"emptyMap": { "type": "object" },
		"rows": {
			"type": "array",
			"items": { "type": "object", "additionalProperties": { "type": "string", "secret": true } }
		},
		"plainRows": {
			"type": "array",
			"items": { "type": "object", "additionalProperties": { "type": "string" } }
		},
		"intRows": {
			"type": "array",
			"items": { "type": "object", "additionalProperties": { "type": "integer" } }
		},
		"secretItems": { "type": "array", "items": { "type": "string", "secret": true } },
		"xs": { "type": "array", "items": { "type": "string" } },
		"o": { "type": "object", "properties": { "id": { "type": "string" } }, "required": ["id"] },
		"k": { "type": "string" },
		"i": { "type": "integer" }
	},
	"required": ["maybeMap","nullableTypeForm","mapOfMaps","grid","counts","nums","openValues","unionBase","emptyMap","rows","plainRows","intRows","secretItems","xs","o","k","i"]
}`

// A base that may be null still reads: access on null is null at runtime, and both
// nullable spellings (anyOf-with-null and the type-array form) behave alike.
func TestComputedKeyEdge_NullableBase(t *testing.T) {
	c := ctx(t, computedKeyEdgeJSON)
	assertSchema(t, infer(t, `maybeMap[k]`, c), `{"type": ["string", "null"]}`)
	assertSchema(t, infer(t, `nullableTypeForm[k]`, c), `{"type": ["string", "null"]}`)
}

// Containers nest, and the nullable value of the outer read is the base of the
// inner one.
func TestComputedKeyEdge_Nested(t *testing.T) {
	c := ctx(t, computedKeyEdgeJSON)
	assertSchema(t, infer(t, `mapOfMaps[k][k]`, c), `{"type": ["integer", "null"]}`)
	assertSchema(t, infer(t, `grid[i][i]`, c), `{"type": ["integer", "null"]}`)
	assertSchema(t, infer(t, `mapOfMaps[k][o.id] ?? 0`, c), `{"type": "integer"}`)
}

// An open map with no value schema hands back the top type, and reading through it
// is rejected the same way any unknown is.
func TestComputedKeyEdge_OpenValueSchema(t *testing.T) {
	c := ctx(t, computedKeyEdgeJSON)
	assertSchema(t, infer(t, `openValues[k]`, c), `{}`)
	inferErr(t, `openValues[k].deeper`, c, "the value is unknown")
}

// Shapes with no single key type. A union of a map and an array has no one answer
// even though each arm would; an object with neither properties nor
// additionalProperties has nothing to read at all.
func TestComputedKeyEdge_UntypeableBases(t *testing.T) {
	c := ctx(t, computedKeyEdgeJSON)
	inferErr(t, `unionBase[k]`, c, "not an array or a map")
	inferErr(t, `emptyMap[k]`, c, "not an array or a map")
	inferErr(t, `i[k]`, c, "not an array or a map")
	inferErr(t, `k[i]`, c, "not an array or a map")
}

// The key is an ordinary expression, so anything of the right type works —
// including a path, a nested read, and a lambda's parameters.
func TestComputedKeyEdge_KeyExpressionShapes(t *testing.T) {
	c := ctx(t, computedKeyEdgeJSON)
	assertSchema(t, infer(t, `counts[o.id]`, c), `{"type": ["integer", "null"]}`)
	assertSchema(t, infer(t, `counts[xs[0] ?? ""]`, c), `{"type": ["integer", "null"]}`)
	assertSchema(t, infer(t, `counts[k + "-total"]`, c), `{"type": ["integer", "null"]}`)
	assertSchema(t, infer(t, `map(xs, x => counts[x])`, c),
		`{"type": "array", "items": {"type": ["integer", "null"]}}`)
	assertSchema(t, infer(t, `map(xs, (v, n) => nums[n])`, c),
		`{"type": "array", "items": {"type": ["integer", "null"]}}`)
}

// ─── guards ────────────────────────────────────────────────────────────────────

// A guard keys on the whole access path, so it works for any key shape that is
// itself a path — not just a bare identifier.
func TestComputedKeyEdge_GuardsOnKeyShapes(t *testing.T) {
	c := ctx(t, computedKeyEdgeJSON)
	assertSchema(t, infer(t, `counts[o.id] != null ? counts[o.id] + 1 : 0`, c), `{"type": "integer"}`)
	assertSchema(t, infer(t, `mapOfMaps[k][o.id] != null ? mapOfMaps[k][o.id] + 1 : 0`, c), `{"type": "integer"}`)
	assertSchema(t, infer(t, `grid[i][i] != null ? grid[i][i] + 1 : 0`, c), `{"type": "integer"}`)
}

// A guard on a[k] is not a guard on a, and vice versa — they are different paths.
func TestComputedKeyEdge_GuardDoesNotSpreadToTheContainer(t *testing.T) {
	c := ctx(t, computedKeyEdgeJSON)
	// Narrowing the value says nothing about a sibling key.
	inferErr(t, `counts[k] != null ? counts[o.id] + 1 : 0`, c, "")
	// Narrowing the nested map does not narrow a read through it.
	inferErr(t, `mapOfMaps[k] != null ? mapOfMaps[k][k] + 1 : 0`, c, "")
}

// A lambda that does not touch the key leaves the guard standing — dropping every
// guard on entry would be safe but would make narrowing useless inside map().
func TestComputedKeyEdge_GuardSurvivesAnUnrelatedLambda(t *testing.T) {
	c := ctx(t, computedKeyEdgeJSON)
	assertSchema(t, infer(t, `counts[k] != null ? map(xs, v => counts[k] + 1) : []`, c),
		`{"type": "array", "items": {"type": "integer"}}`)
}

// …and the index parameter shadows exactly like the value parameter does. This is
// the `lam.IndexParam` half of the drop, which the value-parameter test does not
// reach.
func TestComputedKeyEdge_GuardDroppedByTheIndexParameter(t *testing.T) {
	c := ctx(t, computedKeyEdgeJSON)
	// i is rebound as the lambda's index parameter, so nums[i] is a different read.
	inferErr(t, `nums[i] != null ? map(xs, (v, i) => nums[i] + 1) : []`, c, "")
	// The same lambda is fine when it does not rebind i.
	assertSchema(t, infer(t, `nums[i] != null ? map(xs, (v, n) => nums[i] + 1) : []`, c),
		`{"type": "array", "items": {"type": "integer"}}`)
}

// A guard whose key is shadowed dies even when the container is not, and the
// reverse: shadowing the container drops it too.
func TestComputedKeyEdge_GuardDroppedByEitherRoot(t *testing.T) {
	c := ctx(t, computedKeyEdgeJSON)
	inferErr(t, `counts[k] != null ? map(xs, k => counts[k] + 1) : []`, c, "")
	// The container shadowed instead. intRows' element is a map with the same value
	// type, so the read still types — nullability is the only difference, which is
	// exactly what a stale guard would erase.
	inferErr(t, `counts[k] != null ? map(intRows, counts => counts[k] + 1) : []`, c, "")
	assertSchema(t, infer(t, `map(intRows, counts => (counts[k] ?? 0) + 1)`, c),
		`{"type": "array", "items": {"type": "integer"}}`)
}

// ─── secrets ───────────────────────────────────────────────────────────────────

// The taint may sit on an array's items rather than a map's values, and it may sit
// on the element type of a container the expression only reaches through a lambda
// parameter — a path from the root never names it.
func TestComputedKeyEdge_SecretPositions(t *testing.T) {
	c := ctx(t, computedKeyEdgeJSON)
	secretRefAll(t, c, true, []secretCase{
		{"array_items_computed", `secretItems[i]`},
		{"array_items_literal", `secretItems[0]`},
		{"element_map_via_lambda", `map(rows, r => r[k])`},
		{"element_map_key_from_param", `map(rows, r => r[k] ?? "")`},
	})
	secretRefAll(t, c, false, []secretCase{
		{"plain_element_map_via_lambda", `map(plainRows, r => r[k])`},
		{"plain_nested_map", `map(xs, x => mapOfMaps[x])`},
	})
}

// ─── evaluation ────────────────────────────────────────────────────────────────

func TestComputedKeyEdge_EvalContainerShapes(t *testing.T) {
	context := map[string]any{
		"m":      map[string]any{"a": "1", "": "empty-key", "x.y": "dotted"},
		"xs":     []any{"first", "second"},
		"nested": map[string]any{"a": map[string]any{"b": "deep"}},
		"k":      "a",
		"empty":  "",
		"dotted": "x.y",
		"str":    "not a container",
	}
	assertEq(t, evalOK(t, `m[k]`, context), "1")
	assertEq(t, evalOK(t, `m[empty]`, context), "empty-key")
	assertEq(t, evalOK(t, `m[dotted]`, context), "dotted")
	assertEq(t, evalOK(t, `nested[k][k + "b"]`, context), nil) // "ab" is absent
	assertEq(t, evalOK(t, `nested[k]["b"]`, context), "deep")
	// A non-container base is null, not an error — same as literal access.
	assertEq(t, evalOK(t, `str[k]`, context), nil)
	// A computed key built from a literal is the same read as the literal form.
	assertEq(t, evalOK(t, `m["a"]`, context), evalOK(t, `m[k]`, context))
}

func TestComputedKeyEdge_EvalIndexBoundaries(t *testing.T) {
	context := map[string]any{
		"xs":    []any{"a", "b", "c"},
		"zero":  0,
		"last":  2,
		"past":  3,
		"neg":   -1,
		"big":   "99999999999999999999999999",
		"float": 1.5,
	}
	assertEq(t, evalOK(t, `xs[zero]`, context), "a")
	assertEq(t, evalOK(t, `xs[last]`, context), "c")
	assertEq(t, evalOK(t, `xs[past]`, context), nil)
	assertEq(t, evalOK(t, `xs[neg]`, context), nil)
	// An index too large to be an int64 is out of range, not a crash.
	assertEq(t, evalOK(t, `xs[0 + 99999999999999999999999999]`, context), nil)
	// A non-integral number is a key-type error, like a string would be.
	if err := evalErr(t, `xs[float]`, context); err == nil {
		t.Error("expected an error for a fractional index")
	}
}

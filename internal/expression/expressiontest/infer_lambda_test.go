package expressiontest

import (
	"testing"
)

// Shaped like the context the engine builds (input / outputs.<id> / self.result). `rows` items
// are a $ref because that is likeliest to break — map reads Items() without expanding. The
// `maybe` / `input.opt` name collisions drive the shadowing tests.
const lambdaCtxJSON = `{
	"type": "object",
	"properties": {
		"input": {
			"type": "object",
			"properties": {
				"rows":    { "type": "array", "items": { "$ref": "#/$defs/row" } },
				"tags":    { "type": "array", "items": { "type": "string" } },
				"counts":  { "type": "array", "items": { "type": "integer" } },
				"matrix":  { "type": "array", "items": { "type": "array", "items": { "type": "number" } } },
				"bare":    { "type": "array" },
				"optRows": { "type": ["array", "null"], "items": { "$ref": "#/$defs/row" } },
				"label":   { "type": "string" },
				"flag":    { "type": "boolean" },
				"cfg":     { "type": "object", "properties": { "n": { "type": "integer" } }, "required": ["n"] },
				"opt":     { "type": ["string", "null"] }
			},
			"required": ["rows", "tags", "counts", "matrix", "bare", "optRows", "label", "flag", "cfg", "opt"]
		},
		"outputs": {
			"type": "object",
			"properties": {
				"fetch": {
					"type": "object",
					"properties": { "rows": { "type": "array", "items": { "$ref": "#/$defs/row" } } },
					"required": ["rows"]
				}
			},
			"required": ["fetch"]
		},
		"self": {
			"type": "object",
			"properties": {
				"result": {
					"type": "object",
					"properties": { "items": { "type": "array", "items": { "type": "string" } } },
					"required": ["items"]
				}
			},
			"required": ["result"]
		},
		"maybe": { "type": ["string", "null"] }
	},
	"required": ["input", "outputs", "self", "maybe"],
	"$defs": {
		"row": {
			"type": "object",
			"properties": {
				"name":  { "type": "string" },
				"score": { "type": ["number", "null"] },
				"n":     { "type": "integer" },
				"opt":   { "type": "integer" },
				"token": { "type": "string", "secret": true }
			},
			"required": ["name", "score", "n", "opt", "token"]
		}
	}
}`

// ─── Element typing ─────────────────────────────────────────────────────────────

func TestMapLambda_OverArrayOfArrays(t *testing.T) {
	c := ctx(t, lambdaCtxJSON)
	assertSchema(t, infer(t, `map(input.matrix, row => row)`, c), `{
		"type": "array",
		"items": {"type": "array", "items": {"type": "number"}}
	}`)
}

// The inner map's source is a lambda parameter, not a context path — so
// mapElement has to infer an operand that only exists in `vars`.
func TestMapLambda_NestedMapOverInnerArray(t *testing.T) {
	c := ctx(t, lambdaCtxJSON)
	assertSchema(t, infer(t, `map(input.matrix, row => map(row, n => n * 2))`, c), `{
		"type": "array",
		"items": {"type": "array", "items": {"type": "number"}}
	}`)
}

// Indexing the parameter inside the body still carries out-of-bounds
// nullability — map's non-nullable-element rule applies to the parameter, not to
// everything reached from it.
func TestMapLambda_IndexIntoParameterIsNullable(t *testing.T) {
	c := ctx(t, lambdaCtxJSON)
	assertSchema(t, infer(t, `map(input.matrix, row => row[0])`, c), `{
		"type": "array",
		"items": {"type": ["number", "null"]}
	}`)
}

// A map result is an ordinary array: indexing it is nullable.
func TestMapLambda_ResultIndexIsNullable(t *testing.T) {
	c := ctx(t, lambdaCtxJSON)
	assertSchema(t, infer(t, `map(input.tags, t => t)[0]`, c), `{"type":["string","null"]}`)
}

// ...and a field read off that index inherits the nullability.
func TestMapLambda_ResultIndexFieldIsNullable(t *testing.T) {
	c := ctx(t, lambdaCtxJSON)
	assertSchema(t, infer(t, `map(input.rows, r => {v: r.n})[0].v`, c), `{"type":["integer","null"]}`)
}

// Member access straight on a map result must fail — arrays have no properties,
// and silently yielding null here would hide a typo like `.length`.
func TestMapLambda_ResultMemberAccessFails(t *testing.T) {
	c := ctx(t, lambdaCtxJSON)
	inferErr(t, `map(input.tags, t => t).length`, c, "cannot access .length: schema has no properties")
}

// A map result is a legal map source: it declares items, so elementOf can read it.
func TestMapLambda_ResultAsSource(t *testing.T) {
	c := ctx(t, lambdaCtxJSON)
	assertSchema(t, infer(t, `map(map(input.rows, r => r.n), n => n + 1)`, c), `{
		"type": "array",
		"items": {"type": "integer"}
	}`)
}

func TestMapLambda_SourceFromOutputs(t *testing.T) {
	c := ctx(t, lambdaCtxJSON)
	assertSchema(t, infer(t, `map(outputs.fetch.rows, r => {id: r.name, next: r.n + 1})`, c), `{
		"type": "array",
		"items": {
			"type": "object",
			"properties": {
				"id":   {"type": "string"},
				"next": {"type": "integer"}
			},
			"required": ["id", "next"]
		}
	}`)
}

func TestMapLambda_SourceFromSelfResult(t *testing.T) {
	c := ctx(t, lambdaCtxJSON)
	assertSchema(t, infer(t, `map(self.result.items, s => s + "!")`, c), `{
		"type": "array",
		"items": {"type": "string"}
	}`)
}

// ─── $ref elements and $ref sources ─────────────────────────────────────────────

// The element type is read through the ref, so a field of the referenced
// definition types exactly as if the items had been declared inline.
func TestMapLambda_RefItemsElementField(t *testing.T) {
	c := ctx(t, lambdaCtxJSON)
	assertSchema(t, infer(t, `map(input.rows, r => r.name)`, c), `{
		"type": "array",
		"items": {"type": "string"}
	}`)
}

// Passing the element through unchanged must keep the `$ref` symbolic rather
// than inlining the definition — that is what keeps a recursive output type
// finite, and `items` is the position the productivity rule counts.
func TestMapLambda_RefElementStaysSymbolic(t *testing.T) {
	c := ctx(t, lambdaCtxJSON)
	assertSchema(t, infer(t, `map(input.rows, r => r)`, c), `{
		"type": "array",
		"items": {"$ref": "#/$defs/row"}
	}`)
}

// A map result must stay navigable: the $ref under items is only useful if the array still
// carries the root $defs handle — drop the defs anywhere in Array(...).WithDefs and this fails
// to resolve. [0] is nullable (index may be out of bounds), the element is not.
func TestMapLambda_RefElementIndexResolvesIntegerField(t *testing.T) {
	c := ctx(t, lambdaCtxJSON)
	assertSchema(t, infer(t, `map(input.rows, r => r)[0].n`, c), `{"type":["integer","null"]}`)
}

// The same through a string-typed field of the referenced definition.
func TestMapLambda_RefElementIndexResolvesStringField(t *testing.T) {
	c := ctx(t, lambdaCtxJSON)
	assertSchema(t, infer(t, `map(input.rows, r => r)[0].name`, c), `{"type":["string","null"]}`)
}

// refSourceCtxJSON puts the array types themselves behind `$ref`s, which is what
// a hoisted result_schema looks like after MergeInto.
const refSourceCtxJSON = `{
	"type": "object",
	"properties": {
		"input": {
			"type": "object",
			"properties": {
				"refRows":  { "$ref": "#/$defs/rowList" },
				"optList":  { "$ref": "#/$defs/optList" },
				"bareList": { "$ref": "#/$defs/bareList" }
			},
			"required": ["refRows", "optList", "bareList"]
		}
	},
	"required": ["input"],
	"$defs": {
		"rowList":  { "type": "array", "items": { "type": "object", "properties": {"n": {"type": "integer"}}, "required": ["n"] } },
		"optList":  { "type": ["array", "null"], "items": { "type": "string" } },
		"bareList": { "type": "array" }
	}
}`

// The source itself may be a `$ref` to an array definition — map's source
// position is look-inside, so it must resolve the reference before reading
// `items` rather than rejecting a schema whose type lives behind a ref.
func TestMapLambda_SourceBehindRef(t *testing.T) {
	c := ctx(t, refSourceCtxJSON)
	assertSchema(t, infer(t, `map(input.refRows, r => r.n)`, c), `{
		"type": "array",
		"items": {"type": "integer"}
	}`)
}

// Nullability declared inside the referenced definition is caught too — the
// wrapper at the use site says nothing about it.
func TestMapLambda_SourceBehindRefNullableRejected(t *testing.T) {
	c := ctx(t, refSourceCtxJSON)
	inferErr(t, `map(input.optList, s => s)`, c, "may be null")
}

// ...and `?? []` is the fix, exactly as for a nullable array declared inline.
func TestMapLambda_SourceBehindRefNullableCoalesced(t *testing.T) {
	c := ctx(t, refSourceCtxJSON)
	assertSchema(t, infer(t, `map(input.optList ?? [], s => s)`, c), `{
		"type": "array",
		"items": {"type": "string"}
	}`)
}

// ...as is an itemless array behind a ref.
func TestMapLambda_SourceBehindRefItemlessRejected(t *testing.T) {
	c := ctx(t, refSourceCtxJSON)
	inferErr(t, `map(input.bareList, x => x)`, c, "no element type")
}

// ─── Nesting and capture ────────────────────────────────────────────────────────

// Three levels deep with capture from two levels out: each parameter must
// resolve to its own element type, which is exactly what expr-lang's single `#`
// pointer could not express.
func TestMapLambda_ThreeDeepCapture(t *testing.T) {
	c := ctx(t, lambdaCtxJSON)
	expr := `map(input.rows, r => map(input.tags, t => map(input.counts, c => {c: c, name: r.name, tag: t})))`
	assertSchema(t, infer(t, expr, c), `{
		"type": "array",
		"items": {
			"type": "array",
			"items": {
				"type": "array",
				"items": {
					"type": "object",
					"properties": {
						"c":    {"type": "integer"},
						"name": {"type": "string"},
						"tag":  {"type": "string"}
					},
					"required": ["c", "name", "tag"]
				}
			}
		}
	}`)
}

// ─── Shadowing ──────────────────────────────────────────────────────────────────

// Shadowing holds for every context root, not just `input`: the parameter wins
// even when it is named after the roots the engine always injects.
func TestMapLambda_ParamShadowsOutputsRoot(t *testing.T) {
	c := ctx(t, lambdaCtxJSON)
	assertSchema(t, infer(t, `map(input.rows, outputs => outputs.n)`, c), `{
		"type": "array",
		"items": {"type": "integer"}
	}`)
}

// The same for `self`.
func TestMapLambda_ParamShadowsSelfRoot(t *testing.T) {
	c := ctx(t, lambdaCtxJSON)
	assertSchema(t, infer(t, `map(input.rows, self => self.name)`, c), `{
		"type": "array",
		"items": {"type": "string"}
	}`)
}

// Both parameters shadow, including the index parameter.
func TestMapLambda_IndexParamShadowsRootsToo(t *testing.T) {
	c := ctx(t, lambdaCtxJSON)
	assertSchema(t, infer(t, `map(input.counts, (input, outputs) => input + outputs)`, c), `{
		"type": "array",
		"items": {"type": "integer"}
	}`)
}

// The inner parameter wins over an outer one of the same name. If it did not,
// `x + 1` would be string arithmetic and error out.
func TestMapLambda_InnerParamShadowsOuterParam(t *testing.T) {
	c := ctx(t, lambdaCtxJSON)
	assertSchema(t, infer(t, `map(input.tags, x => map(input.counts, x => x + 1))`, c), `{
		"type": "array",
		"items": {"type": "array", "items": {"type": "integer"}}
	}`)
}

// ...and the shadow is scoped to the inner body: the outer binding is intact
// alongside it, since withParams copies rather than mutates.
func TestMapLambda_OuterParamSurvivesInnerShadow(t *testing.T) {
	c := ctx(t, lambdaCtxJSON)
	assertSchema(t, infer(t, `map(input.tags, x => {inner: map(input.counts, x => x + 1), outer: x})`, c), `{
		"type": "array",
		"items": {
			"type": "object",
			"properties": {
				"inner": {"type": "array", "items": {"type": "integer"}},
				"outer": {"type": "string"}
			},
			"required": ["inner", "outer"]
		}
	}`)
}

// ─── Guard dropping in withParams ───────────────────────────────────────────────

// A guard established outside a lambda says nothing about a parameter taking over that name.
// If the stale guard leaked, `maybe.n` would be a property read on a string and the whole
// expression would fail — so both branches must agree on array<integer>.
func TestMapLambda_ShadowedRootGuardDropped(t *testing.T) {
	c := ctx(t, lambdaCtxJSON)
	expr := `maybe != null ? map(input.rows, maybe => maybe.n) : map(input.rows, r => r.n)`
	assertSchema(t, infer(t, expr, c), `{
		"type": "array",
		"items": {"type": "integer"}
	}`)
}

// The same rule for a guard on a SUB-PATH of a shadowed root. Guards are consulted before
// vars, so a guard that was not dropped would silently win and type the result as
// array<string>, diverging from the else branch.
func TestMapLambda_ShadowedRootSubPathGuardDropped(t *testing.T) {
	c := ctx(t, lambdaCtxJSON)
	expr := `input.opt != null ? map(input.rows, input => input.opt) : map(input.rows, r => r.opt)`
	assertSchema(t, infer(t, expr, c), `{
		"type": "array",
		"items": {"type": "integer"}
	}`)
}

// The index parameter shadows too, and withParams drops guards rooted at its
// name on the same grounds. A leaked guard would make `maybe` a string here and
// the arithmetic would be rejected.
func TestMapLambda_ShadowedGuardDroppedByIndexParam(t *testing.T) {
	c := ctx(t, lambdaCtxJSON)
	expr := `maybe != null ? map(input.tags, (t, maybe) => maybe + 1) : map(input.counts, n => n)`
	assertSchema(t, infer(t, expr, c), `{
		"type": "array",
		"items": {"type": "integer"}
	}`)
}

// A guard rooted at a lambda parameter is dropped by a nested lambda that reuses
// the name: the inner `r` is a fresh element, so its `score` is nullable again.
// Both branches must therefore agree on array<number|null>.
func TestMapLambda_ParamGuardDroppedByNestedShadow(t *testing.T) {
	c := ctx(t, lambdaCtxJSON)
	expr := `map(input.rows, r => r.score != null ? map(input.rows, r => r.score) : map(input.rows, q => q.score))`
	assertSchema(t, infer(t, expr, c), `{
		"type": "array",
		"items": {"type": "array", "items": {"type": ["number", "null"]}}
	}`)
}

// ─── Null narrowing inside a lambda body ────────────────────────────────────────

// Narrowing works on a path rooted at a lambda parameter, so the guarded branch
// is not nullable. Without it every optional element field would need `??` even
// after an explicit null check.
func TestMapLambda_NullNarrowingOnParamPath(t *testing.T) {
	c := ctx(t, lambdaCtxJSON)
	assertSchema(t, infer(t, `map(input.rows, r => r.score != null ? r.score : 0)`, c), `{
		"type": "array",
		"items": {"oneOf": [{"type": "number"}, {"type": "integer"}]}
	}`)
}

// `??` on an element field is the other half: the result must be a plain number,
// usable in arithmetic without a further check.
func TestMapLambda_CoalesceOnParamPath(t *testing.T) {
	c := ctx(t, lambdaCtxJSON)
	assertSchema(t, infer(t, `map(input.rows, r => r.score ?? 0)`, c), `{
		"type": "array",
		"items": {"type": "number"}
	}`)
}

// And the narrowed value is genuinely non-nullable: arithmetic on it would be
// rejected outright if any null survived.
func TestMapLambda_CoalescedParamPathIsUsableInArithmetic(t *testing.T) {
	c := ctx(t, lambdaCtxJSON)
	assertSchema(t, infer(t, `map(input.rows, r => (r.score ?? 0) + r.n)`, c), `{
		"type": "array",
		"items": {"type": "number"}
	}`)
}

// ─── Union sources ──────────────────────────────────────────────────────────────

// `?? [literal]` gives a union of two *non-empty* arrays, so neither variant is
// discarded and the element type is their join. The shared field types the same
// on both sides, so the body stays precise.
func TestMapLambda_UnionSourceCoalesceWithNonEmptyDefault(t *testing.T) {
	c := ctx(t, lambdaCtxJSON)
	assertSchema(t, infer(t, `map(input.optRows ?? [{name: "d"}], x => x.name)`, c), `{
		"type": "array",
		"items": {"type": "string"}
	}`)
}

// A ternary over two arrays with different element types: the element is the
// join of both, not the first one seen.
func TestMapLambda_UnionSourceTernaryJoinsElements(t *testing.T) {
	c := ctx(t, lambdaCtxJSON)
	assertSchema(t, infer(t, `map(input.flag ? input.tags : input.counts, x => x)`, c), `{
		"type": "array",
		"items": {"type": ["integer", "string"]}
	}`)
}

// One itemless variant poisons the whole union: it can supply an element (unlike
// a provably-empty `[]`) and that element is unconstrained, so binding it would
// turn a typo in the body into a runtime null.
func TestMapLambda_UnionSourceItemlessVariantFails(t *testing.T) {
	c := ctx(t, lambdaCtxJSON)
	inferErr(t, `map(input.flag ? input.tags : input.bare, x => x)`, c, "no element type")
}

// ─── Error messages ─────────────────────────────────────────────────────────────
// Asserted because these are registration-time errors an author reads; a generic "invalid
// expression" would not say which side of the lambda is wrong.

// A typo in the body must be caught against the element type, not deferred to a
// runtime null.
func TestMapLambda_ErrBodyFieldTypo(t *testing.T) {
	c := ctx(t, lambdaCtxJSON)
	inferErr(t, `map(input.rows, r => r.nope)`, c, `field "nope" not found in schema`)
}

func TestMapLambda_ErrBodyOperandType(t *testing.T) {
	c := ctx(t, lambdaCtxJSON)
	inferErr(t, `map(input.rows, r => r.name * 2)`, c, "operator requires numeric operands")
}

// Non-array sources name the type they got.
func TestMapLambda_ErrNonArraySourceNamesType(t *testing.T) {
	c := ctx(t, lambdaCtxJSON)
	for _, tc := range []struct{ name, expr, want string }{
		{"string", `map(input.label, x => x)`, `map source must be an array, got "string"`},
		{"object", `map(input.cfg, x => x)`, `map source must be an array, got "object"`},
		{"boolean", `map(input.flag, x => x)`, `map source must be an array, got "boolean"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			inferErr(t, tc.expr, c, tc.want)
		})
	}
}

func TestMapLambda_ErrItemlessArraySource(t *testing.T) {
	c := ctx(t, lambdaCtxJSON)
	inferErr(t, `map(input.bare, x => x)`, c, "map source array has no element type")
}

// The nullable-source rule survives being nested in a literal, and the error is
// attributed to the key that carries it.
func TestMapLambda_ErrNullableSourceInObjectLiteralNamesKey(t *testing.T) {
	c := ctx(t, lambdaCtxJSON)
	inferErr(t, `{a: map(input.optRows, x => x.name)}`, c, `key "a": map source may be null`)
}

func TestMapLambda_ErrNullableSourceInArrayLiteral(t *testing.T) {
	c := ctx(t, lambdaCtxJSON)
	inferErr(t, `[map(input.optRows, x => x.name)]`, c, "map source may be null")
}

// A lambda parameter is not magically an array: an inner map over a scalar
// parameter reports the parameter's real type.
func TestMapLambda_ErrScalarParamAsInnerSource(t *testing.T) {
	c := ctx(t, lambdaCtxJSON)
	inferErr(t, `map(input.tags, t => map(t, u => u))`, c, `map source must be an array, got "string"`)
}

// The failure of a nested map propagates out of the outer body rather than
// collapsing to an untyped array.
func TestMapLambda_ErrNestedMapFailurePropagates(t *testing.T) {
	c := ctx(t, lambdaCtxJSON)
	inferErr(t, `map(input.rows, r => map(input.optRows, x => x.name))`, c, "map source may be null")
}

// ─── Determinism ────────────────────────────────────────────────────────────────

// Inference must be a pure function of the expression and the context: a result
// that depends on Go map iteration order would make generated schemas churn
// between runs and break the fixpoint's equality test. Joins and object literals
// are the constructs that build maps, so they are what this exercises.
func TestMapLambda_Deterministic(t *testing.T) {
	c := ctx(t, lambdaCtxJSON)
	for _, tc := range []struct{ name, expr string }{
		{"coalesced_union_source", `map(input.optRows ?? [{name: "d"}], x => x)`},
		{"ternary_union_source", `map(input.flag ? input.tags : input.counts, x => {a: x})`},
		{"multi_key_object_body", `map(input.rows, r => {a: r.name, b: r.score ?? 0, c: r.n + 1})`},
		{"nested_lambda_body", `map(input.rows, r => map(input.tags, t => {r: r, t: t}))`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assertDeterministic(t, tc.expr, c)
		})
	}
}

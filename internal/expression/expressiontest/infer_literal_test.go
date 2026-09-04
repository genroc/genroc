package expressiontest

import (
	"testing"

	"genroc/internal/schema"
)

// literalCtxJSON carries one field of every kind an object/array literal can hold:
// scalars, a nullable scalar, an optional (not-required) scalar, arrays with and
// without null, objects, an element type with a secret, and a top-level secret.
// Prefixed `literal` so it cannot collide with the fixtures in the sibling files.
const literalCtxJSON = `{
	"type": "object",
	"properties": {
		"input": {
			"type": "object",
			"properties": {
				"name":     { "type": "string" },
				"count":    { "type": "integer" },
				"ratio":    { "type": "number" },
				"flag":     { "type": "boolean" },
				"optName":  { "type": ["string", "null"] },
				"tags":     { "type": "array", "items": { "type": "string" } },
				"optTags":  { "type": ["array", "null"], "items": { "type": "string" } },
				"obj":      { "type": "object", "properties": { "a": { "type": "integer" } }, "required": ["a"] },
				"optObj":   { "type": ["object", "null"], "properties": { "a": { "type": "integer" } }, "required": ["a"] },
				"items": {
					"type": "array",
					"items": {
						"type": "object",
						"properties": {
							"id":    { "type": "string" },
							"token": { "type": "string", "secret": true }
						},
						"required": ["id", "token"]
					}
				},
				"apiKey": { "type": "string", "secret": true },
				"maybe":  { "type": "string" }
			},
			"required": ["name", "count", "ratio", "flag", "optName", "tags", "optTags", "obj", "optObj", "items", "apiKey"]
		}
	},
	"required": ["input"]
}`

// ─── Object literal typing ──────────────────────────────────────────────────────

func TestInferLiteral_Object_SingleKey(t *testing.T) {
	c := ctx(t, literalCtxJSON)
	assertSchema(t, infer(t, `{n: input.count}`, c), `{
		"type": "object",
		"properties": { "n": { "type": "integer" } },
		"required": ["n"]
	}`)
}

// An empty literal is still a closed object: it declares no properties, so every
// key of whatever it is validated against is stripped. Nothing about `{}` should
// make it an open map.
func TestInferLiteral_Object_Empty(t *testing.T) {
	assertSchema(t, infer(t, `{}`, schema.Schema{}), `{"type": "object"}`)
}

// `required` is a []string, so its order is part of the schema's bytes and two registrations
// must not differ by ordering — the fixpoint compares canonical JSON. The mixed-case and
// underscore keys pin the sort as byte-wise ('B' < '_' < 'a'), not case-folded.
func TestInferLiteral_Object_RequiredIsSortedAndComplete(t *testing.T) {
	c := ctx(t, literalCtxJSON)
	assertSchema(t, infer(t, `{z: 1, m: 2, a: 3, B: 4, _q: 5}`, c), `{
		"type": "object",
		"properties": {
			"z":  { "type": "integer" },
			"m":  { "type": "integer" },
			"a":  { "type": "integer" },
			"B":  { "type": "integer" },
			"_q": { "type": "integer" }
		},
		"required": ["B", "_q", "a", "m", "z"]
	}`)
}

func TestInferLiteral_Object_Nested(t *testing.T) {
	c := ctx(t, literalCtxJSON)
	assertSchema(t, infer(t, `{outer: {inner: {leaf: input.name}}, sibling: 1}`, c), `{
		"type": "object",
		"properties": {
			"outer": {
				"type": "object",
				"properties": {
					"inner": {
						"type": "object",
						"properties": { "leaf": { "type": "string" } },
						"required": ["leaf"]
					}
				},
				"required": ["inner"]
			},
			"sibling": { "type": "integer" }
		},
		"required": ["outer", "sibling"]
	}`)
}

// A quoted key may hold characters an identifier cannot. The quotes are lexical
// only — the property name is the unquoted text, and it sorts by that text.
func TestInferLiteral_Object_QuotedAndDashedKeys(t *testing.T) {
	c := ctx(t, literalCtxJSON)
	assertSchema(t, infer(t, `{"content-type": "json", "x y": 1, plain: true}`, c), `{
		"type": "object",
		"properties": {
			"content-type": { "type": "string" },
			"x y":          { "type": "integer" },
			"plain":        { "type": "boolean" }
		},
		"required": ["content-type", "plain", "x y"]
	}`)
}

// Every scalar literal kind maps to its JSON Schema type; `null` is a real type,
// not an absent key, so the property exists and is required.
func TestInferLiteral_Object_ScalarValueKinds(t *testing.T) {
	c := ctx(t, literalCtxJSON)
	assertSchema(t, infer(t, `{i: 1, f: 1.5, s: "x", b: false, n: null}`, c), `{
		"type": "object",
		"properties": {
			"i": { "type": "integer" },
			"f": { "type": "number" },
			"s": { "type": "string" },
			"b": { "type": "boolean" },
			"n": { "type": "null" }
		},
		"required": ["b", "f", "i", "n", "s"]
	}`)
}

// Required-ness and nullability are independent: the key is always present (the
// literal writes it unconditionally) even though its value may be null. Emitting
// the key as optional instead would let a consumer treat a real null as "absent".
func TestInferLiteral_Object_NullableValueStaysRequired(t *testing.T) {
	c := ctx(t, literalCtxJSON)
	assertSchema(t, infer(t, `{v: input.optName}`, c), `{
		"type": "object",
		"properties": { "v": { "type": ["string", "null"] } },
		"required": ["v"]
	}`)
}

// Same rule for a field that is nullable because it is *optional* in the context
// schema (lookupProperty wraps it), not because it declares null.
func TestInferLiteral_Object_OptionalSourceFieldStaysRequired(t *testing.T) {
	c := ctx(t, literalCtxJSON)
	assertSchema(t, infer(t, `{v: input.maybe}`, c), `{
		"type": "object",
		"properties": { "v": { "type": ["string", "null"] } },
		"required": ["v"]
	}`)
}

// Composed values: each sub-expression is inferred in full, so a literal is a
// transparent constructor over whatever the value expressions produce.
func TestInferLiteral_Object_ComposedValueKinds(t *testing.T) {
	c := ctx(t, literalCtxJSON)
	assertSchema(t, infer(t, `{
		arr:  [1, 2],
		obj:  {k: input.name},
		tern: input.flag ? 1 : "a",
		coal: input.optName ?? "d",
		mapd: map(input.items, x => x.id)
	}`, c), `{
		"type": "object",
		"properties": {
			"arr":  { "type": "array", "items": { "type": "integer" } },
			"obj":  { "type": "object", "properties": { "k": { "type": "string" } }, "required": ["k"] },
			"tern": { "oneOf": [ { "type": "integer" }, { "type": "string" } ] },
			"coal": { "type": "string" },
			"mapd": { "type": "array", "items": { "type": "string" } }
		},
		"required": ["arr", "coal", "mapd", "obj", "tern"]
	}`)
}

// A failure inside a value is reported with the key that owns it — without the
// wrapper, a bad field in a ten-key literal gives no clue which key to fix.
func TestInferLiteral_Object_ErrorNamesTheKey(t *testing.T) {
	c := ctx(t, literalCtxJSON)
	inferErr(t, `{ok: 1, bad: input.nope}`, c, `key "bad"`)
}

// Duplicate keys are a parse error, not a last-one-wins merge — the parse failure
// has to survive Infer's wrapping so the author sees it.
func TestInferLiteral_DuplicateKeyRejected(t *testing.T) {
	c := ctx(t, literalCtxJSON)
	inferErr(t, `{a: 1, a: 2}`, c, `duplicate object key "a"`)
}

// The quoted spelling of a key is the same key, so it is a duplicate too.
func TestInferLiteral_DuplicateQuotedKeyRejected(t *testing.T) {
	c := ctx(t, literalCtxJSON)
	inferErr(t, `{a: 1, "a": 2}`, c, `duplicate object key "a"`)
}

// ─── Array literal join semantics ───────────────────────────────────────────────

func TestInferLiteral_Array_SingleElement(t *testing.T) {
	c := ctx(t, literalCtxJSON)
	assertSchema(t, infer(t, `[input.name]`, c), `{
		"type": "array",
		"items": { "type": "string" }
	}`)
}

// integer + number does NOT collapse to "number": the join builds a union and canonicalize
// merges it into a type array. Semantically redundant (integer ⊆ number) but sound — noted so
// a future simplification is deliberate rather than accidental.
func TestInferLiteral_Array_IntegerAndFloatWiden(t *testing.T) {
	c := ctx(t, literalCtxJSON)
	assertSchema(t, infer(t, `[1, 1.5]`, c), `{
		"type": "array",
		"items": { "type": ["integer", "number"] }
	}`)
}

// The same via context fields rather than literals.
func TestInferLiteral_Array_IntegerAndFloatFieldsWiden(t *testing.T) {
	c := ctx(t, literalCtxJSON)
	assertSchema(t, infer(t, `[input.count, input.ratio]`, c), `{
		"type": "array",
		"items": { "type": ["integer", "number"] }
	}`)
}

// Unrelated scalars merge into a type array too (sorted, so deterministic).
func TestInferLiteral_Array_StringAndIntegerUnion(t *testing.T) {
	c := ctx(t, literalCtxJSON)
	assertSchema(t, infer(t, `["a", 1]`, c), `{
		"type": "array",
		"items": { "type": ["integer", "string"] }
	}`)
}

// Two structurally identical objects must join to that object, not to a
// two-armed union of it with itself. A pointless union would make every
// `[{...}, {...}]` fan-out unusable as a typed `over` source.
func TestInferLiteral_Array_IdenticalObjectsDoNotUnion(t *testing.T) {
	c := ctx(t, literalCtxJSON)
	assertSchema(t, infer(t, `[{a: 1, b: input.name}, {a: 2, b: "x"}]`, c), `{
		"type": "array",
		"items": {
			"type": "object",
			"properties": { "a": { "type": "integer" }, "b": { "type": "string" } },
			"required": ["a", "b"]
		}
	}`)
}

// Differing objects merge property-wise rather than becoming a union: a key on
// only one side survives as nullable, and `required` is the intersection — so a
// consumer that reads only the shared keys still type-checks.
func TestInferLiteral_Array_DifferentObjectsMergeProperties(t *testing.T) {
	c := ctx(t, literalCtxJSON)
	assertSchema(t, infer(t, `[{a: 1, shared: "s"}, {b: "x", shared: "t"}]`, c), `{
		"type": "array",
		"items": {
			"type": "object",
			"properties": {
				"a":      { "type": ["integer", "null"] },
				"b":      { "type": ["null", "string"] },
				"shared": { "type": "string" }
			},
			"required": ["shared"]
		}
	}`)
}

// A null member makes the element type nullable rather than producing a
// oneOf[T, null] wrapper — the compact form indexing and `??` already expect.
func TestInferLiteral_Array_WithNullMember(t *testing.T) {
	c := ctx(t, literalCtxJSON)
	assertSchema(t, infer(t, `[1, null]`, c), `{
		"type": "array",
		"items": { "type": ["integer", "null"] }
	}`)
}

// An all-null literal stays array<null> — there is no other type to join with.
func TestInferLiteral_Array_AllNull(t *testing.T) {
	assertSchema(t, infer(t, `[null, null]`, schema.Schema{}), `{
		"type": "array",
		"items": { "type": "null" }
	}`)
}

func TestInferLiteral_Array_OfArrays(t *testing.T) {
	c := ctx(t, literalCtxJSON)
	assertSchema(t, infer(t, `[[1], [2, 3]]`, c), `{
		"type": "array",
		"items": { "type": "array", "items": { "type": "integer" } }
	}`)
}

// Arrays are NOT merged element-wise the way objects are property-wise: two arrays with
// different item types become a union of arrays, not an array of a union. That is the more
// precise reading (it forbids a mixed array), so the asymmetry is deliberate.
func TestInferLiteral_Array_OfArraysWithDifferentItemsUnions(t *testing.T) {
	c := ctx(t, literalCtxJSON)
	assertSchema(t, infer(t, `[[1], ["a"]]`, c), `{
		"type": "array",
		"items": {
			"oneOf": [
				{ "type": "array", "items": { "type": "integer" } },
				{ "type": "array", "items": { "type": "string" } }
			]
		}
	}`)
}

// A nullable field alongside a non-nullable one of the same base type yields one
// nullable element type — not a union of the two.
func TestInferLiteral_Array_NullableFieldMember(t *testing.T) {
	c := ctx(t, literalCtxJSON)
	assertSchema(t, infer(t, `[input.optName, "x"]`, c), `{
		"type": "array",
		"items": { "type": ["null", "string"] }
	}`)
}

// ─── Literals as the base of an accessor / a map source ─────────────────────────

// A literal is a first-class value, so the postfix accessors apply to it. This is
// mostly a parser/inference wiring check: parsePostfix runs after parsePrimary, so
// `{...}.k` must resolve through the literal's own inferred properties.
func TestInferLiteral_FieldAccessOnObjectLiteral(t *testing.T) {
	c := ctx(t, literalCtxJSON)
	assertSchema(t, infer(t, `{a: 1}.a`, c), `{"type": "integer"}`)
}

// ...and through two levels of literal.
func TestInferLiteral_NestedFieldAccessOnObjectLiteral(t *testing.T) {
	c := ctx(t, literalCtxJSON)
	assertSchema(t, infer(t, `{a: {b: input.name}}.a.b`, c), `{"type": "string"}`)
}

// Indexing stays nullable even on a literal of known length: inferIndexNode
// uses Index(), which is nullable because a constant index may be out of
// bounds. It does not special-case a literal whose length it can see.
func TestInferLiteral_IndexOnArrayLiteralIsNullable(t *testing.T) {
	c := ctx(t, literalCtxJSON)
	assertSchema(t, infer(t, `[1, 2][0]`, c), `{"type": ["integer", "null"]}`)
}

// A key lookup is not an index: only integers are valid inside [].
func TestInferLiteral_IndexOnObjectLiteralRejected(t *testing.T) {
	c := ctx(t, literalCtxJSON)
	inferErr(t, `{a: 1}[0]`, c, `index access [n] requires an array schema, got type "object"`)
}

// An array literal is a perfectly good map source — its element type comes from
// the same items slot as a context array's.
func TestInferLiteral_ArrayLiteralAsMapSource(t *testing.T) {
	c := ctx(t, literalCtxJSON)
	assertSchema(t, infer(t, `map([1, 2, 3], x => x + 1)`, c), `{
		"type": "array",
		"items": { "type": "integer" }
	}`)
}

// `[]` is provably empty, so elementOf refuses to bind an element — mapping over
// a literal empty array is rejected even though it would yield [] at runtime.
// Pedantic but consistent with the itemless-array rule, and the expression is a
// no-op worth flagging.
func TestInferLiteral_EmptyArrayLiteralAsMapSourceRejected(t *testing.T) {
	c := ctx(t, literalCtxJSON)
	inferErr(t, `map([], x => x)`, c, "no element type")
}

func TestInferLiteral_ObjectLiteralAsMapSourceRejected(t *testing.T) {
	c := ctx(t, literalCtxJSON)
	inferErr(t, `map({a: 1}, x => x)`, c, `must be an array, got "object"`)
}

// ─── Empty array `[]` interactions ──────────────────────────────────────────────

// `[]` in a ternary keeps maxItems 0 as its own arm, which is what elementOf needs: the
// provably-empty arm is skipped so mapping keeps the other arm's element type. It is absorbed
// rather than unioned — see the union test below for why.
func TestInferLiteral_EmptyArray_InTernary(t *testing.T) {
	c := ctx(t, literalCtxJSON)
	assertSchema(t, infer(t, `input.flag ? [] : input.tags`, c), `{
		"type": "array", "items": { "type": "string" }
	}`)
}

// And the empty arm must not poison the element type downstream.
func TestInferLiteral_EmptyArray_InTernaryAsMapSource(t *testing.T) {
	c := ctx(t, literalCtxJSON)
	assertSchema(t, infer(t, `map(input.flag ? [] : input.tags, x => x)`, c), `{
		"type": "array",
		"items": { "type": "string" }
	}`)
}

// `xs ?? []` is the documented idiom: the union keeps xs's items, and the empty
// arm is discarded by elementOf. The empty arm is absorbed: it contributes no
// values, and keeping it would build an exclusive oneOf that rejects its own
// empty result.
func TestInferLiteral_EmptyArray_AsCoalesceDefault(t *testing.T) {
	c := ctx(t, literalCtxJSON)
	assertSchema(t, infer(t, `input.optTags ?? []`, c), `{
		"type": "array", "items": { "type": "string" }
	}`)
}

// Regression: `[]` used to union as a oneOf (EXACTLY one), but an empty array satisfies both
// arms — so `xs ?? []` inferred a schema rejecting its own empty result, and `over:` failed
// registration because Items() is unreachable through a union. Every such path now absorbs it.
func TestInferLiteral_EmptyArrayUnionsValidateTheirOwnValue(t *testing.T) {
	c := ctx(t, literalCtxJSON)
	for _, tc := range []struct{ name, expr string }{
		{"coalesce_default", `input.optTags ?? []`},
		{"ternary_then", `input.flag ? [] : input.tags`},
		{"ternary_else", `input.flag ? input.tags : []`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assertDescribesOwnEmptyArray(t, c, tc.expr)
		})
	}
}

// Nested: the empty arm must be absorbed at the element level too.
func TestInferLiteral_EmptyArrayNestedUnionValidatesItsOwnValue(t *testing.T) {
	c := ctx(t, literalCtxJSON)
	nested, err := c.Infer(`[input.tags, []]`)
	if err != nil {
		t.Fatalf("Infer: %v", err)
	}
	if _, err := nested.Validate([]any{[]any{"a"}, []any{}}); err != nil {
		t.Errorf("[input.tags, []] rejects its own value: %v", err)
	}
}

// `[] ?? xs` is the mirror case: the left operand is a literal that can never be
// null, so ?? is a no-op and the right side is dropped entirely. Widening to a
// union here would silently promise elements the expression can never produce.
func TestInferLiteral_EmptyArray_OnLeftOfCoalesceIsNoOp(t *testing.T) {
	c := ctx(t, literalCtxJSON)
	assertSchema(t, infer(t, `[] ?? input.tags`, c), `{"type": "array", "maxItems": 0}`)
}

// Nested in an object literal, maxItems 0 stays on the property — accurate, since
// the literal really does write an empty array there. It also means the schema
// forbids elements, so a consumer must not treat this property as "some array".
func TestInferLiteral_EmptyArray_InsideObjectLiteral(t *testing.T) {
	c := ctx(t, literalCtxJSON)
	assertSchema(t, infer(t, `{xs: [], n: 1}`, c), `{
		"type": "object",
		"properties": {
			"xs": { "type": "array", "maxItems": 0 },
			"n":  { "type": "integer" }
		},
		"required": ["n", "xs"]
	}`)
}

// As an element, `[]` cannot merge with a typed array, so the element type would stay a union
// whose first arm forbids elements — sound but lossy (`[[], [1]][1][0]` typed as
// possibly-nothing). Absorbed at the element level so the outer array keeps a usable type.
func TestInferLiteral_EmptyArray_AsArrayElement(t *testing.T) {
	c := ctx(t, literalCtxJSON)
	assertSchema(t, infer(t, `[[], [1]]`, c), `{
		"type": "array",
		"items": { "type": "array", "items": { "type": "integer" } }
	}`)
}

// ─── `??` interactions with literals ────────────────────────────────────────────

// A nullable object on the left and a differently-shaped literal default: the
// null is stripped and both shapes are offered.
func TestInferLiteral_Coalesce_NullableObjectWithObjectDefault(t *testing.T) {
	c := ctx(t, literalCtxJSON)
	assertSchema(t, infer(t, `input.optObj ?? {b: 2}`, c), `{
		"oneOf": [
			{ "type": "object", "properties": { "a": { "type": "integer" } }, "required": ["a"] },
			{ "type": "object", "properties": { "b": { "type": "integer" } }, "required": ["b"] }
		]
	}`)
}

// When the default has exactly the left's non-null shape the union collapses —
// otherwise the common "default to the same shape" idiom would double every
// object type it touches.
func TestInferLiteral_Coalesce_MatchingDefaultCollapses(t *testing.T) {
	c := ctx(t, literalCtxJSON)
	assertSchema(t, infer(t, `input.optObj ?? {a: 1}`, c), `{
		"type": "object",
		"properties": { "a": { "type": "integer" } },
		"required": ["a"]
	}`)
}

// An object literal is never null, so `??` must return the left untouched. A
// union here would be pure noise on an expression with one possible shape.
func TestInferLiteral_Coalesce_ObjectLiteralLeftIsNoOp(t *testing.T) {
	c := ctx(t, literalCtxJSON)
	assertSchema(t, infer(t, `{a: 1} ?? {b: 2}`, c), `{
		"type": "object",
		"properties": { "a": { "type": "integer" } },
		"required": ["a"]
	}`)
}

// ─── Operators applied to literals ──────────────────────────────────────────────

// Arithmetic, ordering, and the unary operators are scalar-only. Each must name
// the offending type so the author sees "object"/"array" rather than a generic
// type-check failure.
func TestInferLiteral_OperatorsRejectLiterals(t *testing.T) {
	c := ctx(t, literalCtxJSON)
	for _, tc := range []struct{ name, expr, want string }{
		{"add_object", `{a: 1} + 1`, `operator requires numeric operands, got "object" and "integer"`},
		{"add_arrays", `[1] + [2]`, `operator requires numeric operands, got "array" and "array"`},
		{"less_than_objects", `{a: 1} < {b: 2}`, `comparison requires numeric operands, got "object" and "object"`},
		{"not_object", `!{a: 1}`, `! requires a boolean operand, got "object"`},
		{"negate_array", `-[1]`, `unary operator requires a numeric operand, got "array"`},
		// String concatenation does not extend to arrays either.
		{"concat_array_and_string", `["a"] + "b"`, `operator requires numeric operands, got "array" and "string"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			inferErr(t, tc.expr, c, tc.want)
		})
	}
}

// Comparing two structured values is rejected at registration, matching the
// runtime half. A deep walk is the only useful answer and this language does not
// hide one behind an operator; the old total-== also panicked, since Go's ==
// crashes on two operands sharing an uncomparable dynamic type.
func TestInferLiteral_StructuredComparisonRejected(t *testing.T) {
	c := ctx(t, literalCtxJSON)
	for _, tc := range []struct{ name, expr string }{
		{"eq_object_literals", `{a: 1} == {a: 1}`},
		{"ne_object_literals", `{a: 1} != {b: 2}`},
		{"ne_array_literals", `[1] != [2]`},
		{"eq_empty_literal_and_field", `[] == input.tags`},
		{"eq_field_and_field", `input.tags == input.tags`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			inferErr(t, tc.expr, c, "not supported between")
		})
	}
}

// The guard fires only when BOTH sides are structured, so a null check on an
// array still type-checks — that is the common and safe use of == on a
// container, and rejecting it would break existing definitions.
func TestInferLiteral_ComparisonAgainstNullOrScalarStillBoolean(t *testing.T) {
	c := ctx(t, literalCtxJSON)
	for _, tc := range []struct{ name, expr string }{
		{"array_field_vs_null", `input.tags == null`},
		{"object_literal_vs_null", `{a: 1} == null`},
		{"array_literal_vs_scalar", `[1] == 1`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assertSchema(t, infer(t, tc.expr, c), `{"type": "boolean"}`)
		})
	}
}

// ─── Secret taint ───────────────────────────────────────────────────────────────

// A secret field copied into a literal keeps its structural `secret` flag on the
// property, so redaction of the produced value works field-wise even without the
// expression-level taint.
func TestInferLiteral_Object_CarriesStructuralSecretFlag(t *testing.T) {
	c := ctx(t, literalCtxJSON)
	assertSchema(t, infer(t, `{k: input.apiKey, plain: input.name}`, c), `{
		"type": "object",
		"properties": {
			"k":     { "type": "string", "secret": true },
			"plain": { "type": "string" }
		},
		"required": ["k", "plain"]
	}`)
}

// A single-member array keeps the flag on items…
func TestInferLiteral_Array_CarriesStructuralSecretFlag(t *testing.T) {
	c := ctx(t, literalCtxJSON)
	assertSchema(t, infer(t, `[input.apiKey]`, c), `{
		"type": "array",
		"items": { "type": "string", "secret": true }
	}`)
}

// …but joining a secret string with a plain one DROPS it: canonicalize's
// simple-variant merge rebuilds a bare {type:[...]} node and `secret` is not one
// of the fields it carries over.
func TestInferLiteral_Array_JoinDropsStructuralSecretFlag(t *testing.T) {
	c := ctx(t, literalCtxJSON)
	assertSchema(t, infer(t, `[input.apiKey, "x"]`, c), `{
		"type": "array",
		"items": { "type": "string" }
	}`)
}

// ─── Determinism ────────────────────────────────────────────────────────────────
// Generated schemas are compared by bytes and Go map iteration is randomized, so the same
// literal — and two source orderings of one key set — must produce identical JSON.

// manyKeyLiteral mixes nested objects, an array join and a nullable field, i.e.
// every construct whose inference builds a Go map.
const manyKeyLiteral = `{d: 4, a: 1, c: {z: 1, y: 2, x: 3}, b: [1, "s"], e: input.optName, f: 6, g: 7, h: 8}`

func TestInferLiteral_Object_RepeatedInferenceIsIdentical(t *testing.T) {
	c := ctx(t, literalCtxJSON)
	first := inferredJSON(t, manyKeyLiteral, c)
	for i := 0; i < 20; i++ {
		if got := inferredJSON(t, manyKeyLiteral, c); got != first {
			t.Fatalf("inference is not deterministic on repeat %d:\n got:  %s\n want: %s", i, got, first)
		}
	}
}

// Source key order must not reach the output.
func TestInferLiteral_Object_KeyOrderDoesNotChangeSchema(t *testing.T) {
	c := ctx(t, literalCtxJSON)
	if a, b := inferredJSON(t, `{b: 1, a: 2}`, c), inferredJSON(t, `{a: 2, b: 1}`, c); a != b {
		t.Errorf("key order changed the inferred schema:\n {b,a}: %s\n {a,b}: %s", a, b)
	}
}

// Same for the nested object's keys.
func TestInferLiteral_Object_NestedKeyOrderDoesNotChangeSchema(t *testing.T) {
	c := ctx(t, literalCtxJSON)
	if a, b := inferredJSON(t, `{o: {q: 1, p: 2}}`, c), inferredJSON(t, `{o: {p: 2, q: 1}}`, c); a != b {
		t.Errorf("nested key order changed the inferred schema:\n %s\n %s", a, b)
	}
}

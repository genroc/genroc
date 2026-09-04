package expressiontest

import (
	"encoding/json"
	"testing"

	exprlib "github.com/expr-lang/expr"
)

var mapEnv = map[string]any{
	"input": map[string]any{
		"items": []any{
			map[string]any{"id": "a", "n": 1, "user": map[string]any{"name": "Ann"}},
			map[string]any{"id": "b", "n": 2, "user": map[string]any{"name": "Bob"}},
		},
		"others": []any{map[string]any{"m": 10}},
		"empty":  []any{},
	},
}

// The three-way conformance contract for lambdas: expr-lang has no `=>`, so each case pairs
// our syntax with `{let x = #; body}`, which binds identically. Real expr-lang stays the oracle
// even though the surface syntax diverges.
func TestMap_MatchesExprLang(t *testing.T) {
	for _, tc := range []struct{ name, ours, oracle string }{
		{"field_of_element",
			`map(input.items, x => x.id)`,
			`map(input.items, {let x = #; x.id})`},
		{"object_body_with_nested_field",
			`map(input.items, x => {id: x.id, name: x.user.name})`,
			`map(input.items, {let x = #; {id: x.id, name: x.user.name}})`},
		{"arithmetic_body",
			`map(input.items, x => x.n + 1)`,
			`map(input.items, {let x = #; x.n + 1})`},
		{"index_param",
			`map(input.items, (x, i) => {i: i, id: x.id})`,
			`map(input.items, {let x = #; let i = #index; {i: i, id: x.id}})`},
		{"empty_source",
			`map(input.empty, x => x.id)`,
			`map(input.empty, {let x = #; x.id})`},
		{"missing_field_of_element",
			`map(input.items, x => x.missing)`,
			`map(input.items, {let x = #; x.missing})`},
		// Nested lambda reaching the outer element. expr-lang's bare `#` cannot
		// express this at all — the inner predicate rebinds it — which is the
		// concrete reason lambdas replaced the pointer syntax.
		{"nested_lambda_reaching_the_outer_element",
			`map(input.items, x => map(input.others, y => {sum: x.n + y.m}))`,
			`map(input.items, {let x = #; map(input.others, {let y = #; {sum: x.n + y.m}})})`},
		// A parameter shadowing a context root reads the element, not the root.
		{"param_shadowing_a_context_root",
			`map(input.items, input => input.id)`,
			`map(input.items, {let input = #; input.id})`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			want, err := exprlib.Eval(tc.oracle, mapEnv)
			if err != nil {
				t.Fatalf("expr-lang oracle %q: %v", tc.oracle, err)
			}
			got := evalOK(t, tc.ours, mapEnv)
			wj, _ := json.Marshal(want)
			gj, _ := json.Marshal(got)
			if string(wj) != string(gj) {
				t.Errorf("map mismatch for %q\n  ours:      %s\n  expr-lang: %s", tc.ours, gj, wj)
			}
		})
	}
}

// A null source is rejected rather than silently yielding []; inference rejects
// it too, so this only guards hand-built contexts.
func TestMap_EvalNullSourceErrors(t *testing.T) {
	evalErr(t, `map(missing, x => x)`, map[string]any{"missing": nil})
}

func TestMap_EvalNonArraySourceErrors(t *testing.T) {
	evalErr(t, `map(n, x => x)`, map[string]any{"n": 5})
}

const mapCtxJSON = `{
	"type": "object",
	"properties": {
		"input": {
			"type": "object",
			"properties": {
				"items": {
					"type": "array",
					"items": {
						"type": "object",
						"properties": {
							"id":    {"type": "string"},
							"n":     {"type": "integer"},
							"token": {"type": "string", "secret": true}
						},
						"required": ["id", "n", "token"]
					}
				},
				"bare":  {"type": "array"},
				"opt":   {"type": ["array", "null"], "items": {"type": "string"}},
				"count": {"type": "integer"}
			},
			"required": ["items", "bare", "opt", "count"]
		}
	},
	"required": ["input"]
}`

func TestMap_InferReshape(t *testing.T) {
	c := ctx(t, mapCtxJSON)
	assertSchema(t, infer(t, `map(input.items, x => {key: x.id, next: x.n + 1})`, c), `{
		"type": "array",
		"items": {
			"type": "object",
			"properties": {
				"key":  {"type": "string"},
				"next": {"type": "integer"}
			},
			"required": ["key", "next"]
		}
	}`)
}

// The element type comes from items, not from Index: map only ever visits real
// elements, so a mapped field must not pick up the out-of-bounds nullability
// that indexing carries.
func TestMap_InferElementIsNotNullable(t *testing.T) {
	c := ctx(t, mapCtxJSON)
	assertSchema(t, infer(t, `map(input.items, x => x.id)`, c), `{
		"type": "array",
		"items": {"type": "string"}
	}`)
}

func TestMap_InferIndexParamIsInteger(t *testing.T) {
	c := ctx(t, mapCtxJSON)
	assertSchema(t, infer(t, `map(input.items, (x, i) => i)`, c), `{
		"type": "array",
		"items": {"type": "integer"}
	}`)
}

func TestMap_InferNested(t *testing.T) {
	c := ctx(t, mapCtxJSON)
	assertSchema(t, infer(t, `map(input.items, x => map(input.items, y => x.n + y.n))`, c), `{
		"type": "array",
		"items": {"type": "array", "items": {"type": "integer"}}
	}`)
}

// A parameter shadows a context root for inference exactly as it does at runtime.
func TestMap_InferParamShadowsRoot(t *testing.T) {
	c := ctx(t, mapCtxJSON)
	assertSchema(t, infer(t, `map(input.items, input => input.n)`, c), `{
		"type": "array",
		"items": {"type": "integer"}
	}`)
}

// A nullable source would panic at runtime, so it must be a registration error.
func TestMap_InferNullableSourceRejected(t *testing.T) {
	inferErr(t, `map(input.opt, x => x)`, ctx(t, mapCtxJSON), "may be null")
}

func TestMap_InferNonArraySourceRejected(t *testing.T) {
	inferErr(t, `map(input.count, x => x)`, ctx(t, mapCtxJSON), "must be an array")
}

// Binding an unconstrained element would turn a typo into a runtime null.
func TestMap_InferItemlessSourceRejected(t *testing.T) {
	inferErr(t, `map(input.bare, x => x.anything)`, ctx(t, mapCtxJSON), "no element type")
}

// A ?? default restores a usable source without losing the element type: the
// [] variant is provably empty, so it contributes nothing to the join.
func TestMap_InferCoalescedSourceKeepsElementType(t *testing.T) {
	c := ctx(t, mapCtxJSON)
	assertSchema(t, infer(t, `map(input.opt ?? [], x => x)`, c), `{
		"type": "array",
		"items": {"type": "string"}
	}`)
}

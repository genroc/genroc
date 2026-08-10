package expressiontest

import "testing"

// Constraints ride navigation, and that is what lets the compat report see a narrowing at
// all: `$: input.tier` carries the enum into `$defs[<id>_output]`, where IsSubset compares
// it. Dropping them here would report every constraint narrowing below the input boundary as
// compatible, and nothing else in the suite would say so.

const constraintCtxJSON = `{
	"type": "object",
	"properties": {
		"input": {
			"type": "object",
			"properties": {
				"tier":  { "type": "string", "enum": ["free", "paid"] },
				"n":     { "type": "integer", "minimum": 1, "maximum": 10 },
				"name":  { "type": "string", "minLength": 2, "maxLength": 8 },
				"rows":  { "type": "array", "items": { "type": "string", "minLength": 1 },
				           "minItems": 1, "maxItems": 3 },
				"maybe": { "type": "string", "enum": ["x"] }
			},
			"required": ["tier", "n", "name", "rows"]
		}
	},
	"required": ["input"]
}`

func TestInfer_NavigationKeepsEveryConstraintKind(t *testing.T) {
	c := ctx(t, constraintCtxJSON)
	for _, tc := range []struct{ name, expr, want string }{
		{"enum", "input.tier", `{"type":"string","enum":["free","paid"]}`},
		{"numeric bounds", "input.n", `{"type":"integer","minimum":1,"maximum":10}`},
		{"string lengths", "input.name", `{"type":"string","minLength":2,"maxLength":8}`},
		{"item counts and the item's own bound", "input.rows",
			`{"type":"array","items":{"type":"string","minLength":1},"minItems":1,"maxItems":3}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assertSchema(t, infer(t, tc.expr, c), tc.want)
		})
	}
}

// Nullability is added around the constraint, not in place of it: an optional read and an
// index that may miss both widen the TYPE while the value, when present, is still bounded.
func TestInfer_NullabilityDoesNotDropTheConstraint(t *testing.T) {
	c := ctx(t, constraintCtxJSON)
	assertSchema(t, infer(t, "input.maybe", c), `{"type":["string","null"],"enum":["x"]}`)
	assertSchema(t, infer(t, "input.rows[0]", c), `{"type":["string","null"],"minLength":1}`)
}

// Arithmetic drops the bounds, and must: the result of `n + 1` has no bound derivable
// without interval arithmetic, and inventing one would be a claim the checker cannot honour.
// Pinned so the loss stays deliberate rather than becoming a place to "fix".
func TestInfer_ComputationDropsBoundsItCannotDerive(t *testing.T) {
	c := ctx(t, constraintCtxJSON)
	assertSchema(t, infer(t, "input.n + 1", c), `{"type":"integer"}`)
	assertSchema(t, infer(t, "input.n * 2", c), `{"type":"integer"}`)
}

// A literal is a container around values that were navigated to, so the constraints travel
// into it — this is the shape a task `output` block actually produces.
func TestInfer_LiteralsCarryConstraintsIntoTheOutputShape(t *testing.T) {
	c := ctx(t, constraintCtxJSON)
	assertSchema(t, infer(t, "[input.tier]", c),
		`{"type":"array","items":{"type":"string","enum":["free","paid"]}}`)
	assertSchema(t, infer(t, "{a: input.n}", c),
		`{"type":"object","properties":{"a":{"type":"integer","minimum":1,"maximum":10}},"required":["a"]}`)
}

package expressiontest

import "testing"

// A guard on the left of `&&` narrows the right, because the evaluator short-circuits:
// `&&` on false never evaluates its right operand (eval.go, evalLogical). The mirror holds
// for `||`, whose right operand runs only when the left is FALSE. Without this the only way
// to read a nullable is `??`, and the idiom every author writes first is refused.
var guardCtx = mustSchema(`{
	"properties": {
		"n": {"type": ["integer", "null"]},
		"m": {"type": ["integer", "null"]},
		"s": {"type": ["string", "null"]}
	},
	"required": ["n", "m", "s"]
}`)

func TestLogicalNarrowing_GuardNarrowsTheRightOperand(t *testing.T) {
	for _, expr := range []string{
		`n != null && n > 2`,
		`n == null || n > 2`,
		`n != null && m != null && n > m`,
		`n != null && n + 1 > 2`,
		`s != null && s == "x"`,
	} {
		t.Run(expr, func(t *testing.T) {
			assertSchema(t, infer(t, expr, guardCtx), `{"type": "boolean"}`)
		})
	}
}

// The narrowing must follow the branch the evaluator actually takes. Each of these reads the
// nullable on the side where the guard does NOT hold, so each must stay refused — accepting
// one would type a value the engine can hand back as null.
func TestLogicalNarrowing_WrongBranchStaysRefused(t *testing.T) {
	for _, expr := range []string{
		`n == null && n > 2`, // right runs only when n IS null
		`n != null || n > 2`, // || runs its right when the left is false: n IS null
		`n > 2 && n != null`, // the guard comes after the use
		`n > 2 || n == null`, // same, mirrored
	} {
		t.Run(expr, func(t *testing.T) {
			inferErr(t, expr, guardCtx, "")
		})
	}
}

// Guards compose through grouping — if `(n != null && true)` held then `n != null` held —
// and a conjunction feeds the ternary the same way it feeds a right operand.
func TestLogicalNarrowing_ComposesThroughGrouping(t *testing.T) {
	assertSchema(t, infer(t, `(n != null && true) && n > 2`, guardCtx), `{"type": "boolean"}`)
	assertSchema(t, infer(t, `(n != null && n > 2) ? 1 : 0`, guardCtx), `{"type": "integer"}`)
}

// What a conjunction proves is per-reference. The truth of a whole boolean expression is not
// a fact about `n`, so nothing narrows through it — the alternative is guessing.
func TestLogicalNarrowing_OnlyPerReferenceFacts(t *testing.T) {
	inferErr(t, `(n != null && n > 2) == true && n > 3`, guardCtx, "")
}

// The runtime pairing: every expression the narrowing now admits must also survive the value
// it was narrowed against. A type system that accepts `n != null && n > 2` and an evaluator
// that then compares null to 2 would have traded a registration error for an uncatchable
// engine.expression at runtime, which is worse than no feature.
func TestLogicalNarrowing_RuntimeAgreesWithTheNarrowing(t *testing.T) {
	for _, tc := range []struct {
		expr string
		ctx  map[string]any
		want any
	}{
		{`n != null && n > 2`, map[string]any{"n": nil}, false},
		{`n != null && n > 2`, map[string]any{"n": 5}, true},
		{`n != null && n > 2`, map[string]any{"n": 1}, false},
		{`n == null || n > 2`, map[string]any{"n": nil}, true},
		{`n == null || n > 2`, map[string]any{"n": 1}, false},
		{`n != null && m != null && n > m`, map[string]any{"n": nil, "m": nil}, false},
		{`n != null && m != null && n > m`, map[string]any{"n": 5, "m": 1}, true},
	} {
		t.Run(tc.expr, func(t *testing.T) {
			if got := evalOK(t, tc.expr, tc.ctx); got != tc.want {
				t.Errorf("Eval(%q) with %v = %v, want %v", tc.expr, tc.ctx, got, tc.want)
			}
		})
	}
}

// `!` swaps the branches: what the operand proves when true, its negation proves when false.
// Without it the catalogue has a hole an author falls into — `!(n == null)` is the same
// guard as `n != null`, and only one of them worked.
func TestLogicalNarrowing_NotSwapsTheBranches(t *testing.T) {
	for _, expr := range []string{
		`!(n == null) && n > 2`,
		`!(n != null) || n > 2`,
		`!(n == null) ? n + 1 : 0`,
		`!!(n != null) && n > 2`,
	} {
		t.Run(expr, func(t *testing.T) {
			if _, err := guardCtx.Infer(expr); err != nil {
				t.Fatalf("Infer(%q): %v", expr, err)
			}
		})
	}

	// The swap has to be exact, or `!` becomes a way to assert what was never proved.
	for _, expr := range []string{
		`!(n != null) && n > 2`, // the right runs when n IS null
		`!(n == null) || n > 2`, // || takes its right when the left is false: n IS null
	} {
		t.Run("refused: "+expr, func(t *testing.T) {
			inferErr(t, expr, guardCtx, "")
		})
	}
}

func TestLogicalNarrowing_NotRuntimeAgrees(t *testing.T) {
	for _, tc := range []struct {
		expr string
		ctx  map[string]any
		want any
	}{
		{`!(n == null) && n > 2`, map[string]any{"n": nil}, false},
		{`!(n == null) && n > 2`, map[string]any{"n": 5}, true},
		{`!(n != null) || n > 2`, map[string]any{"n": nil}, true},
		{`!(n != null) || n > 2`, map[string]any{"n": 1}, false},
	} {
		t.Run(tc.expr, func(t *testing.T) {
			if got := evalOK(t, tc.expr, tc.ctx); got != tc.want {
				t.Errorf("Eval(%q) with %v = %v, want %v", tc.expr, tc.ctx, got, tc.want)
			}
		})
	}
}

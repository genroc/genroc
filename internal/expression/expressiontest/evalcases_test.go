package expressiontest

import (
	"encoding/json"
	"genroc/internal/numeric"
	"reflect"
	"strings"
	"testing"

	"genroc/internal/expression"

	exprlib "github.com/expr-lang/expr"
)

// Fixtures and case runners for the runtime-evaluation suites (eval_edge_test.go,
// map_test.go, secret_test.go). Every runner takes named cases and runs each one
// as its own subtest, so a failure names the behaviour and `go test -run
// 'TestX/case_name'` runs it alone.

// edgeEnv is the runtime fixture for eval_edge_test.go; named apart from
// richCtx/mapEnv so parallel edits to the other eval files cannot collide with it.
var edgeEnv = map[string]any{
	"xs":      []any{map[string]any{"n": 1, "id": "a"}, map[string]any{"n": 2, "id": "b"}},
	"ys":      []any{10, 20},
	"one":     []any{map[string]any{"n": 7}},
	"empty":   []any{},
	"holes":   []any{nil, map[string]any{"n": 1}},
	"deep":    map[string]any{"b": map[string]any{"c": map[string]any{"d": "leaf"}}},
	"scalar":  map[string]any{"b": 5},
	"arrmid":  map[string]any{"b": []any{1, 2}},
	"strmid":  map[string]any{"b": "text"},
	"nullmid": map[string]any{"b": nil},
	"nul":     nil,
	"num":     5,
	"flt":     2.5,
	"i64":     int64(7),
	"f32":     float32(1.5),
	"str":     "abc",
	"no":      false,
	"obj":     map[string]any{"k": 1},
}

// edgeBoom is an identifier deliberately absent from edgeEnv: evaluating it is a
// hard error ("field not found"), so any expression that succeeds despite
// containing it proves the operand was never evaluated.
const edgeBoom = `edge_absent_root`

// ---- single assertions ----

// edgeOracle evaluates ours through Eval and oracle through real expr-lang and
// compares the two as JSON. A divergence here means the rewrite drifted from the
// semantics every existing definition was written against.
func edgeOracle(t *testing.T, ours, oracle string) {
	t.Helper()
	want, err := exprlib.Eval(oracle, edgeEnv)
	if err != nil {
		t.Fatalf("expr-lang oracle %q: %v", oracle, err)
	}
	got := evalOK(t, ours, edgeEnv)
	wj, _ := json.Marshal(want)
	gj, _ := json.Marshal(got)
	if string(wj) != string(gj) {
		t.Errorf("mismatch for %q\n  ours:      %s\n  expr-lang: %s", ours, gj, wj)
	}
}

// edgeExact asserts the exact result. Numbers compare by value — every numeric result is a
// json.Number carrying its exact decimal, so there is no int-vs-float64 distinction left;
// edgeDecimal covers the exact text. Non-numeric results compare structurally.
func edgeExact(t *testing.T, got, want any) {
	t.Helper()
	if _, wantNum := numeric.ToDecimal(want); wantNum {
		if !numeric.Equal(got, want) {
			t.Errorf("got %#v (%T), want %#v (%T)", got, got, want, want)
		}
		return
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %#v (%T), want %#v (%T)", got, got, want, want)
	}
}

// edgeJSON compares a container result against its JSON form, so nesting and
// null placement are checked without writing out []any/map[string]any literals.
func edgeJSON(t *testing.T, expr string, wantJSON string) {
	t.Helper()
	got := evalOK(t, expr, edgeEnv)
	gj, err := json.Marshal(got)
	if err != nil {
		t.Fatalf("marshal result of %q: %v", expr, err)
	}
	var want any
	if err := json.Unmarshal([]byte(wantJSON), &want); err != nil {
		t.Fatalf("invalid wantJSON %q: %v", wantJSON, err)
	}
	wj, _ := json.Marshal(want)
	if string(gj) != string(wj) {
		t.Errorf("Eval(%q) = %s, want %s", expr, gj, wj)
	}
}

// edgeDecimal asserts the exact decimal text of an arithmetic result. Arithmetic
// yields a json.Number carrying the exact value, so asserting the digits is a
// stronger check than the Go type ever was: the integer/number distinction now
// lives only in the type system, not in the runtime representation.
func edgeDecimal(t *testing.T, got any, want string) {
	t.Helper()
	n, ok := got.(json.Number)
	if !ok {
		t.Errorf("expected a json.Number, got %#v (%T)", got, got)
		return
	}
	if n.String() != want {
		t.Errorf("got %s, want %s", n, want)
	}
}

// edgeNull asserts that expr evaluates without error to null.
func edgeNull(t *testing.T, expr string) {
	t.Helper()
	if got := evalOK(t, expr, edgeEnv); got != nil {
		t.Errorf("Eval(%q) = %#v, want nil", expr, got)
	}
}

// edgeErrContains asserts that expr fails with a message containing want.
func edgeErrContains(t *testing.T, expr, want string) error {
	t.Helper()
	err := evalErr(t, expr, edgeEnv)
	if !strings.Contains(err.Error(), want) {
		t.Errorf("Eval(%q): error %v does not contain %q", expr, err, want)
	}
	return err
}

// ---- named case sweeps ----

// edgePair is one oracle case: a behaviour name, our syntax, and its expr-lang
// translation.
type edgePair struct{ name, ours, oracle string }

func edgeOracleAll(t *testing.T, cases []edgePair) {
	t.Helper()
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) { edgeOracle(t, tc.ours, tc.oracle) })
	}
}

// edgeCase is one expression under a behaviour name, for sweeps whose expected
// outcome is the same for every case (null, or an error).
type edgeCase struct{ name, expr string }

func edgeNullAll(t *testing.T, cases []edgeCase) {
	t.Helper()
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) { edgeNull(t, tc.expr) })
	}
}

func edgeErrAll(t *testing.T, cases []edgeCase) {
	t.Helper()
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) { evalErr(t, tc.expr, edgeEnv) })
	}
}

func edgeErrContainsAll(t *testing.T, cases []edgeCase, want string) {
	t.Helper()
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) { edgeErrContains(t, tc.expr, want) })
	}
}

// edgeNoPanicAll requires only "a value or an error, never a panic" per case.
func edgeNoPanicAll(t *testing.T, cases []edgeCase) {
	t.Helper()
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			defer func() {
				if r := recover(); r != nil {
					t.Errorf("Eval(%q) panicked: %v — want a value or an error", tc.expr, r)
				}
			}()
			if _, err := expression.Eval(tc.expr, edgeEnv); err != nil {
				t.Logf("Eval(%q) returned an error (acceptable): %v", tc.expr, err)
			}
		})
	}
}

// edgeValueCase is one expression and the exact value (type included) it must
// evaluate to.
type edgeValueCase struct {
	name, expr string
	want       any
}

func edgeExactAll(t *testing.T, cases []edgeValueCase) {
	t.Helper()
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) { edgeExact(t, evalOK(t, tc.expr, edgeEnv), tc.want) })
	}
}

// edgeDecCase is one arithmetic expression and the exact decimal text it must
// produce.
type edgeDecCase struct{ name, expr, want string }

func edgeDecimalAll(t *testing.T, cases []edgeDecCase) {
	t.Helper()
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) { edgeDecimal(t, evalOK(t, tc.expr, edgeEnv), tc.want) })
	}
}

// edgeJSONCase is one expression and the JSON form of the container it must
// produce.
type edgeJSONCase struct{ name, expr, wantJSON string }

func edgeJSONAll(t *testing.T, cases []edgeJSONCase) {
	t.Helper()
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) { edgeJSON(t, tc.expr, tc.wantJSON) })
	}
}

// ---- secret taint sweeps ----

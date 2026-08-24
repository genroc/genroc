package expressiontest

import (
	"testing"

	"genroc/internal/expression"
)

// Through separates a root the expression READS INTO from one it merely copies. A copied root
// keeps its references and reaches the next write unloaded; a root read through must be
// materialized, or the read finds a marker where the data should be.
//
// Direction matters and is not symmetric: over-reporting costs one load, under-reporting hands
// a marker to an operator. Every case below that expects `false` is a saved load; every case
// that expects `true` is a correctness requirement.
func TestRootsThrough_SeparatesReadingFromCopying(t *testing.T) {
	cases := []struct {
		expr    string
		through bool
		why     string
	}{
		// Copy positions: the value is placed somewhere, never inspected.
		{"outputs.x", false, "the whole expression is the value"},
		{"{a: outputs.x}", false, "an object value is placed, not read"},
		{"[outputs.x]", false, "an array item is placed, not read"},
		{"c ? outputs.x : outputs.y", false, "a branch becomes this node's value"},

		// Read-through positions: something inspects the value.
		{"outputs.x.y", true, "a field access walks into it"},
		{"outputs.x[0]", true, "an index walks into it"},
		{"outputs.x[k]", true, "a computed key walks into it"},
		{"outputs.x == 1", true, "an operator compares it"},
		{"!outputs.x", true, "a unary operator inspects it"},
		{"map(outputs.x, e => e)", true, "a function inspects its argument"},
		{"(c ? outputs.x : outputs.y).z", true, "the branch's value is walked into"},

		// A copy nested inside a read-through position is still only copied.
		{"{a: outputs.x, b: outputs.y.z}", true, "y is read through"},
	}
	for _, tc := range cases {
		t.Run(tc.expr, func(t *testing.T) {
			r, err := expression.RootRefs(tc.expr)
			if err != nil {
				t.Fatalf("RootRefs(%q): %v", tc.expr, err)
			}
			if len(r.Outputs) == 0 && !r.AllOutputs {
				t.Fatalf("RootRefs(%q) named no outputs root at all", tc.expr)
			}
			got := len(r.Through.Outputs) > 0 || r.Through.AllOutputs
			if got != tc.through {
				t.Errorf("through = %v, want %v (%s)", got, tc.through, tc.why)
			}
		})
	}
}

// The copy case must stay precise per task id: one output copied and another read through means
// only the second is loaded.
func TestRootsThrough_IsPerOutputId(t *testing.T) {
	r, err := expression.RootRefs("{a: outputs.x, b: outputs.y.z}")
	if err != nil {
		t.Fatalf("RootRefs: %v", err)
	}
	if len(r.Outputs) != 2 {
		t.Fatalf("Outputs = %v, want both named", r.Outputs)
	}
	if len(r.Through.Outputs) != 1 || r.Through.Outputs[0] != "y" {
		t.Errorf("Through.Outputs = %v, want just [y]: x is copied, so its object never loads", r.Through.Outputs)
	}
}

// error.code must not pay for error.data. This was the intent of the ErrorData root all along;
// the old resolver defeated it by materializing every child of the map it walked.
func TestRootsThrough_ErrorCodeDoesNotPullTheBody(t *testing.T) {
	r, err := expression.RootRefs("error.code == \"http.500\"")
	if err != nil {
		t.Fatalf("RootRefs: %v", err)
	}
	if r.Through.ErrorData {
		t.Errorf("reading error.code marked the body as read through; the body is the one part of error that can be externalized")
	}

	r, err = expression.RootRefs("error.data.detail")
	if err != nil {
		t.Fatalf("RootRefs: %v", err)
	}
	if !r.Through.ErrorData {
		t.Errorf("reading error.data.detail must load the body, or the read finds a marker")
	}
}

// A bare namespace narrows nothing, so it must be treated as read through in a position that
// inspects it. Under-reporting here would export a marker.
func TestRootsThrough_BareNamespaceInAnOperationIsReadThrough(t *testing.T) {
	r, err := expression.RootRefs("outputs == null")
	if err != nil {
		t.Fatalf("RootRefs: %v", err)
	}
	if !r.AllOutputs || !r.Through.AllOutputs {
		t.Errorf("AllOutputs=%v Through.AllOutputs=%v, want both: nothing narrows what a bare `outputs` wants", r.AllOutputs, r.Through.AllOutputs)
	}
}

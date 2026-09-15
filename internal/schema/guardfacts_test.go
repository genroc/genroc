package schema

import (
	"testing"

	"genroc/internal/expression/syntax"
)

// The catalogue as a table, independent of what either consumer does with it: the
// expression inferrer turns a fact into a narrowed Schema, the cross-task pass will turn the
// same fact into a symbolic non-null (specs/guard-narrowing.md). Both rest on this walk
// being exact, and the false branch is where the unsound mistakes live.
func TestGuardFacts_Catalogue(t *testing.T) {
	// fact renders one fact as "path==lit" / "path!=lit" so a table row reads as the claim.
	render := func(facts []guardFact) []string {
		out := make([]string, 0, len(facts))
		for _, f := range facts {
			op := "!="
			if f.equal {
				op = "=="
			}
			lit := "?"
			switch n := f.lit.(type) {
			case *syntax.NullNode:
				lit = "null"
			case *syntax.IntNode:
				lit = "int"
			case *syntax.StringNode:
				lit = "str"
			case *syntax.BoolNode:
				lit = "bool"
			default:
				_ = n
			}
			out = append(out, renderPath(f.steps)+op+lit)
		}
		return out
	}

	for _, tc := range []struct {
		expr         string
		wantT, wantF []string
	}{
		{`a != null`, []string{"a!=null"}, []string{"a==null"}},
		{`a == null`, []string{"a==null"}, []string{"a!=null"}},
		{`null != a`, []string{"a!=null"}, []string{"a==null"}}, // literal on the left
		{`a.b.c != null`, []string{"a.b.c!=null"}, []string{"a.b.c==null"}},
		{`a == 1`, []string{"a==int"}, []string{"a!=int"}},

		// A conjunction proves both when true and neither when false; `||` mirrors it.
		{`a != null && b != null`, []string{"a!=null", "b!=null"}, nil},
		{`a == null || b == null`, nil, []string{"a!=null", "b!=null"}},
		{`a != null && b != null && c != null`, []string{"a!=null", "b!=null", "c!=null"}, nil},

		// `!` swaps exactly, and nests.
		{`!(a == null)`, []string{"a!=null"}, []string{"a==null"}},
		{`!!(a != null)`, []string{"a!=null"}, []string{"a==null"}},
		{`!(a != null && b != null)`, nil, []string{"a!=null", "b!=null"}},

		// Nothing to say: no literal, a computed subject, or an operator outside the set.
		{`a != b`, nil, nil},
		{`a > 1`, nil, nil},
		{`a + 1 == 2`, nil, nil},
		{`(a != null) == true`, nil, nil},
	} {
		t.Run(tc.expr, func(t *testing.T) {
			node, err := syntax.Parse(tc.expr)
			if err != nil {
				t.Fatalf("parse: %v", err)
			}
			gotT, gotF := guardFacts(node)
			assertFacts(t, "when true", render(gotT), tc.wantT)
			assertFacts(t, "when false", render(gotF), tc.wantF)
		})
	}
}

func assertFacts(t *testing.T, branch string, got, want []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("%s: got %v, want %v", branch, got, want)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Errorf("%s[%d]: got %q, want %q (full: %v)", branch, i, got[i], want[i], got)
		}
	}
}

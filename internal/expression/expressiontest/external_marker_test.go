package expressiontest

import (
	"strings"
	"testing"

	"genroc/internal/expression"
	"genroc/internal/model"
)

// An unresolved reference is legal to COPY and illegal to OPERATE ON. Copying is how an
// untouched value reaches the next write without ever being loaded; comparing, indexing or
// rendering one would compute a plausible wrong answer, so it must fail instead. A hit here
// means expression.Roots called a read a copy -- the analysis is the bug, and this is the
// alarm that says so. specs/lazy-context.md.
func TestExternalMarker_CopyIsLegalAndOperatingOnOneFails(t *testing.T) {
	marker := &model.ObjectRef{Ref: "deadbeefdeadbeefdeadbeefdeadbeef", Size: 4096}
	env := map[string]any{
		"outputs": map[string]any{"a": marker},
		"input":   map[string]any{"n": float64(1)},
	}

	t.Run("copying it through is legal", func(t *testing.T) {
		got, err := expression.Eval("outputs.a", env)
		if err != nil {
			t.Fatalf("copying a reference must not fail: %v", err)
		}
		if got != any(marker) {
			t.Errorf("got %#v, want the marker itself -- a copy must not materialize it", got)
		}
	})

	for _, tc := range []struct {
		name string
		expr string
	}{
		{"field access", "outputs.a.field"},
		{"index", "outputs.a[0]"},
		{"computed key", "outputs.a[input.n]"},
		{"comparison", "outputs.a == 1"},
		{"unary", "!outputs.a"},
		{"function argument", "map(outputs.a, e => e)"},
	} {
		t.Run(tc.name+" fails loudly", func(t *testing.T) {
			_, err := expression.Eval(tc.expr, env)
			if err == nil {
				t.Fatalf("%q returned a value for an unloaded reference; a wrong answer here is worse than an error, because nothing downstream can tell", tc.expr)
			}
			if !strings.Contains(err.Error(), "deadbeef") {
				t.Errorf("error does not name the object: %v", err)
			}
		})
	}
}

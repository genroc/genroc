package schematest

import (
	"fmt"
	"testing"

	"genroc/internal/schema"
)

// mustAssumed parses a JSON schema and wraps it as-is (subset tests supply
// already-flat fixtures; some deliberately exercise pre-normalized shapes).
func mustAssumed(t *testing.T, src string) schema.Schema {
	t.Helper()
	raw, err := schema.Parse([]byte(src))
	if err != nil {
		t.Fatalf("invalid schema: %v", err)
	}
	return raw.AssumeNormalized()
}

func assertSubset(t *testing.T, subJSON, superJSON string, want bool) {
	t.Helper()
	sub, super := mustAssumed(t, subJSON), mustAssumed(t, superJSON)
	got := sub.IsSubset(super)
	if got != want {
		t.Errorf("IsSubset(%s, %s) = %v, want %v", subJSON, superJSON, got, want)
	}
	// Every rejection must be able to say why. This runs over every case in every
	// subset_*_test.go table, so a new failure branch returning a bare false is caught here
	// rather than surfacing as a diagnostic with nothing in it.
	if !got {
		breaks := sub.ExplainSubset(super)
		if len(breaks) == 0 {
			t.Errorf("IsSubset(%s, %s) = false with no break reported", subJSON, superJSON)
		}
		// EVERY break, not the first: the walk keeps going past a failure while tracing, so a
		// branch that reports a half-empty one is only reachable through the later positions.
		for _, brk := range breaks {
			if brk.Sub == "" || brk.Super == "" {
				t.Errorf("break at %q is half-empty (%q → %q); a message needs both sides",
					brk.Path, brk.Sub, brk.Super)
			}
		}
	}
}

// explainOne is for a pair holding exactly ONE difference, which is what every case below
// is built to be. ExplainSubset reports them all, so a case that grows a second break has
// to say so here rather than quietly having its first one asserted.
func explainOne(t *testing.T, sub, super schema.Schema) *schema.SubsetBreak {
	t.Helper()
	breaks := sub.ExplainSubset(super)
	if len(breaks) != 1 {
		paths := make([]string, 0, len(breaks))
		for _, b := range breaks {
			paths = append(paths, fmt.Sprintf("%s (%s)", b.Path, b.Kind))
		}
		t.Fatalf("got %d breaks %v, want exactly 1", len(breaks), paths)
	}
	return breaks[0]
}

// assertEquivalent checks that a ⊆ b and b ⊆ a, proving semantic equivalence.
func assertEquivalent(t *testing.T, aJSON, bJSON string, want bool) {
	t.Helper()
	a, b := mustAssumed(t, aJSON), mustAssumed(t, bJSON)
	aSubB := a.IsSubset(b)
	bSubA := b.IsSubset(a)
	got := aSubB && bSubA
	if got != want {
		t.Errorf("equivalent(%s, %s): a⊆b=%v b⊆a=%v, want equivalent=%v", aJSON, bJSON, aSubB, bSubA, want)
	}
}

package schematest

import (
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
	// subset_*_test.go table, so a new failure branch that returns a bare false is caught
	// here rather than surfacing as a diagnostic with nothing in it — which is what the
	// separate explainer used to produce, and why it was deleted.
	if !got {
		brk := sub.ExplainSubset(super)
		switch {
		case brk == nil:
			t.Errorf("IsSubset(%s, %s) = false with no break reported", subJSON, superJSON)
		case brk.Sub == "" || brk.Super == "":
			t.Errorf("break at %q is half-empty (%q → %q); a message needs both sides",
				brk.Path, brk.Sub, brk.Super)
		}
	}
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

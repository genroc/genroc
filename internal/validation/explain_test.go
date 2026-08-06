package validation

import (
	"testing"

	"genroc/internal/schema"
)

// The explainer decides a SECOND time whether an absent nullable property is a break —
// isSubset answers it for the verdict, `explain` answers it again to word the message. A
// drift between them is silent: the verdict stays right while the message sends a reader
// after a property the check tolerated, or hides the one that actually broke.

func explainSchema(t *testing.T, src string) schema.Schema {
	t.Helper()
	raw, err := schema.Parse([]byte(src))
	if err != nil {
		t.Fatalf("parse: %v\n%s", err, src)
	}
	s, err := raw.Normalize()
	if err != nil {
		t.Fatalf("normalize: %v\n%s", err, src)
	}
	return s
}

// Each spelling of "nullable" sits beside a property that genuinely broke, so the explainer
// must walk past the tolerated one and name the other. A `$ref` is the case that decides
// whether the message's rule resolves through $defs the way the relation's does.
func TestReadExplainer_WalksPastEveryGapTheRelationTolerates(t *testing.T) {
	spellings := []struct {
		name     string
		nullable string
	}{
		{"a type list", `{"type":["number","null"]}`},
		{"a union", `{"oneOf":[{"type":"number"},{"type":"null"}]}`},
		{"a definition it points at", `{"$ref":"#/$defs/maybe"}`},
		{"a nested union", `{"anyOf":[{"anyOf":[{"type":"number"},{"type":"null"}]},{"type":"string"}]}`},
	}
	sub := explainSchema(t, `{"type":"object","properties":{"broken":{"type":"string"}},"required":["broken"]}`)
	for _, sp := range spellings {
		t.Run(sp.name, func(t *testing.T) {
			super := explainSchema(t, `{"$defs":{"maybe":{"type":["number","null"]}},
				"type":"object","properties":{"broken":{"type":"number"},"tolerated":`+sp.nullable+`},
				"required":["broken","tolerated"]}`)
			if got, want := readExplainer.explain("", sub, super, 0), "broken: string → number"; got != want {
				t.Errorf("got %q, want %q — the message must name the break the verdict is about", got, want)
			}
		})
	}
}

// Which explainer relaxes is decided by whether anything conforms the value afterwards, so
// the same pair must read differently on the two paths: nothing re-validates a stored
// context, while a waiting parent conforms a child's output against its result_schema.
func TestExplainers_TheOutputContractStaysStrictWhereAReadRelaxes(t *testing.T) {
	produced := explainSchema(t, `{"type":"object","properties":{"id":{"type":"string"}},"required":["id"]}`)
	expected := explainSchema(t, `{"type":"object","properties":{"id":{"type":"string"},
		"note":{"type":["string","null"]}},"required":["id","note"]}`)

	if got := readExplainer.explain("", produced, expected, 0); got != "" {
		t.Errorf("a reader cannot tell an absent key from a stored null, got %q", got)
	}
	if got, want := contractExplainer.explain("output", produced, expected, 0), "output.note: newly required field"; got != want {
		t.Errorf("got %q, want %q — collect conforms this value, and a conform rejects the absence", got, want)
	}
}

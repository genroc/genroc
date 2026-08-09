package validation

import (
	"testing"

	"genroc/internal/schema"
)

// The explainer no longer decides anything — the relation reports its own break and this
// only words it, so a message cannot name a property the verdict tolerated. What these
// tests pin is the half that is still a choice: WHICH break the relation surfaces when
// several properties are in play, and how a swapped check reads.

// explained joins the two halves the way the report renderer does (genctl prints
// `path: message`), so these assert the line an operator actually reads. The halves travel
// apart all the way to Issue.Path — see TestExplain_PathIsAFieldNotParsedFromTheMessage.
func explained(e explainer, sub, super schema.Schema) string {
	path, msg := e.explain(sub, super)
	if path == "" {
		return msg
	}
	return path + ": " + msg
}

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
func TestReadExplainer_NamesTheBreakPastEveryGapTheRelationTolerates(t *testing.T) {
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
			if got, want := explained(storedExplainer, sub, super), "broken: string → number"; got != want {
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

	if got := explained(storedExplainer, produced, expected); got != "" {
		t.Errorf("a reader cannot tell an absent key from a stored null, got %q", got)
	}
	// "no longer guaranteed", not "newly required field": this check runs new ⊆ old, so the
	// message is written from the reader's side — the old output promised `note` and the new
	// one does not (specs/compat-command.md §3a). The path is the relation's own, rooted at
	// the compared schema; the address the issue is filed under is a separate field.
	if got, want := explained(contractExplainer, produced, expected), "note: no longer guaranteed"; got != want {
		t.Errorf("got %q, want %q — collect conforms this value, and a conform rejects the absence, "+
			"so the output contract must report where a read would not", got, want)
	}
}

// A constraint break used to render as its own type on BOTH sides of the arrow: the
// explainer walked separately from the relation and had no vocabulary for bounds, so a
// maxItems break came out "array<string> → array<string>" — a message asserting that
// nothing changed. The relation names what it actually rejected.
func TestExplain_NamesTheConstraintThatBroke(t *testing.T) {
	for _, tc := range []struct{ name, sub, super, want string }{
		{"maxItems", `{"type":"array","items":{"type":"string"}}`,
			`{"type":"array","items":{"type":"string"},"maxItems":2}`,
			"no maxItems is not accepted where maxItems 2 is expected"},
		{"maxLength", `{"type":"string"}`, `{"type":"string","maxLength":2}`,
			"no maxLength is not accepted where maxLength 2 is expected"},
		{"minimum", `{"type":"number"}`, `{"type":"number","minimum":3}`,
			"no minimum is not accepted where minimum 3 is expected"},
		{"enum", `{"type":"string"}`, `{"type":"string","enum":["a","b"]}`,
			`any string is not accepted where enum ["a", "b"] is expected`},
		{"under a property", `{"type":"object","properties":{"rows":{"type":"array","items":{"type":"string"}}},"required":["rows"]}`,
			`{"type":"object","properties":{"rows":{"type":"array","items":{"type":"string"},"maxItems":2}},"required":["rows"]}`,
			"rows: no maxItems → maxItems 2"},
		{"through an array", `{"type":"object","properties":{"a":{"type":"array","items":{"type":"object","properties":{"id":{"type":"number"}},"required":["id"]}}},"required":["a"]}`,
			`{"type":"object","properties":{"a":{"type":"array","items":{"type":"object","properties":{"id":{"type":"integer"}},"required":["id"]}}},"required":["a"]}`,
			"a[].id: number → integer"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := explained(explainer{}, explainSchema(t, tc.sub), explainSchema(t, tc.super))
			if got != tc.want {
				t.Errorf("got %q, want %q — a break whose two sides read the same tells a reader nothing", got, tc.want)
			}
		})
	}
}

// The explainer used to stop at four levels: it re-walked without the relation's cycle
// guard, so it had to invent a bound, and anything deeper reported as the outermost pair
// with two identical descriptions. The relation's guard terminates on the schema graph
// itself, so nesting is not a limit.
func TestExplain_ReachesBreaksBelowTheOldDepthCap(t *testing.T) {
	nest := func(leaf string) string {
		s := leaf
		for _, k := range []string{"d", "c", "b", "a"} {
			s = `{"type":"object","properties":{"` + k + `":` + s + `},"required":["` + k + `"]}`
		}
		return s
	}
	sub := nest(`{"type":"object","properties":{"e":{"type":"number"}},"required":["e"]}`)
	super := nest(`{"type":"object","properties":{"e":{"type":"integer"}},"required":["e"]}`)
	got := explained(explainer{}, explainSchema(t, sub), explainSchema(t, super))
	if want := "a.b.c.d.e: number → integer"; got != want {
		t.Errorf("got %q, want %q — a break five levels down must still be locatable", got, want)
	}
}

// The path is a FIELD, carried from the relation to Issue.Path untouched. It used to be
// recovered by searching the rendered message for the first ": " and rejecting any prefix
// containing a space — so a property whose name has one silently lost its path, and the
// report could not say where the break was.
func TestExplain_PathIsAFieldNotParsedFromTheMessage(t *testing.T) {
	sub := explainSchema(t, `{"type":"object","properties":{"my key":{"type":"string"}},"required":["my key"]}`)
	super := explainSchema(t, `{"type":"object","properties":{"my key":{"type":"integer"}},"required":["my key"]}`)
	path, msg := (explainer{}).explain(sub, super)
	if path != "my key" {
		t.Errorf("path = %q, want %q — a name with a space is still a location", path, "my key")
	}
	if msg != "string → integer" {
		t.Errorf("message = %q, want %q — the message says what, the path says where", msg, "string → integer")
	}
}

package validation

import (
	"strings"
	"testing"

	"genroc/internal/schema"
)

// The relation reports its own breaks; these pin the half that is still a choice — which
// breaks reach a reader, and how a swapped check reads.

// explained joins each break the way genctl prints it. The "; " between breaks is this
// helper's, so a case can assert once; the report gives each its own line.
func explained(e explainer, sub, super schema.Schema) string {
	var lines []string
	for _, f := range e.explain(sub, super) {
		if f.path == "" {
			lines = append(lines, f.msg)
			continue
		}
		lines = append(lines, f.path+": "+f.msg)
	}
	return strings.Join(lines, "; ")
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

// Each spelling of "nullable" sits beside a property that genuinely broke; the `$ref` case
// is what decides whether the wording resolves through $defs the way the relation does.
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

// One pair, two paths: nothing re-validates a stored context, while a waiting parent
// conforms a child's output against its result_schema.
func TestExplainers_TheOutputContractStaysStrictWhereAReadRelaxes(t *testing.T) {
	produced := explainSchema(t, `{"type":"object","properties":{"id":{"type":"string"}},"required":["id"]}`)
	expected := explainSchema(t, `{"type":"object","properties":{"id":{"type":"string"},
		"note":{"type":["string","null"]}},"required":["id","note"]}`)

	if got := explained(storedExplainer, produced, expected); got != "" {
		t.Errorf("a reader cannot tell an absent key from a stored null, got %q", got)
	}
	// "no longer guaranteed", not "newly required field": the check runs new ⊆ old, so the
	// message is written from the reader's side (specs/compat-command.md §3a).
	if got, want := explained(contractExplainer, produced, expected), "note: no longer guaranteed"; got != want {
		t.Errorf("got %q, want %q — collect conforms this value, and a conform rejects the absence, "+
			"so the output contract must report where a read would not", got, want)
	}
}

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

// The relation's cycle guard terminates on the schema graph, so nesting is not a limit.
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

// The path is a FIELD, carried to Issue.Path untouched — never recovered by splitting the
// rendered message, which silently dropped any property name containing a space.
func TestExplain_PathIsAFieldNotParsedFromTheMessage(t *testing.T) {
	sub := explainSchema(t, `{"type":"object","properties":{"my key":{"type":"string"}},"required":["my key"]}`)
	super := explainSchema(t, `{"type":"object","properties":{"my key":{"type":"integer"}},"required":["my key"]}`)
	found := (explainer{}).explain(sub, super)
	if len(found) != 1 {
		t.Fatalf("got %d findings, want 1: %v", len(found), found)
	}
	if found[0].path != "my key" {
		t.Errorf("path = %q, want %q — a name with a space is still a location", found[0].path, "my key")
	}
	if found[0].msg != "string → integer" {
		t.Errorf("message = %q, want %q — the message says what, the path says where", found[0].msg, "string → integer")
	}
}

// The expected order is the relation's walk: `required` before the properties, and the
// properties sorted — the schema's shape rather than the map's.
func TestExplain_NamesEveryBreakNotTheFirst(t *testing.T) {
	sub := explainSchema(t, `{"type":"object","properties":{
		"amount":{"type":"number"},"currency":{"type":"string"},"note":{"type":"string"}},
		"required":["amount","note"]}`)
	super := explainSchema(t, `{"type":"object","properties":{
		"amount":{"type":"string"},"currency":{"type":"string"},"note":{"type":"string"}},
		"required":["amount","currency","note"]}`)

	want := "currency: newly required field; amount: number → string"
	if got := explained(explainer{}, sub, super); got != want {
		t.Errorf("got %q, want %q", got, want)
	}
	// The tolerated property must not appear beside them: reporting everything is not the
	// same as reporting everything that differs, and `note` differs in neither schema.
	if got := explained(explainer{}, sub, super); strings.Contains(got, "note") {
		t.Errorf("got %q — a property that fits is not a finding", got)
	}
}

// Each arm of a union is TRIED, and under `asStored` the stripped-null retry is a second
// such alternative — describing a schema nobody wrote.
func TestExplain_AlternativesLeaveNoBreakBehind(t *testing.T) {
	sub := explainSchema(t, `{"type":"object","properties":{"v":{"type":"boolean"}},"required":["v"]}`)
	super := explainSchema(t, `{"type":"object","properties":{
		"v":{"oneOf":[{"type":"string"},{"type":"number"},{"type":"integer"}]}},"required":["v"]}`)
	want := "v: boolean → any of [string, number, integer]"
	if got := explained(explainer{}, sub, super); got != want {
		t.Errorf("got %q, want %q — one break for the union, not one per arm tried", got, want)
	}

	// The stored relation's stripped-null retry, made to fail (the underlying types differ
	// too), so what must be reported is the pair as written, once.
	nullable := explainSchema(t, `{"type":"object","properties":{"v":{"type":["string","null"]}}}`)
	strict := explainSchema(t, `{"type":"object","properties":{"v":{"type":"integer"}}}`)
	if got, want := explained(storedExplainer, nullable, strict), "v: string|null → integer"; got != want {
		t.Errorf("got %q, want %q — the stripped re-check is an alternative, not a second finding", got, want)
	}
}

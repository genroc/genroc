package schematest

import (
	"encoding/json"
	"strings"
	"testing"

	"genroc/internal/schema"
)

// Conformed is the TYPE of what the fill produces, and this file holds it to the fill directly:
// for every case a value of `inf` is run through Validate(v, ConformToSchemaExactly) against
// `dec`, and the result must validate STRICTLY against Conformed(inf, dec). A type that the real
// output does not satisfy is a hover, a CLI answer and a generated TypeScript type that are all
// wrong together, which is the one way the unification could be worse than the drift it replaced.

type conformedCase struct {
	name string
	inf  string
	dec  string
	// values of `inf`, each run through the fill and checked against the result type
	values []string
	// what the result must summarise as — the readable claim, beside the checked one
	summary string
}

var conformedCases = []conformedCase{
	{
		name:    "a top-type declaration leaves the inferred type alone",
		inf:     `{"type":"object","properties":{"who":{"type":"string"}},"required":["who"]}`,
		dec:     `{"description":"opaque here"}`,
		values:  []string{`{"who":"x"}`},
		summary: "object{who}",
	},
	{
		name:    "an optional null is removed, and the key becomes may-be-absent",
		inf:     `{"type":"object","properties":{"d":{"type":["number","null"]}},"required":["d"]}`,
		dec:     `{"type":"object","properties":{"d":{"type":"number"}}}`,
		values:  []string{`{"d":null}`, `{"d":5}`},
		summary: "object{d?}",
	},
	{
		name:    "a nullable target keeps the null and the key",
		inf:     `{"type":"object","properties":{"d":{"type":["number","null"]}},"required":["d"]}`,
		dec:     `{"type":"object","properties":{"d":{"type":["number","null"]}}}`,
		values:  []string{`{"d":null}`, `{"d":5}`},
		summary: "object{d}",
	},
	{
		name:   "a required nullable the value never sets is written in as null",
		inf:    `{"type":"object","properties":{"a":{"type":"number"}},"required":["a"]}`,
		dec:    `{"type":"object","properties":{"a":{"type":"number"},"seen":{"type":["string","null"]}},"required":["a","seen"]}`,
		values: []string{`{"a":1}`},
		// `=null` is Summary's spelling for a property whose type is exactly null.
		summary: "object{a, seen=null}",
	},
	{
		name:    "an optional declared property the value never sets stays out of the type",
		inf:     `{"type":"object","properties":{"a":{"type":"number"}},"required":["a"]}`,
		dec:     `{"type":"object","properties":{"a":{"type":"number"},"b":{"type":"string"}}}`,
		values:  []string{`{"a":1}`},
		summary: "object{a}",
	},
	{
		name:    "the inferred side stays the more precise one on a leaf",
		inf:     `{"type":"object","properties":{"n":{"type":"integer"}},"required":["n"]}`,
		dec:     `{"type":"object","properties":{"n":{"type":"number"}},"required":["n"]}`,
		values:  []string{`{"n":3}`},
		summary: "object{n}",
	},
	{
		name:    "nested: the repair applies one level down",
		inf:     `{"type":"object","properties":{"c":{"type":"object","properties":{"d":{"type":["number","null"]}},"required":["d"]}},"required":["c"]}`,
		dec:     `{"type":"object","properties":{"c":{"type":"object","properties":{"d":{"type":"number"}}}}}`,
		values:  []string{`{"c":{"d":null}}`, `{"c":{"d":2}}`},
		summary: "object{c}",
	},
	{
		name:    "array items are refined without any removal",
		inf:     `{"type":"array","items":{"type":"object","properties":{"n":{"type":"integer"}},"required":["n"]}}`,
		dec:     `{"type":"array","items":{"type":"object","properties":{"n":{"type":"number"}}}}`,
		values:  []string{`[{"n":1},{"n":2}]`},
		summary: "array<object{n}>",
	},
	{
		name:    "a nullable inferred object against a non-nullable declared one keeps its own null",
		inf:     `{"type":"object","properties":{"c":{"anyOf":[{"type":"object","properties":{"d":{"type":"number"}},"required":["d"]},{"type":"null"}]}},"required":["c"]}`,
		dec:     `{"type":"object","properties":{"c":{"type":"object","properties":{"d":{"type":"number"}}}}}`,
		values:  []string{`{"c":null}`, `{"c":{"d":1}}`},
		summary: "object{c?}",
	},
}

func TestConformedIsTheTypeTheFillProduces(t *testing.T) {
	for _, c := range conformedCases {
		t.Run(c.name, func(t *testing.T) {
			inf, dec := mustSchema(t, c.inf), mustSchema(t, c.dec)
			if !inf.ConformsExactlyTo(dec) {
				t.Fatalf("premise gone: the relation refuses this pair, so the fill never runs on it: %v",
					breakStrings(inf.ExplainConformsExactlyTo(dec)))
			}
			got := inf.Conformed(dec)
			if err := got.CheckDoc(); err != nil {
				t.Fatalf("Conformed is not a valid schema document: %v", err)
			}
			if s := got.Summary(); s != c.summary {
				t.Fatalf("summary %q, want %q", s, c.summary)
			}
			// The result must still be something the declaration accepts, or it describes a
			// value the far side would refuse.
			if !got.IsSubset(dec) {
				t.Fatalf("Conformed does not fit the declaration it was conformed to: %v",
					breakStrings(got.ExplainSubset(dec)))
			}
			for _, v := range c.values {
				filled, err := dec.Validate(decodeValue(t, v), schema.ConformToSchemaExactly)
				if err != nil {
					t.Fatalf("the fill failed on %s: %v", v, err)
				}
				if _, err := got.Validate(filled); err != nil {
					t.Fatalf("the fill produced %s from %s, and Conformed refuses it: %v",
						valueJSON(t, filled), v, err)
				}
			}
		})
	}
}

// The description is why an imported schema is worth having, and it is the ONE thing taken from
// the declaration rather than derived from the inferred side. Its `default` must never come with
// it: that keyword beside `required` is not a valid document, which is how the spread broke once.
func TestConformedCarriesProseAndNeverADefault(t *testing.T) {
	inf := mustSchema(t, `{"type":"object","properties":{"ms":{"type":"integer"}},"required":["ms"]}`)
	dec := mustSchema(t, `{"type":"object","properties":{"ms":{"type":"integer","default":5000,"description":"a budget"}},
	                       "description":"the request"}`)
	got := inf.Conformed(dec)
	raw, _ := json.Marshal(got)
	if !strings.Contains(string(raw), `"a budget"`) || !strings.Contains(string(raw), `"the request"`) {
		t.Fatalf("prose was not carried: %s", raw)
	}
	if strings.Contains(string(raw), "default") {
		t.Fatalf("a default was carried into a required property: %s", raw)
	}
	if err := got.CheckDoc(); err != nil {
		t.Fatalf("not a valid document: %v", err)
	}
}

// A reference the declaration does not reach inside stays a reference, which is what keeps a
// recursive inferred type finite.
func TestConformedLeavesAnUnconstrainedRefSymbolic(t *testing.T) {
	inf := mustSchema(t, `{"$defs":{"N":{"type":"object","properties":{"v":{"type":"number"},
	                       "next":{"anyOf":[{"$ref":"#/$defs/N"},{"type":"null"}]}},"required":["v"]}},
	                       "type":"object","properties":{"root":{"$ref":"#/$defs/N"}},"required":["root"]}`)
	dec := mustSchema(t, `{"type":"object","properties":{"root":{"description":"a list"}}}`)
	got := inf.Conformed(dec)
	raw, _ := json.Marshal(got)
	if !strings.Contains(string(raw), `"$ref"`) {
		t.Fatalf("the recursive reference was materialised: %s", raw)
	}
	if err := got.CheckDoc(); err != nil {
		t.Fatalf("not a valid document: %v", err)
	}
}

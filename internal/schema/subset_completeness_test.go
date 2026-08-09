package schema

import (
	"reflect"
	"testing"
)

// The subset relation is NOT driven by the walk table (it compares two schemas at once), so
// nothing mechanical tells you a keyword was left out of it. minItems/maxItems were left
// out: an unbounded array counted as a subset of a capped one, permissively, and every
// existing test passed because the corpus is example-based and organised by feature area —
// subset_scalar_test.go owned minimum/minLength, subset_array_test.go owned item TYPES, and
// item COUNTS belonged to neither file's idea of itself.
//
// This closes that by construction: the field list is checked against node by reflection, so
// a new field fails here until someone either proves the relation reads it or records why it
// cannot narrow anything.

type narrowing struct {
	sub, super string
	// build replaces the JSON pair for a field no parser will produce: allOf is rejected as
	// user input on purpose, so its case is built the one way the code builds it (FlattenNamed
	// bundling refs). Without this the relation's allOf branches would have no test at all.
	build func() (sub, super *node)
}

// narrowingCases pairs a sub that omits a constraint with a super that imposes it. Each must
// make IsSubset say no — that is what "the relation reads this field" means.
var narrowingCases = map[string]narrowing{
	"Type":                 {sub: `{"type":["string","number"]}`, super: `{"type":"string"}`},
	"Properties":           {sub: `{"type":"object","properties":{"a":{"type":"number"}}}`, super: `{"type":"object","properties":{"a":{"type":"string"}}}`},
	"Required":             {sub: `{"type":"object"}`, super: `{"type":"object","properties":{"a":{"type":"string"}},"required":["a"]}`},
	"AdditionalProperties": {sub: `{"type":"object","properties":{"x":{"type":"number"}}}`, super: `{"type":"object","additionalProperties":{"type":"string"}}`},
	"Items":                {sub: `{"type":"array","items":{"type":"number"}}`, super: `{"type":"array","items":{"type":"string"}}`},
	"OneOf":                {sub: `{"type":"string"}`, super: `{"oneOf":[{"type":"number"},{"type":"boolean"}]}`},
	"AnyOf":                {sub: `{"type":"string"}`, super: `{"anyOf":[{"type":"number"},{"type":"boolean"}]}`},
	"AllOf": {build: func() (*node, *node) {
		return &node{Type: SchemaType{"string"}}, &node{AllOf: []*node{{Type: SchemaType{"number"}}}}
	}},
	"Enum":      {sub: `{"type":"string"}`, super: `{"type":"string","enum":["a"]}`},
	"Minimum":   {sub: `{"type":"integer"}`, super: `{"type":"integer","minimum":3}`},
	"Maximum":   {sub: `{"type":"integer"}`, super: `{"type":"integer","maximum":3}`},
	"MinLength": {sub: `{"type":"string"}`, super: `{"type":"string","minLength":3}`},
	"MaxLength": {sub: `{"type":"string"}`, super: `{"type":"string","maxLength":3}`},
	"MinItems":  {sub: `{"type":"array","items":{"type":"string"}}`, super: `{"type":"array","items":{"type":"string"},"minItems":3}`},
	"MaxItems":  {sub: `{"type":"array","items":{"type":"string"}}`, super: `{"type":"array","items":{"type":"string"},"maxItems":3}`},
	"Ref":       {sub: `{"type":"string"}`, super: `{"$ref":"#/$defs/N","$defs":{"N":{"type":"number"}}}`},
}

// cannotNarrow records why a field admits no such pair. Each entry is a claim, not a skip.
var cannotNarrow = map[string]string{
	"Description": "an annotation; canonicalizeNode strips it and description_test.go pins that it cannot move the relation",
	"Defs":        "a namespace, reachable only through a $ref — Ref carries the case",
	"Anchor":      "normalization resolves it away; no node reaching the relation carries one",
	"ID":          "same as Anchor — a resource marker consumed by normalize",
	"Default":     "no effect on which values are VALID; IsSubsetAsStored reads it as a presence guarantee, pinned by subset_stored_test.go",
	"Secret":      "a redaction annotation, orthogonal to validity",
	"pending":     "a solver sentinel, never present in a stored or compared schema",
}

func TestSubsetReadsEveryNarrowingField(t *testing.T) {
	rt := reflect.TypeOf(node{})
	for i := range rt.NumField() {
		name := rt.Field(i).Name
		_, narrows := narrowingCases[name]
		_, exempt := cannotNarrow[name]
		switch {
		case narrows && exempt:
			t.Errorf("%s is listed both as narrowing and as unable to narrow; it is one or the other", name)
		case !narrows && !exempt:
			t.Errorf("node.%s is in neither list: decide whether the subset relation must read it "+
				"and add a pair to narrowingCases, or record in cannotNarrow why it cannot narrow "+
				"the value set. Nothing else in the suite will notice if subset.go ignores it — "+
				"that is how minItems/maxItems became a permissive wrong answer.", name)
		}
	}

	for name, tc := range narrowingCases {
		t.Run(name, func(t *testing.T) {
			sub, super := mustNode(t, tc.sub), mustNode(t, tc.super)
			if tc.build != nil {
				sub, super = tc.build()
			}
			if isSubset(sub, super) {
				t.Fatalf("IsSubset ignores %s: %s counted as a subset of %s, so a schema change "+
					"narrowing it would be reported compatible and then reject data at conform time",
					name, tc.sub, tc.super)
			}
			ok, brk := subsetExplain(sub, super, subsetMode{}, true)
			if ok || brk == nil {
				t.Fatalf("%s: rejected with no break to report", name)
			}
		})
	}
}

func mustNode(t *testing.T, src string) *node {
	t.Helper()
	if src == "" {
		return nil
	}
	raw, err := Parse([]byte(src))
	if err != nil {
		t.Fatalf("parse %s: %v", src, err)
	}
	s, err := raw.Normalize()
	if err != nil {
		t.Fatalf("normalize %s: %v", src, err)
	}
	return s.n
}

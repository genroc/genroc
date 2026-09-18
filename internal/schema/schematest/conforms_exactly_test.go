package schematest

import (
	"testing"

	"genroc/internal/schema"
)

// ConformsExactlyTo and ConformToSchemaExactly are a pair, and this file is that claim: the
// relation must accept exactly the gaps the conform closes. A relation that tolerates more
// promises a definition the checker blessed whose conform then fails at runtime — and that
// conform is an ASSERTION (specs/declared-slot-schemas.md §4), so its failure would be read as
// a bug in the type system rather than as the relation being too generous.
//
// It differs from IsSubsetAsStored in one rule, and `TestConformsExactlyToHasNoDefaultsRule` is
// the whole reason it is a separate relation.

// pairing is one accepted gap written as the whole transformation: the two schemas, a value of
// `sub`, and the exact value the conform must hand back.
type pairing struct {
	name string
	sub  string
	sup  string
	in   string
	want string
}

var pairings = []pairing{
	{
		name: "an optional null is removed, which is the case the feature exists for",
		sub:  `{"type":"object","properties":{"discount":{"type":["number","null"]}}}`,
		sup:  `{"type":"object","properties":{"discount":{"type":"number"}}}`,
		in:   `{"discount":null}`,
		want: `{}`,
	},
	{
		name: "the same value non-null passes through untouched",
		sub:  `{"type":"object","properties":{"discount":{"type":["number","null"]}}}`,
		sup:  `{"type":"object","properties":{"discount":{"type":"number"}}}`,
		in:   `{"discount":5}`,
		want: `{"discount":5}`,
	},
	{
		name: "a nullable target KEEPS the null — both states are valid, so there is nothing to reconcile",
		sub:  `{"type":"object","properties":{"note":{"type":["string","null"]}}}`,
		sup:  `{"type":"object","properties":{"note":{"type":["string","null"]}}}`,
		in:   `{"note":null}`,
		want: `{"note":null}`,
	},
	{
		name: "an absent required nullable is written in — the other half of the same conform",
		sub:  `{"type":"object","properties":{"seen":{"type":["string","null"]}}}`,
		sup:  `{"type":"object","properties":{"seen":{"type":["string","null"]}},"required":["seen"]}`,
		in:   `{}`,
		want: `{"seen":null}`,
	},
	{
		name: "an optional property the value never set stays absent",
		sub:  `{"type":"object","properties":{"a":{"type":"string"},"b":{"type":"string"}}}`,
		sup:  `{"type":"object","properties":{"a":{"type":"string"},"b":{"type":"string"}}}`,
		in:   `{"a":"x"}`,
		want: `{"a":"x"}`,
	},
	{
		name: "one level down",
		sub:  `{"type":"object","properties":{"cfg":{"type":"object","properties":{"n":{"type":["number","null"]}}}}}`,
		sup:  `{"type":"object","properties":{"cfg":{"type":"object","properties":{"n":{"type":"number"}}}}}`,
		in:   `{"cfg":{"n":null}}`,
		want: `{"cfg":{}}`,
	},
	{
		name: "inside an array element",
		sub: `{"type":"array","items":{"type":"object",
		       "properties":{"n":{"type":["number","null"]}}}}`,
		sup: `{"type":"array","items":{"type":"object",
		       "properties":{"n":{"type":"number"}}}}`,
		in:   `[{"n":null},{"n":2}]`,
		want: `[{},{"n":2}]`,
	},
	{
		name: "through a $ref on both sides",
		sub: `{"$defs":{"T":{"type":"object","properties":{"n":{"type":["number","null"]}}}},
		       "type":"object","properties":{"t":{"$ref":"#/$defs/T"}}}`,
		sup: `{"$defs":{"T":{"type":"object","properties":{"n":{"type":"number"}}}},
		       "type":"object","properties":{"t":{"$ref":"#/$defs/T"}}}`,
		in:   `{"t":{"n":null}}`,
		want: `{"t":{}}`,
	},
	{
		name: "a recursive definition still terminates and still repairs",
		sub: `{"$defs":{"N":{"type":"object","properties":{"v":{"type":["number","null"]},
		       "next":{"anyOf":[{"$ref":"#/$defs/N"},{"type":"null"}]}}}},"$ref":"#/$defs/N"}`,
		sup: `{"$defs":{"N":{"type":"object","properties":{"v":{"type":"number"},
		       "next":{"anyOf":[{"$ref":"#/$defs/N"},{"type":"null"}]}}}},"$ref":"#/$defs/N"}`,
		in:   `{"v":null,"next":{"v":1,"next":null}}`,
		want: `{"next":{"next":null,"v":1}}`,
	},
}

func TestConformsExactlyToAcceptsWhatTheConformCloses(t *testing.T) {
	for _, c := range pairings {
		t.Run(c.name, func(t *testing.T) {
			sub, sup := mustSchema(t, c.sub), mustSchema(t, c.sup)
			if !sub.ConformsExactlyTo(sup) {
				t.Fatalf("relation refused a gap the conform closes — a declaration that works would be rejected\nbreaks: %v",
					breakStrings(sub.ExplainConformsExactlyTo(sup)))
			}
			got, err := sup.Validate(decodeValue(t, c.in), schema.ConformToSchemaExactly)
			if err != nil {
				t.Fatalf("relation accepted but the conform failed: %v", err)
			}
			if have := valueJSON(t, got); have != c.want {
				t.Fatalf("conform produced %s, want %s", have, c.want)
			}
		})
	}
}

// The non-stripping half of the pairing, asserted separately because it is the property the
// `closed` rule exists to guarantee and a key-set comparison is what would catch its absence.
// A key may vanish only where its value was null — that is the documented repair and nothing else.
func TestConformsExactlyToNeverStripsANonNullKey(t *testing.T) {
	for _, c := range pairings {
		t.Run(c.name, func(t *testing.T) {
			sup := mustSchema(t, c.sup)
			in := decodeValue(t, c.in)
			got, err := sup.Validate(in, schema.ConformToSchemaExactly)
			if err != nil {
				t.Fatalf("conform: %v", err)
			}
			assertNoNonNullKeyLost(t, in, got, "")
		})
	}
}

func assertNoNonNullKeyLost(t *testing.T, in, out any, path string) {
	t.Helper()
	switch src := in.(type) {
	case map[string]any:
		dst, ok := out.(map[string]any)
		if !ok {
			t.Fatalf("%s: an object conformed to %T", path, out)
		}
		for k, v := range src {
			at := path + "." + k
			if _, present := dst[k]; !present {
				if v != nil {
					t.Fatalf("%s: a non-null key was stripped — the conform may only remove a null", at)
				}
				continue
			}
			assertNoNonNullKeyLost(t, v, dst[k], at)
		}
	case []any:
		dst, ok := out.([]any)
		if !ok {
			t.Fatalf("%s: an array conformed to %T", path, out)
		}
		if len(dst) != len(src) {
			t.Fatalf("%s: the array changed length, %d to %d", path, len(src), len(dst))
		}
		for i := range src {
			assertNoNonNullKeyLost(t, src[i], dst[i], path+"[]")
		}
	}
}

// The `closed` rule. Every row is a key the conform would silently DROP, which at a slot whose
// schema the author wrote is a deletion of something they typed.
func TestConformsExactlyToRefusesAnUndeclaredKey(t *testing.T) {
	cases := []struct{ name, sub, sup string }{
		{
			name: "at the top level",
			sub:  `{"type":"object","properties":{"page":{"type":"number"},"pgae":{"type":"number"}}}`,
			sup:  `{"type":"object","properties":{"page":{"type":"number"}}}`,
		},
		{
			name: "one level down",
			sub:  `{"type":"object","properties":{"c":{"type":"object","properties":{"a":{"type":"string"},"b":{"type":"string"}}}}}`,
			sup:  `{"type":"object","properties":{"c":{"type":"object","properties":{"a":{"type":"string"}}}}}`,
		},
		{
			name: "inside an array element",
			sub:  `{"type":"array","items":{"type":"object","properties":{"a":{"type":"string"},"b":{"type":"string"}}}}`,
			sup:  `{"type":"array","items":{"type":"object","properties":{"a":{"type":"string"}}}}`,
		},
		{
			name: "behind a $ref",
			sub: `{"$defs":{"T":{"type":"object","properties":{"a":{"type":"string"},"b":{"type":"string"}}}},
			       "type":"object","properties":{"t":{"$ref":"#/$defs/T"}}}`,
			sup: `{"$defs":{"T":{"type":"object","properties":{"a":{"type":"string"}}}},
			       "type":"object","properties":{"t":{"$ref":"#/$defs/T"}}}`,
		},
		{
			name: "inside a recursive definition",
			sub: `{"$defs":{"N":{"type":"object","properties":{"v":{"type":"number"},"extra":{"type":"string"},
			       "next":{"anyOf":[{"$ref":"#/$defs/N"},{"type":"null"}]}}}},"$ref":"#/$defs/N"}`,
			sup: `{"$defs":{"N":{"type":"object","properties":{"v":{"type":"number"},
			       "next":{"anyOf":[{"$ref":"#/$defs/N"},{"type":"null"}]}}}},"$ref":"#/$defs/N"}`,
		},
		{
			// The arm that is silent when missing: an OPEN MAP carries keys no schema names,
			// so the strip stays reachable and §4's assertion is quietly false without it.
			name: "an open-map sub against a closed super",
			sub:  `{"type":"object","additionalProperties":{"type":"string"}}`,
			sup:  `{"type":"object","properties":{"a":{"type":"string"}}}`,
		},
		{
			name: "an open map nested inside a declared property",
			sub:  `{"type":"object","properties":{"c":{"type":"object","additionalProperties":{"type":"string"}}}}`,
			sup:  `{"type":"object","properties":{"c":{"type":"object","properties":{"a":{"type":"string"}}}}}`,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			sub, sup := mustSchema(t, c.sub), mustSchema(t, c.sup)
			if sub.ConformsExactlyTo(sup) {
				t.Fatal("accepted a key the conform would silently strip")
			}
			breaks := sub.ExplainConformsExactlyTo(sup)
			if len(breaks) == 0 {
				t.Fatal("refused with no break — a reader is told nothing")
			}
			if !hasKind(breaks, schema.BreakUndeclared) {
				t.Fatalf("refused for the wrong reason: %v", breakStrings(breaks))
			}
		})
	}
}

// An open super has SAID what its extras mean, so the closed rule does not apply to it and the
// existing additionalProperties check does the work. Without this the relation would refuse
// every imported schema that types its extras.
func TestConformsExactlyToLeavesAnOpenSuperAlone(t *testing.T) {
	sub := mustSchema(t, `{"type":"object","properties":{"a":{"type":"string"},"b":{"type":"string"}}}`)
	sup := mustSchema(t, `{"type":"object","properties":{"a":{"type":"string"},"b":{"type":"string"}},"additionalProperties":{"type":"string"}}`)
	if !sub.ConformsExactlyTo(sup) {
		t.Fatalf("refused an open super: %v", breakStrings(sub.ExplainConformsExactlyTo(sup)))
	}
}

// The one rule that makes this a separate relation rather than IsSubsetAsStored with a flag.
// IsSubsetAsStored reads a default on the SUB side as a guarantee of presence, because whatever
// produced the data filled it. Nothing filled ours, and ConformToSchemaExactly does NOT fill
// defaults — so borrowing that relation would bless a value the conform then rejects.
func TestConformsExactlyToHasNoDefaultsRule(t *testing.T) {
	sub := mustSchema(t, `{"type":"object","properties":{"a":{"type":"string","default":"x"}}}`)
	sup := mustSchema(t, `{"type":"object","properties":{"a":{"type":"string"}},"required":["a"]}`)

	if !sub.IsSubsetAsStored(sup) {
		t.Fatal("premise gone: IsSubsetAsStored no longer accepts a defaulted property as present")
	}
	if sub.ConformsExactlyTo(sup) {
		t.Fatal("borrowed IsSubsetAsStored's defaults rule, which this conform does not perform")
	}
	// And the conform is why: it leaves the absent optional absent, so super's `required` fails.
	if _, err := sup.Validate(map[string]any{}, schema.ConformToSchemaExactly); err == nil {
		t.Fatal("the conform filled a default in Exactly mode — the relation above is now the wrong shape")
	}
}

// The case nothing can repair: absence is not valid either, so the conform has no move and the
// relation must refuse rather than leave it to fail at runtime.
func TestConformsExactlyToRefusesANullItCannotRemove(t *testing.T) {
	sub := mustSchema(t, `{"type":"object","properties":{"n":{"type":["number","null"]}},"required":["n"]}`)
	sup := mustSchema(t, `{"type":"object","properties":{"n":{"type":"number"}},"required":["n"]}`)
	if sub.ConformsExactlyTo(sup) {
		t.Fatal("accepted a required null the conform cannot remove")
	}
	if _, err := sup.Validate(decodeValue(t, `{"n":null}`), schema.ConformToSchemaExactly); err == nil {
		t.Fatal("the conform repaired a required null — the relation above is now too strict")
	}
}

// An unknown is refused on the value side, and the reason is the assertion: {} can be anything
// at runtime, so admitting it is the one thing that would give this conform a LEGITIMATE
// failure. specs/declared-slot-schemas.md §4.
func TestConformsExactlyToRefusesAnUnknown(t *testing.T) {
	sub := mustSchema(t, `{}`)
	sup := mustSchema(t, `{"type":"object","properties":{"a":{"type":"string"}}}`)
	if sub.ConformsExactlyTo(sup) {
		t.Fatal("admitted an unknown — this relation is not NarrowsTo and must not become it")
	}
	if !sub.NarrowsTo(sup) {
		t.Fatal("premise gone: NarrowsTo no longer admits an unknown, so the contrast above is stale")
	}
}

func hasKind(breaks []*schema.SubsetBreak, kind schema.SubsetBreakKind) bool {
	for _, b := range breaks {
		if b.Kind == kind {
			return true
		}
	}
	return false
}

func breakStrings(breaks []*schema.SubsetBreak) []string {
	out := make([]string, 0, len(breaks))
	for _, b := range breaks {
		out = append(out, b.Path+": "+b.Sub+" -> "+b.Super+" ("+string(b.Kind)+")")
	}
	return out
}

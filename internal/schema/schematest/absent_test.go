package schematest

import (
	"encoding/json"
	"testing"

	"genroc/internal/schema"
)

// IsSubsetAbsentAsNull and FillAbsentAsNull are a pair — one decides a gap is closable, the
// other closes it — so the tests are organised around that claim (gaps, refusals, fill cases,
// properties). A relation tolerating more than the fill can close promises a failing upgrade.

func mustSchema(t *testing.T, src string) schema.Schema {
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

func decodeValue(t *testing.T, src string) any {
	t.Helper()
	var v any
	if err := json.Unmarshal([]byte(src), &v); err != nil {
		t.Fatalf("unmarshal: %v\n%s", err, src)
	}
	return v
}

func valueJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return string(b)
}

// ── the closable gaps ─────────────────────────────────────────────────────────

// gap is a pair the strict relation refuses and the forgiving one accepts, plus a value
// shaped like the old side that the fill must carry across.
type gap struct {
	name  string
	old   string
	new   string
	value string
}

var gaps = []gap{
	{
		name:  "a nullable property became required",
		old:   `{"type":"object","properties":{"a":{"type":"string"}},"required":["a"]}`,
		new:   `{"type":"object","properties":{"a":{"type":"string"},"b":{"type":["number","null"]}},"required":["a","b"]}`,
		value: `{"a":"x"}`,
	},
	{
		name: "nullable spelled as a oneOf rather than a type list",
		old:  `{"type":"object","properties":{"a":{"type":"string"}},"required":["a"]}`,
		new: `{"type":"object","properties":{"a":{"type":"string"},
		       "b":{"oneOf":[{"type":"number"},{"type":"null"}]}},"required":["a","b"]}`,
		value: `{"a":"x"}`,
	},
	{
		name: "nullable only through the definition it points at",
		old:  `{"type":"object","properties":{"a":{"type":"string"}},"required":["a"]}`,
		new: `{"$defs":{"maybe":{"type":["number","null"]}},
		       "type":"object","properties":{"a":{"type":"string"},"b":{"$ref":"#/$defs/maybe"}},
		       "required":["a","b"]}`,
		value: `{"a":"x"}`,
	},
	{
		name:  "the property is plain null",
		old:   `{"type":"object","properties":{"a":{"type":"string"}},"required":["a"]}`,
		new:   `{"type":"object","properties":{"a":{"type":"string"},"b":{"type":"null"}},"required":["a","b"]}`,
		value: `{"a":"x"}`,
	},
	{
		name:  "the old side declared it, merely optional",
		old:   `{"type":"object","properties":{"a":{"type":"string"},"b":{"type":["number","null"]}},"required":["a"]}`,
		new:   `{"type":"object","properties":{"a":{"type":"string"},"b":{"type":["number","null"]}},"required":["a","b"]}`,
		value: `{"a":"x"}`,
	},
	{
		name: "several at once",
		old:  `{"type":"object","properties":{"a":{"type":"string"}},"required":["a"]}`,
		new: `{"type":"object","properties":{"a":{"type":"string"},"b":{"type":["number","null"]},
		       "c":{"type":["string","null"]}},"required":["a","b","c"]}`,
		value: `{"a":"x"}`,
	},
	{
		name: "nested one level down",
		old: `{"type":"object","properties":{"inner":{"type":"object","properties":{"a":{"type":"string"}},
		      "required":["a"]}},"required":["inner"]}`,
		new: `{"type":"object","properties":{"inner":{"type":"object","properties":{"a":{"type":"string"},
		      "b":{"type":["string","null"]}},"required":["a","b"]}},"required":["inner"]}`,
		value: `{"inner":{"a":"x"}}`,
	},
	{
		name: "three levels down",
		old: `{"type":"object","properties":{"l1":{"type":"object","properties":{"l2":{"type":"object",
		      "properties":{"a":{"type":"string"}},"required":["a"]}},"required":["l2"]}},"required":["l1"]}`,
		new: `{"type":"object","properties":{"l1":{"type":"object","properties":{"l2":{"type":"object",
		      "properties":{"a":{"type":"string"},"b":{"type":["number","null"]}},"required":["a","b"]}},
		      "required":["l2"]}},"required":["l1"]}`,
		value: `{"l1":{"l2":{"a":"x"}}}`,
	},
	{
		name:  "inside array items",
		old:   `{"type":"array","items":{"type":"object","properties":{"a":{"type":"string"}},"required":["a"]}}`,
		new:   `{"type":"array","items":{"type":"object","properties":{"a":{"type":"string"},"b":{"type":["boolean","null"]}},"required":["a","b"]}}`,
		value: `[{"a":"x"},{"a":"y"}]`,
	},
	{
		name: "inside an array of arrays",
		old:  `{"type":"array","items":{"type":"array","items":{"type":"object","properties":{"a":{"type":"string"}},"required":["a"]}}}`,
		new: `{"type":"array","items":{"type":"array","items":{"type":"object","properties":{"a":{"type":"string"},
		      "b":{"type":["number","null"]}},"required":["a","b"]}}}`,
		value: `[[{"a":"x"}],[{"a":"y"},{"a":"z"}]]`,
	},
	{
		name:  "through a $ref at the root",
		old:   `{"$defs":{"t":{"type":"object","properties":{"a":{"type":"string"}},"required":["a"]}},"$ref":"#/$defs/t"}`,
		new:   `{"$defs":{"t":{"type":"object","properties":{"a":{"type":"string"},"b":{"type":["number","null"]}},"required":["a","b"]}},"$ref":"#/$defs/t"}`,
		value: `{"a":"x"}`,
	},
	{
		name: "an open map's value type",
		old: `{"type":"object","additionalProperties":{"type":"object","properties":{"a":{"type":"string"}},
		      "required":["a"]}}`,
		new: `{"type":"object","additionalProperties":{"type":"object","properties":{"a":{"type":"string"},
		      "b":{"type":["string","null"]}},"required":["a","b"]}}`,
		value: `{"k1":{"a":"x"},"k2":{"a":"y"}}`,
	},
	{
		name: "inside a union variant",
		old: `{"anyOf":[{"type":"object","properties":{"a":{"type":"string"}},"required":["a"]},
		      {"type":"null"}]}`,
		new: `{"anyOf":[{"type":"object","properties":{"a":{"type":"string"},"b":{"type":["number","null"]}},
		      "required":["a","b"]},{"type":"null"}]}`,
		value: `{"a":"x"}`,
	},
	{
		name: "a recursive schema",
		old: `{"$defs":{"n":{"type":"object","properties":{"v":{"type":"string"},
		      "next":{"oneOf":[{"$ref":"#/$defs/n"},{"type":"null"}]}},"required":["v"]}},"$ref":"#/$defs/n"}`,
		new: `{"$defs":{"n":{"type":"object","properties":{"v":{"type":"string"},"tag":{"type":["string","null"]},
		      "next":{"oneOf":[{"$ref":"#/$defs/n"},{"type":"null"}]}},"required":["v","tag"]}},"$ref":"#/$defs/n"}`,
		value: `{"v":"a","next":{"v":"b","next":null}}`,
	},
	{
		name:  "the old side declared it NON-nullable, merely optional",
		old:   `{"type":"object","properties":{"a":{"type":"string"},"b":{"type":"number"}},"required":["a"]}`,
		new:   `{"type":"object","properties":{"a":{"type":"string"},"b":{"type":["number","null"]}},"required":["a","b"]}`,
		value: `{"a":"x"}`,
	},
	{
		name: "nullable only at the end of a chain of definitions",
		old:  `{"type":"object","properties":{"a":{"type":"string"}},"required":["a"]}`,
		new: `{"$defs":{"maybe":{"$ref":"#/$defs/inner"},"inner":{"type":["number","null"]}},
		       "type":"object","properties":{"a":{"type":"string"},"b":{"$ref":"#/$defs/maybe"}},
		       "required":["a","b"]}`,
		value: `{"a":"x"}`,
	},
	{
		name: "nullable inside a nested union, which a one-level scan misses",
		old:  `{"type":"object","properties":{"a":{"type":"string"}},"required":["a"]}`,
		new: `{"type":"object","properties":{"a":{"type":"string"},
		       "b":{"anyOf":[{"anyOf":[{"type":"number"},{"type":"null"}]},{"type":"string"}]}},
		       "required":["a","b"]}`,
		value: `{"a":"x"}`,
	},
	{
		name: "nullable found by walking a reference cycle",
		old:  `{"type":"object","properties":{"a":{"type":"string"}},"required":["a"]}`,
		new: `{"$defs":{"cyc":{"anyOf":[{"$ref":"#/$defs/cyc"},{"type":"null"}]}},
		       "type":"object","properties":{"a":{"type":"string"},"b":{"$ref":"#/$defs/cyc"}},
		       "required":["a","b"]}`,
		value: `{"a":"x"}`,
	},
	{
		name: "the whole object became one variant of a union",
		old:  `{"type":"object","properties":{"a":{"type":"string"}},"required":["a"]}`,
		new: `{"anyOf":[{"type":"object","properties":{"a":{"type":"string"},"b":{"type":["number","null"]}},
		       "required":["a","b"]},{"type":"string"}]}`,
		value: `{"a":"x"}`,
	},
	{
		name: "an open map on the new side against declared keys on the old",
		old: `{"type":"object","properties":{"k1":{"type":"object","properties":{"a":{"type":"string"}},
		      "required":["a"]}},"required":["k1"]}`,
		new: `{"type":"object","additionalProperties":{"type":"object","properties":{"a":{"type":"string"},
		      "b":{"type":["string","null"]}},"required":["a","b"]}}`,
		value: `{"k1":{"a":"x"}}`,
	},
}

// The forgiving relation must accept every gap — and the strict one must refuse it, or the
// case proves nothing about the relaxation.
func TestAbsentAsNull_RelationAcceptsEveryClosableGap(t *testing.T) {
	for _, g := range gaps {
		t.Run(g.name, func(t *testing.T) {
			old, new := mustSchema(t, g.old), mustSchema(t, g.new)
			if old.IsSubset(new) {
				t.Fatal("the strict relation already accepts this, so it is not a gap")
			}
			if !old.IsSubsetAbsentAsNull(new) {
				t.Fatal("the forgiving relation must accept a gap the fill can close")
			}
		})
	}
}

// ...and the fill must produce something the NEW schema accepts under a STRICT conform.
// This is the claim an upgrade makes, end to end, and the reason the pair may be trusted.
func TestAbsentAsNull_FillClosesEveryGapItAccepted(t *testing.T) {
	for _, g := range gaps {
		t.Run(g.name, func(t *testing.T) {
			old, new := mustSchema(t, g.old), mustSchema(t, g.new)
			value := decodeValue(t, g.value)

			if _, err := old.Validate(value); err != nil {
				t.Fatalf("the fixture value must be valid under the old schema: %v", err)
			}
			if _, err := new.Validate(value); err == nil {
				t.Fatal("the value already conforms to the new schema, so the fill is not being tested")
			}
			filled, err := new.Validate(value, schema.FillAbsentAsNull)
			if err != nil {
				t.Fatalf("the fill must close the gap the relation accepted: %v", err)
			}
			// ...and what it produced must satisfy a STRICT check. This is the claim an
			// upgrade makes, end to end.
			if _, err := new.Validate(filled); err != nil {
				t.Fatalf("the filled value does not conform strictly: %v", err)
			}
		})
	}
}

// ── what must stay refused ────────────────────────────────────────────────────

// The relaxation is exactly one rule, and everything else isSubset refuses must keep being
// refused — otherwise it stops being a subset check and becomes a hope.
func TestAbsentAsNull_RefusesWhatCannotBeClosed(t *testing.T) {
	cases := []struct {
		name string
		old  string
		new  string
	}{
		{
			name: "a required NON-nullable property, which nothing could invent",
			old:  `{"type":"object","properties":{"a":{"type":"string"}},"required":["a"]}`,
			new:  `{"type":"object","properties":{"a":{"type":"string"},"b":{"type":"number"}},"required":["a","b"]}`,
		},
		{
			name: "a required name with no declared property, so no type to call nullable",
			old:  `{"type":"object","properties":{"a":{"type":"string"}},"required":["a"]}`,
			new:  `{"type":"object","properties":{"a":{"type":"string"}},"required":["a","b"]}`,
		},
		{
			name: "a type that changed, which the relaxation must not reach",
			old:  `{"type":"object","properties":{"a":{"type":"number"}},"required":["a"]}`,
			new:  `{"type":"object","properties":{"a":{"type":"string"}},"required":["a"]}`,
		},
		{
			name: "a nullable property whose non-null type is incompatible",
			old:  `{"type":"object","properties":{"a":{"type":"string"},"b":{"type":"string"}},"required":["a","b"]}`,
			new:  `{"type":"object","properties":{"a":{"type":"string"},"b":{"type":["number","null"]}},"required":["a","b"]}`,
		},
		{
			name: "one closable gap alongside one that is not",
			old:  `{"type":"object","properties":{"a":{"type":"string"}},"required":["a"]}`,
			new: `{"type":"object","properties":{"a":{"type":"string"},"b":{"type":["number","null"]},
			      "c":{"type":"number"}},"required":["a","b","c"]}`,
		},
		{
			name: "an array element type that changed",
			old:  `{"type":"array","items":{"type":"number"}}`,
			new:  `{"type":"array","items":{"type":"string"}}`,
		},
		{
			name: "an enum that narrowed",
			old:  `{"type":"string","enum":["a","b"]}`,
			new:  `{"type":"string","enum":["a"]}`,
		},
		{
			name: "a bound that tightened",
			old:  `{"type":"integer","minimum":0}`,
			new:  `{"type":"integer","minimum":5}`,
		},
		{
			name: "an unknown flowing into a typed slot, which only NarrowsTo admits",
			old:  `{}`,
			new:  `{"type":"object","properties":{"a":{"type":"string"}},"required":["a"]}`,
		},
		{
			name: "an old side that is not an object at all, so the relaxation is never reached",
			old:  `{"type":"string"}`,
			new:  `{"type":"object","properties":{"b":{"type":["number","null"]}},"required":["b"]}`,
		},
		{
			name: "one union variant that cannot fit, however closable the other is",
			old: `{"anyOf":[{"type":"object","properties":{"a":{"type":"string"}},"required":["a"]},
			      {"type":"number"}]}`,
			new: `{"type":"object","properties":{"a":{"type":"string"},"b":{"type":["number","null"]}},
			      "required":["a","b"]}`,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			old, new := mustSchema(t, c.old), mustSchema(t, c.new)
			if old.IsSubsetAbsentAsNull(new) {
				t.Fatal("this gap cannot be closed by filling a null, so it must stay refused")
			}
		})
	}
}

// A presence gap is closable exactly when the missing property's type admits null, and that
// question is answered TWICE — once by the relation, once by the fill. The two must give the
// same answer or the pair's promise breaks, so these pin the awkward spellings against both.
//
// One case is deliberately absent: a `required` name with no declared property. The relation
// refuses it (see the HasNull test below) while the fill walks declared properties only and
// never sees it — the relation being the stricter half, which is the harmless direction.
func TestAbsentAsNull_BothHalvesAgreeOnWhichNamesAreFillable(t *testing.T) {
	cases := []struct {
		name  string
		new   string
		value string
	}{
		{
			name:  "a plain non-nullable property, which nothing could invent",
			new:   `{"type":"object","properties":{"a":{"type":"string"},"b":{"type":"number"}},"required":["a","b"]}`,
			value: `{"a":"x"}`,
		},
		{
			name: "a nullability question that walks a reference cycle and finds no null",
			new: `{"$defs":{"cyc":{"anyOf":[{"$ref":"#/$defs/cyc"},{"type":"string"}]}},
			       "type":"object","properties":{"a":{"type":"string"},"b":{"$ref":"#/$defs/cyc"}},
			       "required":["a","b"]}`,
			value: `{"a":"x"}`,
		},
		{
			// The unknown does admit null, and neither half reads it that way. They AGREE,
			// which is the property that matters; teaching one alone would break the pair.
			name:  "a property typed as the unknown, which declares no null",
			new:   `{"type":"object","properties":{"a":{"type":"string"},"b":{}},"required":["a","b"]}`,
			value: `{"a":"x"}`,
		},
		{
			// A default is not a second way to close a gap. Teaching the fill to use one
			// without teaching the relation would make it close more than was accepted;
			// the reverse promises an upgrade that then fails to conform.
			name:  "a non-nullable property carrying a default that could have supplied it",
			new:   `{"type":"object","properties":{"a":{"type":"string"},"b":{"type":"number","default":7}},"required":["a","b"]}`,
			value: `{"a":"x"}`,
		},
	}
	old := mustSchema(t, `{"type":"object","properties":{"a":{"type":"string"}},"required":["a"]}`)
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			new := mustSchema(t, c.new)
			if old.IsSubsetAbsentAsNull(new) {
				t.Error("the relation accepted a gap the fill cannot close")
			}
			if _, err := new.Validate(decodeValue(t, c.value), schema.FillAbsentAsNull); err == nil {
				t.Error("the fill closed a gap the relation refuses, so the two have drifted apart")
			}
		})
	}
}

// Where a gap IS closable, the value written is null even if the property declares a
// default — and the runtime agrees: a default never reaches a REQUIRED property, because
// the absence is rejected before one is looked for. So the migration invents nothing the
// runtime would not have produced, which is the only reason writing null over a stated
// default is defensible.
func TestAbsentAsNull_FillWritesNullOverADeclaredDefault(t *testing.T) {
	s := mustSchema(t, `{"type":"object","properties":{"a":{"type":"string"},
		"b":{"type":["number","null"],"default":7}},"required":["a","b"]}`)
	absent := decodeValue(t, `{"a":"x"}`)

	filled, err := s.Validate(absent, schema.FillAbsentAsNull)
	if err != nil {
		t.Fatalf("the property admits null, so the gap is closable: %v", err)
	}
	if got := valueJSON(t, filled); got != `{"a":"x","b":null}` {
		t.Fatalf("got %s: the fill closes the gap the relation accepted, and it was accepted on nullability", got)
	}
	if _, err := s.Validate(absent); err == nil {
		t.Fatal("a default on a required property is inert under a strict conform too, or the two disagree about what absence means")
	}
}

// ── the fill on its own ───────────────────────────────────────────────────────

func TestAbsentAsNull_FillLeavesAcceptedValuesUntouched(t *testing.T) {
	cases := []struct {
		name   string
		schema string
		value  string
	}{
		{
			name:   "a value already carrying the key",
			schema: `{"type":"object","properties":{"a":{"type":"string"},"b":{"type":["number","null"]}},"required":["a","b"]}`,
			value:  `{"a":"x","b":7}`,
		},
		{
			name:   "a value already carrying an explicit null",
			schema: `{"type":"object","properties":{"a":{"type":"string"},"b":{"type":["number","null"]}},"required":["a","b"]}`,
			value:  `{"a":"x","b":null}`,
		},
		{
			name:   "an OPTIONAL nullable property, which was never demanded",
			schema: `{"type":"object","properties":{"a":{"type":"string"},"b":{"type":["number","null"]}},"required":["a"]}`,
			value:  `{"a":"x"}`,
		},
		{
			name:   "an optional property with a DEFAULT, which a strict check would fill",
			schema: `{"type":"object","properties":{"a":{"type":"string"},"b":{"type":"number","default":7}},"required":["a"]}`,
			value:  `{"a":"x"}`,
		},
		{
			name:   "undeclared keys, which a strict check strips and a migration must not",
			schema: `{"type":"object","properties":{"a":{"type":"string"}},"required":["a"]}`,
			value:  `{"a":"x","stale":"kept"}`,
		},
		{
			name:   "undeclared keys nested inside a declared one",
			schema: `{"type":"object","properties":{"a":{"type":"object","properties":{"b":{"type":"string"}},"required":["b"]}},"required":["a"]}`,
			value:  `{"a":{"b":"x","stale":1}}`,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := mustSchema(t, c.schema)
			value := decodeValue(t, c.value)
			before := valueJSON(t, value)
			filled, err := s.Validate(value, schema.FillAbsentAsNull)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if after := valueJSON(t, filled); after != before {
				t.Fatalf("value changed:\n before: %s\n after:  %s", before, after)
			}
		})
	}
}

// A migration that quietly handed back the value it was given would let an upgrade report
// success over data that does not fit the version it was moved to. So everything the fill
// cannot close is REPORTED, not passed through.
func TestAbsentAsNull_FillReportsWhatItCannotClose(t *testing.T) {
	cases := []struct {
		name   string
		schema string
		value  string
	}{
		{
			name:   "a required property that is not nullable, so there is nothing to invent",
			schema: `{"type":"object","properties":{"a":{"type":"string"},"b":{"type":"number"}},"required":["a","b"]}`,
			value:  `{"a":"x"}`,
		},
		{
			name:   "a scalar where the schema describes an object",
			schema: `{"type":"object","properties":{"a":{"type":"string"}},"required":["a"]}`,
			value:  `"not an object"`,
		},
		{
			name:   "an array where the schema describes an object",
			schema: `{"type":"object","properties":{"a":{"type":"string"}},"required":["a"]}`,
			value:  `[1,2]`,
		},
		{
			name:   "null where the schema does not admit it",
			schema: `{"type":"object","properties":{"b":{"type":["number","null"]}},"required":["b"]}`,
			value:  `null`,
		},
		{
			name:   "a value matching no union variant",
			schema: `{"anyOf":[{"type":"object","properties":{"x":{"type":"number"}},"required":["x"]},{"type":"string"}]}`,
			value:  `[1]`,
		},
		{
			name:   "a property whose type changed under it",
			schema: `{"type":"object","properties":{"a":{"type":"number"}},"required":["a"]}`,
			value:  `{"a":"still a string"}`,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := mustSchema(t, c.schema)
			if _, err := s.Validate(decodeValue(t, c.value), schema.FillAbsentAsNull); err == nil {
				t.Fatal("the fill cannot close this, so it must say so rather than pass the value through")
			}
		})
	}
}

// The zero Schema constrains nothing, so a value passes through it untouched.
func TestAbsentAsNull_FillOnTheZeroSchema(t *testing.T) {
	var zero schema.Schema
	got, err := zero.Validate(decodeValue(t, `{"a":1}`), schema.FillAbsentAsNull)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if valueJSON(t, got) != `{"a":1}` {
		t.Fatalf("got %s", valueJSON(t, got))
	}
}

// A union of objects must be filled through the variant the value belongs to, chosen by
// conforming the candidate — filling through all of them would inject keys from a variant
// the value does not match.
func TestAbsentAsNull_FillPicksTheMatchingUnionVariant(t *testing.T) {
	s := mustSchema(t, `{"anyOf":[
		{"type":"object","properties":{"kind":{"type":"string","enum":["a"]},"av":{"type":["number","null"]}},"required":["kind","av"]},
		{"type":"object","properties":{"kind":{"type":"string","enum":["b"]},"bv":{"type":["string","null"]}},"required":["kind","bv"]}]}`)

	filled, err := s.Validate(decodeValue(t, `{"kind":"b"}`), schema.FillAbsentAsNull)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := valueJSON(t, filled); got != `{"bv":null,"kind":"b"}` {
		t.Fatalf("the b-variant's key must be filled and the a-variant's left out, got %s", got)
	}
}

// ── properties that must hold across the whole corpus ─────────────────────────

// allSchemas is every schema this file declares, so the properties below are asserted over
// the corpus rather than a hand-picked few.
func allSchemas(t *testing.T) []schema.Schema {
	t.Helper()
	var out []schema.Schema
	for _, g := range gaps {
		out = append(out, mustSchema(t, g.old), mustSchema(t, g.new))
	}
	return out
}

// The forgiving relation must be strictly MORE permissive: everything the strict one
// accepts, it accepts too. A relaxation that accidentally refuses something is a regression
// no single case would catch.
func TestAbsentAsNull_IsStrictlyMorePermissive(t *testing.T) {
	all := allSchemas(t)
	for i, a := range all {
		for j, b := range all {
			if a.IsSubset(b) && !a.IsSubsetAbsentAsNull(b) {
				t.Errorf("schema[%d] ⊆ schema[%d] strictly, but the forgiving relation refuses it", i, j)
			}
		}
	}
}

// Filling is idempotent — a migration that has already run must be a no-op the second time,
// or an upgrade would stop being safe to repeat.
func TestAbsentAsNull_FillIsIdempotent(t *testing.T) {
	for _, g := range gaps {
		t.Run(g.name, func(t *testing.T) {
			s := mustSchema(t, g.new)
			once, err := s.Validate(decodeValue(t, g.value), schema.FillAbsentAsNull)
			if err != nil {
				t.Fatalf("first fill: %v", err)
			}
			twice, err := s.Validate(once, schema.FillAbsentAsNull)
			if err != nil {
				t.Fatalf("second fill: %v", err)
			}
			if a, b := valueJSON(t, once), valueJSON(t, twice); a != b {
				t.Fatalf("second fill changed the value:\n once:  %s\n twice: %s", a, b)
			}
		})
	}
}

// Filling never removes or rewrites: every key the value carried is still there, unchanged.
// This is what makes a migration built on it non-destructive.
func TestAbsentAsNull_FillOnlyEverAdds(t *testing.T) {
	for _, g := range gaps {
		t.Run(g.name, func(t *testing.T) {
			s := mustSchema(t, g.new)
			value := decodeValue(t, g.value)
			filled, err := s.Validate(value, schema.FillAbsentAsNull)
			if err != nil {
				t.Fatalf("fill: %v", err)
			}
			assertKeptEverything(t, value, filled, "")
		})
	}
}

func assertKeptEverything(t *testing.T, before, after any, path string) {
	t.Helper()
	switch b := before.(type) {
	case map[string]any:
		a, ok := after.(map[string]any)
		if !ok {
			t.Fatalf("%s: an object became %T", path, after)
		}
		for k, v := range b {
			got, present := a[k]
			if !present {
				t.Fatalf("%s.%s was dropped", path, k)
			}
			assertKeptEverything(t, v, got, path+"."+k)
		}
	case []any:
		a, ok := after.([]any)
		if !ok || len(a) != len(b) {
			t.Fatalf("%s: an array of %d became %T", path, len(b), after)
		}
		for i := range b {
			assertKeptEverything(t, b[i], a[i], path+"[]")
		}
	default:
		if valueJSON(t, before) != valueJSON(t, after) {
			t.Fatalf("%s: %s was rewritten to %s", path, valueJSON(t, before), valueJSON(t, after))
		}
	}
}

// A value that already conforms must still conform after filling — the fill may only close
// gaps, never open one.
func TestAbsentAsNull_FillPreservesValidity(t *testing.T) {
	for _, g := range gaps {
		t.Run(g.name, func(t *testing.T) {
			s := mustSchema(t, g.new)
			filled, err := s.Validate(decodeValue(t, g.value), schema.FillAbsentAsNull)
			if err != nil {
				t.Fatalf("fill: %v", err)
			}
			if _, err := s.Validate(filled); err != nil {
				t.Fatalf("filled value does not conform: %v", err)
			}
			refilled, err := s.Validate(filled, schema.FillAbsentAsNull)
			if err != nil {
				t.Fatalf("re-filling an already-conforming value failed: %v", err)
			}
			if _, err := s.Validate(refilled); err != nil {
				t.Fatalf("re-filling an already-conforming value broke it: %v", err)
			}
		})
	}
}

// HasNull is asked about the property at a name, and a map index that misses hands it the
// zero Schema. It used to panic on that, which made the ordinary "is this name nullable"
// question a crash — reachable from a schema that lists a `required` name it never declares.
func TestAbsentAsNull_HasNullOnTheZeroSchemaIsFalse(t *testing.T) {
	var zero schema.Schema
	if zero.HasNull() {
		t.Fatal("the zero Schema declares no type, so nothing admits null")
	}
	s := mustSchema(t, `{"type":"object","properties":{"a":{"type":"string"}},"required":["a","ghost"]}`)
	if s.Properties()["ghost"].HasNull() {
		t.Fatal("an undeclared property has no type to call nullable")
	}
}

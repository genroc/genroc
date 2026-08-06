package schematest

import (
	"encoding/json"
	"testing"

	"genroc/internal/schema"

	"github.com/xeipuuv/gojsonschema"
)

// Pins our validator's accept/reject decision to gojsonschema's across a broad corpus. NOT
// message text — and our output deliberately diverges (strips undeclared, fills defaults),
// neither of which changes validity. Intended divergences go in knownDivergence with a reason.

type diffCase struct {
	name   string
	schema string
	docs   []string // documents exercised against the schema
}

// knownDivergence records (schemaName, doc) pairs where our decision intentionally
// differs from gojsonschema, keyed as schemaName+"\x00"+doc.
var knownDivergence = map[string]string{}

func TestValidateDecisionMatchesGojsonschema(t *testing.T) {
	cases := []diffCase{
		{
			name:   "object_required_and_nullable",
			schema: `{"type":"object","properties":{"id":{"type":"integer"},"comment":{"type":["string","null"]}},"required":["id"]}`,
			docs: []string{
				`{"id":1}`,
				`{"id":1,"comment":null}`,
				`{"id":1,"comment":"hi"}`,
				`{"comment":"hi"}`,           // required missing
				`{"id":null}`,                // required non-nullable is null
				`{"id":1,"comment":42}`,      // nullable wrong type
				`{"id":1,"extra":"dropped"}`, // undeclared prop (both accept)
				`[]`,                         // wrong root type
				`"nope"`,
			},
		},
		{
			name:   "enum",
			schema: `{"type":"string","enum":["red","green","blue"]}`,
			docs:   []string{`"red"`, `"green"`, `"yellow"`, `1`, `null`},
		},
		{
			name:   "integer_vs_number",
			schema: `{"type":"integer"}`,
			docs:   []string{`5`, `5.0`, `5.5`, `"5"`, `true`, `null`},
		},
		{
			name:   "number_type",
			schema: `{"type":"number"}`,
			docs:   []string{`5`, `5.5`, `"5"`, `null`},
		},
		{
			name:   "numeric_range_inclusive",
			schema: `{"type":"integer","minimum":1,"maximum":10}`,
			docs:   []string{`1`, `10`, `0`, `11`, `5`},
		},
		{
			name:   "string_length",
			schema: `{"type":"string","minLength":2,"maxLength":4}`,
			docs:   []string{`"ab"`, `"abcd"`, `"a"`, `"abcde"`, `"héllo"`, `"hél"`},
		},
		{
			name:   "array_items_and_bounds",
			schema: `{"type":"array","items":{"type":"integer"},"minItems":1,"maxItems":2}`,
			docs:   []string{`[1]`, `[1,2]`, `[]`, `[1,2,3]`, `[1,"x"]`, `[1.5]`},
		},
		{
			name:   "nested_objects",
			schema: `{"type":"object","properties":{"user":{"type":"object","properties":{"name":{"type":"string"}},"required":["name"]}},"required":["user"]}`,
			docs: []string{
				`{"user":{"name":"al"}}`,
				`{"user":{"name":"al","x":1}}`, // nested extra (both accept)
				`{"user":{}}`,                  // nested required missing
				`{"user":{"name":5}}`,          // nested wrong type
				`{}`,                           // top required missing
			},
		},
		{
			name:   "oneof_disjoint_by_type",
			schema: `{"oneOf":[{"type":"string"},{"type":"integer"}]}`,
			docs:   []string{`"x"`, `3`, `true`, `null`, `1.5`},
		},
		{
			name:   "oneof_overlapping_matches_two",
			schema: `{"oneOf":[{"type":"string"},{"type":"string","minLength":2}]}`,
			docs:   []string{`"a"`, `"abc"`}, // "a": 1 match (valid); "abc": 2 matches (invalid)
		},
		{
			name:   "anyof",
			schema: `{"anyOf":[{"type":"string"},{"type":"integer"}]}`,
			docs:   []string{`"x"`, `3`, `true`, `1.5`},
		},
		{
			name:   "nullable_via_type_array",
			schema: `{"type":["string","null"]}`,
			docs:   []string{`"x"`, `null`, `1`, `[]`},
		},
		{
			name:   "boolean",
			schema: `{"type":"boolean"}`,
			docs:   []string{`true`, `false`, `1`, `"true"`, `null`},
		},
		{
			name:   "array_of_objects",
			schema: `{"type":"array","items":{"type":"object","properties":{"v":{"type":"integer"}},"required":["v"]}}`,
			docs: []string{
				`[{"v":1},{"v":2}]`,
				`[{"v":1},{"w":2}]`, // second missing required v
				`[{"v":"x"}]`,       // wrong type
				`[]`,                // empty ok (no minItems)
			},
		},
		{
			name:   "oneof_with_null_branch",
			schema: `{"oneOf":[{"type":"object","properties":{"a":{"type":"string"}},"required":["a"]},{"type":"null"}]}`,
			docs:   []string{`null`, `{"a":"x"}`, `{"a":1}`, `{}`, `"x"`, `{"a":"x","b":9}`},
		},
		{
			name:   "multi_type_scalar",
			schema: `{"type":["integer","string"]}`,
			docs:   []string{`1`, `"x"`, `true`, `null`, `1.5`, `[]`},
		},
		{
			name:   "empty_schema_accepts_anything",
			schema: `{}`,
			docs:   []string{`1`, `"x"`, `null`, `true`, `[]`, `{"a":1}`, `[1,{"b":2}]`},
		},
		{
			name:   "enum_mixed_types_no_declared_type",
			schema: `{"enum":[1,"two",null,true]}`,
			docs:   []string{`1`, `"two"`, `null`, `true`, `2`, `"three"`, `false`, `1.0`},
		},
		{
			name:   "ref_defs",
			schema: `{"type":"object","properties":{"item":{"$ref":"#/$defs/Item"}},"required":["item"],"$defs":{"Item":{"type":"object","properties":{"id":{"type":"integer"}},"required":["id"]}}}`,
			docs: []string{
				`{"item":{"id":1}}`,
				`{"item":{"id":"x"}}`,     // ref target wrong type
				`{"item":{}}`,             // ref target missing required
				`{"item":{"id":1,"z":9}}`, // extra inside ref (both accept)
			},
		},
	}

	for _, tc := range cases {
		compiled, err := gojsonschema.NewSchema(gojsonschema.NewStringLoader(tc.schema))
		if err != nil {
			t.Fatalf("[%s] gojsonschema failed to compile schema: %v", tc.name, err)
		}
		rawSc, err := schema.Parse([]byte(tc.schema))
		if err != nil {
			t.Fatalf("[%s] schema.Parse failed: %v", tc.name, err)
		}
		sc := rawSc.AssumeNormalized()

		for _, doc := range tc.docs {
			var data any
			if err := json.Unmarshal([]byte(doc), &data); err != nil {
				t.Fatalf("[%s] bad test doc %q: %v", tc.name, doc, err)
			}

			result, err := compiled.Validate(gojsonschema.NewGoLoader(data))
			if err != nil {
				t.Fatalf("[%s] gojsonschema.Validate(%s) error: %v", tc.name, doc, err)
			}
			theirsValid := result.Valid()

			_, ourErr := sc.Validate(data)
			oursValid := ourErr == nil

			if oursValid == theirsValid {
				continue
			}
			if reason, ok := knownDivergence[tc.name+"\x00"+doc]; ok {
				t.Logf("[%s] known divergence on %s (%s): ours-valid=%v gojsonschema-valid=%v",
					tc.name, doc, reason, oursValid, theirsValid)
				continue
			}
			t.Errorf("[%s] DISAGREEMENT on %s: ours-valid=%v gojsonschema-valid=%v\n  ourErr=%v\n  theirErrs=%v",
				tc.name, doc, oursValid, theirsValid, ourErr, result.Errors())
		}
	}
}

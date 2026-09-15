package schematest

import (
	"strings"
	"testing"

	"genroc/internal/schema"
)

// A required property's default can never apply: required is judged first, so an absent key
// is refused rather than filled. `config_schema` has always said so; every other schema
// accepted the dead pair silently. Checked in CheckDoc, so it holds wherever an author
// writes a schema — input_schema, responses, result_schema, raises, $defs.
func TestCheckDoc_RequiredWithDefaultIsRefused(t *testing.T) {
	for _, tc := range []struct {
		name, doc string
		wantErr   bool
	}{
		{name: "required with a default", wantErr: true,
			doc: `{"type":"object","properties":{"u":{"type":"string","default":"x"}},"required":["u"]}`},
		{name: "nested, required with a default", wantErr: true,
			doc: `{"type":"object","properties":{"o":{"type":"object","properties":{"u":{"type":"string","default":"x"}},"required":["u"]}}}`},
		{name: "required inside an array item", wantErr: true,
			doc: `{"type":"array","items":{"type":"object","properties":{"u":{"type":"string","default":"x"}},"required":["u"]}}`},
		{name: "a default through a $ref", wantErr: true,
			doc: `{"type":"object","$defs":{"D":{"type":"string","default":"x"}},"properties":{"u":{"$ref":"#/$defs/D"}},"required":["u"]}`},

		{name: "optional with a default", wantErr: false,
			doc: `{"type":"object","properties":{"u":{"type":"string","default":"x"}}}`},
		{name: "required without a default", wantErr: false,
			doc: `{"type":"object","properties":{"u":{"type":"string"}},"required":["u"]}`},
		{name: "another property is required", wantErr: false,
			doc: `{"type":"object","properties":{"u":{"type":"string","default":"x"},"v":{"type":"string"}},"required":["v"]}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			parsed, err := schema.Parse([]byte(tc.doc))
			if err != nil {
				t.Fatalf("parse: %v", err)
			}
			err = parsed.AssumeNormalized().CheckDoc()
			if !tc.wantErr {
				if err != nil {
					t.Fatalf("a legitimate schema was refused: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatal("accepted a required property carrying a default; the default can never apply, " +
					"so the schema says something it cannot do")
			}
			if !strings.Contains(err.Error(), "u") {
				t.Errorf("error %q does not name the offending property", err)
			}
		})
	}
}

package schematest

import "testing"

// A CheckDoc error names where in the DOCUMENT the bad node lives, assembled from one
// label per slot as the recursion unwinds. It is the definition site, not an access path:
// a $defs entry is checked once under its own name however many refs reach it, which is
// what keeps the location finite on a recursive schema.
func TestCheckDocErrorNamesTheFailingLocation(t *testing.T) {
	for _, tc := range []struct{ name, doc, want string }{
		{
			"through properties",
			`{"type":"object","properties":{"user":{"type":"object","properties":{"age":{"type":"strng"}}}}}`,
			`user: age: unsupported schema type "strng"`,
		},
		{
			"through items",
			`{"type":"object","properties":{"rows":{"type":"array","items":{"type":"object","properties":{"x":{"type":"strng"}}}}}}`,
			`rows: items: x: unsupported schema type "strng"`,
		},
		{
			// The variant index is the whole location here: without it a bad arm reads as
			// though the union itself were at fault, and in a wide union there is nothing
			// left to point at.
			"union arm carries its index",
			`{"oneOf":[{"type":"string"},{"type":"integer"},{"type":"object","properties":{"x":{"type":"strng"}}}]}`,
			`oneOf[2]: x: unsupported schema type "strng"`,
		},
		{
			"union arm holding the bad default",
			`{"type":"object","properties":{"a":{"oneOf":[{"type":"string"},{"type":"integer","default":"nope"}]}}}`,
			`a: oneOf[1]: default does not validate against its schema: expected type integer, got string`,
		},
		{
			"every slot kind at once",
			`{"$defs":{"Foo":{"type":"object","properties":{"rows":{"type":"array","items":{"anyOf":[
				{"type":"string"},{"type":"object","properties":{"z":{"type":"strng"}}}
			]}}}}},"$ref":"#/$defs/Foo"}`,
			`$defs.Foo: rows: items: anyOf[1]: z: unsupported schema type "strng"`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := normalize(t, tc.doc).CheckDoc()
			if err == nil {
				t.Fatalf("CheckDoc accepted a schema it must reject; wanted %q", tc.want)
			}
			if err.Error() != tc.want {
				t.Errorf("location reported as\n  %q\nwant\n  %q\na dropped segment leaves the author "+
					"hunting for which node the message is about", err.Error(), tc.want)
			}
		})
	}
}

package schematest

import "testing"

func TestIsSubset_composition_sub(t *testing.T) {
	cases := []struct {
		name  string
		sub   string
		super string
		want  bool
	}{
		{
			"anyOf [integer,number] ⊆ number (all variants fit)",
			`{"anyOf":[{"type":"integer"},{"type":"number"}]}`,
			`{"type":"number"}`,
			true,
		},
		{
			"anyOf [integer,string] ⊄ number (string doesn't fit)",
			`{"anyOf":[{"type":"integer"},{"type":"string"}]}`,
			`{"type":"number"}`,
			false,
		},
		{
			"oneOf [integer,string] ⊆ anyOf [integer,string,boolean]",
			`{"oneOf":[{"type":"integer"},{"type":"string"}]}`,
			`{"anyOf":[{"type":"integer"},{"type":"string"},{"type":"boolean"}]}`,
			true,
		},
		{
			"oneOf [integer,string] ⊄ oneOf [integer,boolean]",
			`{"oneOf":[{"type":"integer"},{"type":"string"}]}`,
			`{"oneOf":[{"type":"integer"},{"type":"boolean"}]}`,
			false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assertSubset(t, tc.sub, tc.super, tc.want)
		})
	}
}

func TestIsSubset_composition_super(t *testing.T) {
	cases := []struct {
		name  string
		sub   string
		super string
		want  bool
	}{
		{
			"string ⊆ anyOf [string,integer]",
			`{"type":"string"}`,
			`{"anyOf":[{"type":"string"},{"type":"integer"}]}`,
			true,
		},
		{
			"boolean ⊄ anyOf [string,integer]",
			`{"type":"boolean"}`,
			`{"anyOf":[{"type":"string"},{"type":"integer"}]}`,
			false,
		},
		{
			"integer ⊆ anyOf [string,number] (widening)",
			`{"type":"integer"}`,
			`{"anyOf":[{"type":"string"},{"type":"number"}]}`,
			true,
		},
		{
			"string ⊆ oneOf [string,integer]",
			`{"type":"string"}`,
			`{"oneOf":[{"type":"string"},{"type":"integer"}]}`,
			true,
		},
		{
			"[integer,null] ⊆ anyOf [integer,null] (one union, two spellings)",
			`{"type":["integer","null"]}`,
			`{"anyOf":[{"type":"integer"},{"type":"null"}]}`,
			true,
		},
		{
			"[integer,null] ⊆ oneOf [number,null]",
			`{"type":["integer","null"]}`,
			`{"oneOf":[{"type":"number"},{"type":"null"}]}`,
			true,
		},
		{
			"[integer,null] ⊆ anyOf [$ref integer, null]",
			`{"type":["integer","null"]}`,
			`{"$defs":{"Count":{"type":"integer"}},"anyOf":[{"$ref":"#/$defs/Count"},{"type":"null"}]}`,
			true,
		},
		{
			"[integer,string] ⊄ anyOf [integer,null] (each type needs its own arm)",
			`{"type":["integer","string"]}`,
			`{"anyOf":[{"type":"integer"},{"type":"null"}]}`,
			false,
		},
		{
			"a split keeps each type's constraints",
			`{"type":["integer","null"],"minimum":0}`,
			`{"anyOf":[{"type":"integer","minimum":1},{"type":"null"}]}`,
			false,
		},
		{
			"an enum splits by value type",
			`{"type":["string","null"],"enum":["a",null]}`,
			`{"anyOf":[{"type":"string","enum":["a"]},{"type":"null"}]}`,
			true,
		},
		{
			// Kept with a nil enum, the integer arm would read as every integer.
			"a type no enum value has adds nothing",
			`{"type":["string","integer"],"enum":["a"]}`,
			`{"anyOf":[{"type":"string"},{"type":"null"}]}`,
			true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assertSubset(t, tc.sub, tc.super, tc.want)
		})
	}
}

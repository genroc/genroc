package schematest

import (
	"strings"
	"testing"
)

// assertDocErr checks that CheckDoc rejects the schema with an error mentioning wantSub.
func assertDocErr(t *testing.T, schemaJSON, wantSub string) {
	t.Helper()
	err := mustParse(t, schemaJSON).CheckDoc()
	if err == nil {
		t.Fatalf("CheckDoc: expected error containing %q, got nil", wantSub)
	}
	if !strings.Contains(err.Error(), wantSub) {
		t.Errorf("CheckDoc error %q does not contain %q", err.Error(), wantSub)
	}
}

// assertDocOK checks that CheckDoc accepts the schema.
func assertDocOK(t *testing.T, schemaJSON string) {
	t.Helper()
	if err := mustParse(t, schemaJSON).CheckDoc(); err != nil {
		t.Errorf("CheckDoc: unexpected error: %v", err)
	}
}

func TestCheckDocRejectsDefaultTypeMismatch(t *testing.T) {
	assertDocErr(t,
		`{"type":"object","properties":{"blob":{"type":"string","default":12}}}`,
		"expected type string, got integer")
}

func TestCheckDocRejectsDefaultViolatingConstraints(t *testing.T) {
	assertDocErr(t,
		`{"type":"integer","minimum":1,"default":0}`,
		"less than minimum")
	assertDocErr(t,
		`{"type":"string","enum":["a","b"],"default":"c"}`,
		"enum")
}

func TestCheckDocRejectsInvalidDefaultAnywhere(t *testing.T) {
	// Inside items.
	assertDocErr(t,
		`{"type":"array","items":{"type":"string","default":true}}`,
		"default does not validate")
	// On a $defs entry (validated against the pool it lives in).
	assertDocErr(t,
		`{"$ref":"#/$defs/S","$defs":{"S":{"type":"string","default":5}}}`,
		"default does not validate")
	// An object default missing a required member.
	assertDocErr(t,
		`{"type":"object","properties":{"r":{
			"type":"object","properties":{"v":{"type":"integer"}},"required":["v"],"default":{}
		}}}`,
		"required property")
}

func TestCheckDocAcceptsValidDefaults(t *testing.T) {
	assertDocOK(t, `{"type":"object","properties":{
		"s":{"type":"string","default":"hi"},
		"n":{"type":"integer","minimum":1,"default":3},
		"o":{"type":"object","properties":{"v":{"type":"integer"}},"required":["v"],"default":{"v":1}}
	}}`)
	// A default on a $ref target, validated against the resolved schema.
	assertDocOK(t, `{"type":"object","properties":{"r":{"$ref":"#/$defs/S"}},
		"$defs":{"S":{"type":"string","default":"ok"}}}`)
}

func TestValidateRejectsInvalidDefaultAtFillTime(t *testing.T) {
	// A schema that never went through CheckDoc still may not fill a
	// schema-violating default.
	sc := mustParse(t, `{"type":"object","properties":{"blob":{"type":"string","default":12}}}`)
	if _, err := sc.Validate(mustData(t, `{}`)); err == nil {
		t.Errorf("Validate: expected invalid-default error, got none")
	}
	// A present value never touches the default: still valid.
	if _, err := sc.Validate(mustData(t, `{"blob":"x"}`)); err != nil {
		t.Errorf("Validate with property present: unexpected error: %v", err)
	}
}

// `default: null` and no default at all both decode to a nil `any`, so null cannot be
// declared as what an absent property becomes. Nothing may be built on reading one back —
// the absent-as-null migration is the one thing that writes a null in, and it decides on
// the type admitting null rather than on any default.
func TestDefaultNullIsIndistinguishableFromNoDefault(t *testing.T) {
	sc := mustParse(t, `{"type":"object","properties":{"b":{"type":["number","null"],"default":null}}}`)
	if d := sc.Properties()["b"].Default(); d != nil {
		t.Fatalf("got default %#v; a null default is not representable and must not appear to be", d)
	}
	// It is dropped, not rejected: a schema saying it is still a valid document.
	assertDocOK(t, `{"type":"object","properties":{"b":{"type":["number","null"],"default":null}}}`)
	// And with no default to fill, an absent optional stays absent under a strict conform.
	got, err := sc.Validate(mustData(t, `{}`))
	if err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if m, ok := got.(map[string]any); !ok || len(m) != 0 {
		t.Fatalf("got %#v, want the key left out entirely", got)
	}
}

func TestValidateNormalizesFilledDefault(t *testing.T) {
	// A filled object default is conformed like a supplied value: undeclared
	// members are pruned and nested defaults fill in.
	assertNormalized(t,
		`{"type":"object","properties":{"o":{
			"type":"object",
			"properties":{"v":{"type":"integer"},"mode":{"type":"string","default":"auto"}},
			"default":{"v":1,"junk":true}
		}}}`,
		`{}`,
		`{"o":{"v":1,"mode":"auto"}}`)
}

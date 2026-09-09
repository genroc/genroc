package schema

import "testing"

// A default is not an annotation here: conforming fills an absent optional's, so the value is
// always there. Navigation has typed it non-nullable all along, and a summary marking it `?`
// contradicted the type printed beside it.
func TestADefaultedPropertyIsNotMarkedAbsent(t *testing.T) {
	s := mustParse(t, `{"type":"object","properties":{
		"who":{"type":"string","default":"world"},
		"bare":{"type":"string"},
		"req":{"type":"string"}},"required":["req"]}`)

	if got := s.MemberNames(); got != "bare?, req, who" {
		t.Errorf("MemberNames = %q, want %q", got, "bare?, req, who")
	}
	for name, want := range map[string]bool{"who": false, "bare": true, "req": false} {
		if got := s.MayBeAbsent(name); got != want {
			t.Errorf("MayBeAbsent(%q) = %v, want %v", name, got, want)
		}
	}
}

// The two answers must agree: what a reader is told about a member's presence, and what the
// expression reading it actually types as.
func TestPresenceAgreesWithWhatReadingItTypes(t *testing.T) {
	s := mustParse(t, `{"type":"object","properties":{
		"who":{"type":"string","default":"world"},
		"bare":{"type":"string"}},"required":[]}`)
	for _, name := range []string{"who", "bare"} {
		read, err := s.At(name)
		if err != nil {
			t.Fatalf("At(%q): %v", name, err)
		}
		if s.MayBeAbsent(name) != read.HasNull() {
			t.Errorf("%q: MayBeAbsent=%v but reading it types as %s",
				name, s.MayBeAbsent(name), read.Summary())
		}
	}
}

func mustParse(t *testing.T, src string) Schema {
	t.Helper()
	raw, err := Parse([]byte(src))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	s, err := raw.Normalize()
	if err != nil {
		t.Fatalf("normalize: %v", err)
	}
	return s
}

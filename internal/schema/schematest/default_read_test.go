package schematest

import (
	"encoding/json"
	"strings"
	"testing"

	"genroc/internal/schema"
)

// A `default` says how the object CONTAINING a property is conformed — absent, fill this. By the
// time the value is READ the fill has happened, so the keyword is spent and is not part of what
// the read yields. Carrying it made the inferred type an invalid schema DOCUMENT, which is how
// this was found: a `$process` spread wrote an inferred `raises` payload whose property was both
// required and defaulted, and `CheckDoc` refuses that pair.

func TestReadingADefaultedPropertyDropsTheDefault(t *testing.T) {
	s := mustSchema(t, `{"type":"object","properties":{"ms":{"type":"integer","default":5000}}}`)
	prop, err := s.At("ms")
	if err != nil {
		t.Fatalf("At: %v", err)
	}
	raw, err := json.Marshal(prop)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(raw), "default") {
		t.Fatalf("a read carried the default forward: %s", raw)
	}
	// The presence rule must survive the drop: a defaulted property is guaranteed present, so
	// the read is NOT nullable. Dropping the default before deciding that is the way to break it.
	if prop.HasNull() {
		t.Fatal("a defaulted property read back nullable — the default was dropped before it decided presence")
	}
	// And the contrast: no default, optional, so absence is real and the read is nullable.
	plain := mustSchema(t, `{"type":"object","properties":{"ms":{"type":"integer"}}}`)
	opt, err := plain.At("ms")
	if err != nil {
		t.Fatalf("At: %v", err)
	}
	if !opt.HasNull() {
		t.Fatal("an optional property with no default must read back nullable")
	}
}

// The consequence, stated as the thing that actually broke: an inferred type becomes a schema
// DOCUMENT (a `$process` spread writes one), so it has to be a valid one.
func TestATypeBuiltFromADefaultedReadIsAValidDocument(t *testing.T) {
	s := mustSchema(t, `{"type":"object","properties":{"ms":{"type":"integer","default":5000}}}`)
	prop, err := s.At("ms")
	if err != nil {
		t.Fatalf("At: %v", err)
	}
	// What inference builds for `data: { budget_ms: "$: input.ms" }`: the read is guaranteed
	// present, so the property is required.
	built := schema.Object().WithProperty("budget_ms", prop, true)
	if err := built.CheckDoc(); err != nil {
		t.Fatalf("a type built from a defaulted read is not a valid document: %v", err)
	}
}

// A default behind a `$ref` is deliberately left alone: removing it means materializing the
// reference, which costs the name and does not terminate on a recursive one. This is here so the
// limit is a decision on the record rather than a gap someone finds later and reads as a bug.
func TestADefaultBehindARefIsLeftAlone(t *testing.T) {
	s := mustSchema(t, `{"$defs":{"MS":{"type":"integer","default":5000}},
	                     "type":"object","properties":{"ms":{"$ref":"#/$defs/MS"}}}`)
	prop, err := s.At("ms")
	if err != nil {
		t.Fatalf("At: %v", err)
	}
	if prop.HasNull() {
		t.Fatal("the presence rule must see through the ref: a defaulted property is present")
	}
}

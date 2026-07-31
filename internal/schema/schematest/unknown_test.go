package schematest

import (
	"encoding/json"
	"strings"
	"testing"

	"genroc/internal/schema"
)

// "unknown" is not a keyword — it is the empty schema {}, JSON Schema's top type, used
// for a value carried but never inspected. A dedicated `type: unknown` spelling was
// built and then dropped: it would have been the only thing genroc accepts that a
// standard validator rejects, and since it was erased at parse it never reached the
// stored definition anyway (see docs/unknown-type.md). These tests pin the behaviours
// that make {} usable as a type, and the narrowing relation that lets it re-enter the
// typed world.

// An annotation describes the slot without constraining the value, so a described empty
// node is still the top type. This is the recommended way to say the opacity is
// deliberate, since a bare {} cannot say it.
func TestDescribedEmptyNodeIsStillTop(t *testing.T) {
	for _, tc := range []struct{ name, in string }{
		{"bare", `{"type":"object","properties":{"payload":{}},"required":["payload"]}`},
		{"described", `{"type":"object","properties":{"payload":{"description":"opaque"}},"required":["payload"]}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := normalize(t, tc.in)
			// Navigating TO it works; reading THROUGH it does not.
			if _, err := s.At("payload"); err != nil {
				t.Fatalf("navigating to the unknown must work: %v", err)
			}
			_, err := s.At("payload.answer")
			if err == nil {
				t.Fatal("expected an error reading through an unknown")
			}
			if !strings.Contains(err.Error(), "the value is unknown") {
				t.Errorf("message should name the cause, got: %v", err)
			}
			// And it is not a subset of anything typed.
			if s.IsSubset(normalize(t, `{"type":"object","properties":{"payload":{"type":"string"}},"required":["payload"]}`)) {
				t.Error("an unknown must not be a subset of a typed schema")
			}
		})
	}
}

// A type name outside the JSON Schema simpleTypes enum matches no value, so the schema
// is unsatisfiable — it used to parse cleanly and then reject everything at runtime. The
// check is a CheckDoc validity rule, not a decode rule, so a definition already stored
// with a bad name stays decodable (see the note in checkdoc.go).
func TestUnsupportedTypeNameRejected(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{`{"type":"date"}`, `unsupported schema type "date"`},
		{`{"type":"strng"}`, `unsupported schema type "strng"`},
		{`{"type":"object","properties":{"a":{"type":"any"}}}`, `a: unsupported schema type "any"`},
		// "unknown" is not a genroc keyword: it is as invalid as any other bad name.
		{`{"type":"unknown"}`, `unsupported schema type "unknown"`},
		{`{"type":"object","properties":{"a":{}}}`, ``},
	} {
		raw, err := schema.Parse([]byte(tc.in))
		if err != nil {
			t.Fatalf("parse %s: %v", tc.in, err)
		}
		err = raw.CheckDoc()
		if tc.want == "" {
			if err != nil {
				t.Errorf("CheckDoc(%s) = %v, want nil", tc.in, err)
			}
			continue
		}
		if err == nil || err.Error() != tc.want {
			t.Errorf("CheckDoc(%s) = %v, want %q", tc.in, err, tc.want)
		}
	}
}

// The decode path stays permissive so an already-stored definition with a bad type name
// fails its own registration rather than becoming undecodable — which would take out the
// whole ListDefinitions page it sits on.
func TestUnsupportedTypeNameStillDecodes(t *testing.T) {
	if _, err := schema.Parse([]byte(`{"type":"date"}`)); err != nil {
		t.Errorf("parse must stay permissive on type names, got: %v", err)
	}
	var s schema.Schema
	if err := json.Unmarshal([]byte(`{"type":"date"}`), &s); err != nil {
		t.Errorf("stored-schema decode must stay permissive, got: %v", err)
	}
}

// An unknown validates anything and passes it through untouched — the property that
// makes forwarding work at all. The enclosing object still strips its own undeclared
// keys, so opacity does not leak outward.
func TestUnknownValidatesAnythingVerbatim(t *testing.T) {
	s := normalize(t, `{"type":"object","properties":{"status":{"type":"string"},"payload":{"description":"opaque"}},"required":["status"]}`)
	var in any
	if err := json.Unmarshal([]byte(`{"status":"done","payload":{"answer":42,"deep":[1,{"a":true}]},"extra":"dropped"}`), &in); err != nil {
		t.Fatal(err)
	}
	out, err := s.Validate(in)
	if err != nil {
		t.Fatalf("Validate: %v", err)
	}
	got, _ := json.Marshal(out)
	const want = `{"payload":{"answer":42,"deep":[1,{"a":true}]},"status":"done"}`
	if string(got) != want {
		t.Errorf("Validate = %s, want %s", got, want)
	}
}

func assertNarrows(t *testing.T, subJSON, superJSON string, want bool) {
	t.Helper()
	if got := mustAssumed(t, subJSON).NarrowsTo(mustAssumed(t, superJSON)); got != want {
		t.Errorf("NarrowsTo(%s, %s) = %v, want %v", subJSON, superJSON, got, want)
	}
}

// NarrowsTo differs from IsSubset on exactly one case: an unknown in sub position. It
// answers "could this be narrowed to super?", which is sound only because the caller
// conforms the value against super at runtime.
func TestNarrowsToAdmitsUnknown(t *testing.T) {
	const typed = `{"type":"object","properties":{"answer":{"type":"number"}},"required":["answer"]}`

	// Bare unknown, and one carrying a description — both are the top type.
	for _, unknown := range []string{`{}`, `{"description":"opaque"}`} {
		assertSubset(t, unknown, typed, false)
		assertNarrows(t, unknown, typed, true)
	}

	// Nested: the canonical poller shape — a typed envelope around one opaque field.
	const envelope = `{"type":"object","properties":{"payload":{}},"required":["payload"]}`
	const pinned = `{"type":"object","properties":{"payload":` + typed + `},"required":["payload"]}`
	assertSubset(t, envelope, pinned, false)
	assertNarrows(t, envelope, pinned, true)

	// Inside an array element.
	assertSubset(t, `{"type":"array","items":{}}`, `{"type":"array","items":`+typed+`}`, false)
	assertNarrows(t, `{"type":"array","items":{}}`, `{"type":"array","items":`+typed+`}`, true)
}

// Narrowing relaxes the unknown case and nothing else: a genuinely wrong type is still
// a static error, so the relation stays a real check rather than a rubber stamp.
func TestNarrowsToStillRejectsRealMismatch(t *testing.T) {
	const typed = `{"type":"object","properties":{"answer":{"type":"number"}},"required":["answer"]}`
	assertNarrows(t, `{"type":"string"}`, typed, false)
	assertNarrows(t, `{"type":"object","properties":{"answer":{"type":"string"}},"required":["answer"]}`, typed, false)
	// A required field the sub never produces.
	assertNarrows(t, `{"type":"object","properties":{"other":{"type":"number"}}}`, typed, false)
	// An unknown in SUPER position is the pre-existing rule and unaffected: everything
	// fits into an unknown, which is how forwarding upward works.
	assertNarrows(t, typed, `{}`, true)
	assertSubset(t, typed, `{}`, true)
}

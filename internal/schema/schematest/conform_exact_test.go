package schematest

import (
	"strings"
	"testing"

	"genroc/internal/schema"
)

// The REMOVE half of ConformToSchemaExactly, and the relation that promises it. absent_test.go
// holds the add half — a required nullable that is missing gets its null written in. This file
// does the other direction: a stored null the new schema will not hold, where the property is
// optional, so absence is valid and dropping the key reconciles the row.
//
// The pairing is the whole point. IsSubsetAsStored must accept exactly what this can close:
// tolerate more and an upgrade the report blessed fails at the conform; close more and the
// relation is refusing something that works.

// closable is a version gap the strict relation refuses and IsSubsetAsStored accepts, written
// as the whole transformation — the two schemas, the row stored under `old`, and the exact row
// the conform must hand back under `new`.
type closable struct {
	name string
	old  string
	new  string
	in   string
	want string
}

var closables = []closable{
	{
		name: "optional string|null becomes optional string",
		old:  `{"type":"object","properties":{"note":{"type":["string","null"]}}}`,
		new:  `{"type":"object","properties":{"note":{"type":"string"}}}`,
		in:   `{"note":null}`,
		want: `{}`,
	},
	{
		name: "required string|null becomes optional string",
		old:  `{"type":"object","properties":{"note":{"type":["string","null"]}},"required":["note"]}`,
		new:  `{"type":"object","properties":{"note":{"type":"string"}}}`,
		in:   `{"note":null}`,
		want: `{}`,
	},
	{
		name: "the sibling that still fits keeps its value",
		old: `{"type":"object","properties":{"note":{"type":["string","null"]},"keep":{"type":"integer"}},
		       "required":["keep"]}`,
		new: `{"type":"object","properties":{"note":{"type":"string"},"keep":{"type":"integer"}},
		       "required":["keep"]}`,
		in:   `{"note":null,"keep":7}`,
		want: `{"keep":7}`,
	},
	{
		name: "one level down, the outer object survives",
		old: `{"type":"object","properties":{"cfg":{"type":"object","properties":{"note":{"type":["string","null"]}}}},
		       "required":["cfg"]}`,
		new: `{"type":"object","properties":{"cfg":{"type":"object","properties":{"note":{"type":"string"}}}},
		       "required":["cfg"]}`,
		in:   `{"cfg":{"note":null}}`,
		want: `{"cfg":{}}`,
	},
	{
		name: "through a $ref",
		old: `{"type":"object","properties":{"u":{"$ref":"#/$defs/U"}},"required":["u"],
		       "$defs":{"U":{"type":"object","properties":{"note":{"type":["string","null"]},"id":{"type":"string"}}}}}`,
		new: `{"type":"object","properties":{"u":{"$ref":"#/$defs/U"}},"required":["u"],
		       "$defs":{"U":{"type":"object","properties":{"note":{"type":"string"},"id":{"type":"string"}}}}}`,
		in:   `{"u":{"note":null,"id":"abc"}}`,
		want: `{"u":{"id":"abc"}}`,
	},
	{
		name: "inside a union variant, the other variant untouched",
		old:  `{"oneOf":[{"type":"object","properties":{"note":{"type":["string","null"]}}},{"type":"integer"}]}`,
		new:  `{"oneOf":[{"type":"object","properties":{"note":{"type":"string"}}},{"type":"integer"}]}`,
		in:   `{"note":null}`,
		want: `{}`,
	},
	{
		name: "an undeclared key is not data to tidy away",
		old:  `{"type":"object","properties":{"note":{"type":["string","null"]}}}`,
		new:  `{"type":"object","properties":{"note":{"type":"string"}}}`,
		in:   `{"note":null,"stale":{"from":"a dropped task"}}`,
		want: `{"stale":{"from":"a dropped task"}}`,
	},
}

// oneLine strips the source indentation a wrapped raw string carries into a failure message,
// so old and new print aligned and the single difference between them is visible.
func oneLine(s string) string { return strings.Join(strings.Fields(s), "") }

// showing renders the whole scenario, so a failure reads as the transformation that broke
// rather than a key name with no schema next to it.
func (c closable) showing(got any, t *testing.T) string {
	return "\n  old:  " + oneLine(c.old) +
		"\n  new:  " + oneLine(c.new) +
		"\n  in:   " + c.in +
		"\n  want: " + valueJSON(t, decodeValue(t, c.want)) +
		"\n  got:  " + valueJSON(t, got)
}

func TestConformExact_RemovesTheNullTheRelationPromised(t *testing.T) {
	for _, c := range closables {
		t.Run(c.name, func(t *testing.T) {
			old, new := mustSchema(t, c.old), mustSchema(t, c.new)

			if old.IsSubset(new) {
				t.Error("the STRICT relation accepted this: then there is no gap, and this row " +
					"is not testing a migration")
			}
			if !old.IsSubsetAsStored(new) {
				t.Fatal("IsSubsetAsStored refused a gap the conform can close — the relation is " +
					"the promise, so refusing it bars an upgrade that would have worked")
			}

			got, err := new.Validate(decodeValue(t, c.in), schema.ConformToSchemaExactly)
			if err != nil {
				t.Fatalf("the conform could not close a gap the relation accepted: %v%s",
					err, c.showing(nil, t))
			}
			if valueJSON(t, got) != valueJSON(t, decodeValue(t, c.want)) {
				t.Errorf("the conform did not produce the reconciled row%s", c.showing(got, t))
			}
			// The claim every migration rests on, executed rather than asserted.
			if _, err := new.Validate(got, schema.Strict); err != nil {
				t.Errorf("the reconciled row does not pass a strict conform: %v%s", err, c.showing(got, t))
			}
		})
	}
}

// Reconciling twice is reconciling once: a partial bulk run has to be repairable by repetition.
func TestConformExact_RemovalIsIdempotent(t *testing.T) {
	for _, c := range closables {
		t.Run(c.name, func(t *testing.T) {
			s := mustSchema(t, c.new)
			once, err := s.Validate(decodeValue(t, c.in), schema.ConformToSchemaExactly)
			if err != nil {
				t.Fatalf("first: %v", err)
			}
			twice, err := s.Validate(once, schema.ConformToSchemaExactly)
			if err != nil {
				t.Fatalf("second: %v", err)
			}
			if valueJSON(t, once) != valueJSON(t, twice) {
				t.Errorf("a second run changed the row again\n  once:  %s\n  twice: %s",
					valueJSON(t, once), valueJSON(t, twice))
			}
		})
	}
}

// Removal fires on the null the TARGET will not hold, never on one it will. Nullable admits
// both states, so there is nothing to reconcile and the row stands exactly as stored —
// removing there would invent a canonical form the schema does not name, and destroy a null
// someone wrote on purpose.
func TestConformExact_ANullTheTargetStillHoldsIsLeftAlone(t *testing.T) {
	cases := []struct{ name, schema, unchanged string }{
		{
			name:      "optional string|null keeps the null",
			schema:    `{"type":"object","properties":{"note":{"type":["string","null"]}}}`,
			unchanged: `{"note":null}`,
		},
		{
			name:      "required string|null keeps the null",
			schema:    `{"type":"object","properties":{"note":{"type":["string","null"]}},"required":["note"]}`,
			unchanged: `{"note":null}`,
		},
		{
			name:      "nullable spelled as a union keeps the null",
			schema:    `{"type":"object","properties":{"note":{"oneOf":[{"type":"string"},{"type":"null"}]}}}`,
			unchanged: `{"note":null}`,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := mustSchema(t, c.schema).Validate(decodeValue(t, c.unchanged),
				schema.ConformToSchemaExactly)
			if err != nil {
				t.Fatalf("conform: %v", err)
			}
			if valueJSON(t, got) != valueJSON(t, decodeValue(t, c.unchanged)) {
				t.Errorf("the row was reconciled when the schema holds it as stored"+
					"\n  schema: %s\n  stored: %s\n  got:    %s",
					oneLine(c.schema), c.unchanged, valueJSON(t, got))
			}
		})
	}
}

// Removal is a reconciliation only where absence is a VALID state. These are the places it is
// not — and the relation must refuse them rather than promise a migration that cannot run.
func TestConformExact_WhereAbsenceIsNotValidTheRelationRefuses(t *testing.T) {
	cases := []struct{ name, old, new, in, why string }{
		{
			name: "a property that stays required",
			old:  `{"type":"object","properties":{"note":{"type":["string","null"]}},"required":["note"]}`,
			new:  `{"type":"object","properties":{"note":{"type":"string"}},"required":["note"]}`,
			in:   `{"note":null}`,
			why:  "required: the null can be neither kept nor removed",
		},
		{
			name: "an array element",
			old:  `{"type":"array","items":{"type":["string","null"]}}`,
			new:  `{"type":"array","items":{"type":"string"}}`,
			in:   `["a",null]`,
			why:  "an element has no absent state — dropping it would shorten the array",
		},
		{
			// Removable in principle: absence is valid for an open map's key too. But the
			// rule only walks declared properties, so BOTH sides refuse. That is the safe
			// direction of the pairing — a promise not made — and pinning it here means a
			// fix has to move the relation and the conform together.
			name: "an open map's value",
			old:  `{"type":"object","additionalProperties":{"type":["string","null"]}}`,
			new:  `{"type":"object","additionalProperties":{"type":"string"}}`,
			in:   `{"a":null}`,
			why:  "the removal rule does not reach past declared properties",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			old, new := mustSchema(t, c.old), mustSchema(t, c.new)
			accepted := old.IsSubsetAsStored(new)
			got, err := new.Validate(decodeValue(t, c.in), schema.ConformToSchemaExactly)

			if accepted && err != nil {
				t.Errorf("the relation promised a migration the conform cannot perform (%s)"+
					"\n  old: %s\n  new: %s\n  in:  %s\n  err: %v", c.why, oneLine(c.old), oneLine(c.new), c.in, err)
			}
			if !accepted && err == nil {
				t.Errorf("the conform closed a gap the relation refuses (%s) — they must accept "+
					"exactly the same set, in both directions"+
					"\n  old: %s\n  new: %s\n  in:  %s\n  got: %s",
					c.why, oneLine(c.old), oneLine(c.new), c.in, valueJSON(t, got))
			}
		})
	}
}

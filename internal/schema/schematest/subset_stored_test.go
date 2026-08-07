package schematest

import (
	"testing"

	"genroc/internal/schema"
)

// IsSubsetAsStored reads both schemas as descriptions of data a conform already produced.
// Its one extra rule over IsSubsetAbsentAsNull — a defaulted property is guaranteed present —
// is the only relaxation in the package that needs NO migration behind it, and the tests are
// organised around exactly that claim: what the sub side guarantees is tolerated, what only
// the super side declares is not. specs/compat-command.md §2e.
//
// Getting the sides the wrong way round is the failure this file exists to catch. It would
// promise an upgrade over a row that never held the value, and nothing downstream would
// notice until an expression read null.

// ── the sub side guarantees it: tolerated, and no fill is involved ────────────

func TestStored_DefaultOnSubSatisfiesRequiredOnSuper(t *testing.T) {
	cases := []struct{ name, old, new string }{
		{
			name: "required added to a property that already carried a default",
			old:  `{"type":"object","properties":{"retries":{"type":"integer","default":3}}}`,
			new:  `{"type":"object","properties":{"retries":{"type":"integer","default":3}},"required":["retries"]}`,
		},
		{
			name: "required added, and the new side dropped the default",
			old:  `{"type":"object","properties":{"retries":{"type":"integer","default":3}}}`,
			new:  `{"type":"object","properties":{"retries":{"type":"integer"}},"required":["retries"]}`,
		},
		{
			name: "nested one level down",
			old:  `{"type":"object","properties":{"cfg":{"type":"object","properties":{"n":{"type":"integer","default":1}}}},"required":["cfg"]}`,
			new:  `{"type":"object","properties":{"cfg":{"type":"object","properties":{"n":{"type":"integer"}},"required":["n"]}},"required":["cfg"]}`,
		},
		{
			name: "through a $ref",
			old: `{"type":"object","properties":{"u":{"$ref":"#/$defs/U"}},"required":["u"],
			       "$defs":{"U":{"type":"object","properties":{"n":{"type":"integer","default":1}}}}}`,
			new: `{"type":"object","properties":{"u":{"$ref":"#/$defs/U"}},"required":["u"],
			       "$defs":{"U":{"type":"object","properties":{"n":{"type":"integer"}},"required":["n"]}}}`,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			old, new := mustSchema(t, c.old), mustSchema(t, c.new)
			if !old.IsSubsetAsStored(new) {
				t.Error("refused: a row conformed under the old schema holds the value, " +
					"because creation filled the default — there is no gap to close")
			}
			// The point of a separate relation: the fill-paired one must NOT accept this,
			// or absent_test.go's contract (it tolerates exactly what the fill closes) breaks.
			if old.IsSubsetAbsentAsNull(new) {
				t.Error("IsSubsetAbsentAsNull accepted it; the fill writes no defaults, " +
					"so that relation would be promising a migration it cannot perform")
			}
		})
	}
}

// ── only the super side declares it: refused, because a fill would be needed ──

func TestStored_DefaultOnSuperAloneIsRefused(t *testing.T) {
	cases := []struct{ name, old, new string }{
		{
			name: "a required property gained a default the old side never had",
			old:  `{"type":"object","properties":{"retries":{"type":"integer"}}}`,
			new:  `{"type":"object","properties":{"retries":{"type":"integer","default":3}},"required":["retries"]}`,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			old, new := mustSchema(t, c.old), mustSchema(t, c.new)
			if old.IsSubsetAsStored(new) {
				t.Error("accepted: the old row may lack the key entirely, and the upgrade fill " +
					"writes no defaults — this is the case internal/schema/CLAUDE.md declines")
			}
		})
	}
}

// A default on super alone is not a REQUIREMENT on super, either. The row in hand was
// conformed under the old schema and is carried over by a fill that writes no defaults, so
// what the new schema demands of it is its `required` set — nothing else. The consequence of
// adding a default is that readers see the value as non-null, and that surfaces where it is
// read: in the inferred context, as `integer|null → integer`.
func TestStored_DefaultOnSuperAloneIsNotARequirement(t *testing.T) {
	old := mustSchema(t, `{"type":"object","properties":{"retries":{"type":"integer"}}}`)
	new := mustSchema(t, `{"type":"object","properties":{"retries":{"type":"integer","default":3}}}`)
	if !old.IsSubsetAsStored(new) {
		t.Error("refused: the stored row is valid under the new schema — `retries` is optional " +
			"there — so reporting a break here would be a false alarm, and the real consequence " +
			"is reported at the values that read it")
	}
}

// ── the relaxation must not leak into the strict relation ────────────────────

func TestStored_StrictIsUnaffected(t *testing.T) {
	old := mustSchema(t, `{"type":"object","properties":{"retries":{"type":"integer","default":3}}}`)
	new := mustSchema(t, `{"type":"object","properties":{"retries":{"type":"integer","default":3}},"required":["retries"]}`)
	if old.IsSubset(new) {
		t.Error("IsSubset accepted a newly required property: the contract check reads a schema " +
			"as what may ARRIVE, and conformObject rejects the absence before it looks for a default")
	}
}

// ── the rule is about presence, never about type ─────────────────────────────

func TestStored_DefaultDoesNotExcuseATypeChange(t *testing.T) {
	old := mustSchema(t, `{"type":"object","properties":{"n":{"type":"integer","default":1}}}`)
	new := mustSchema(t, `{"type":"object","properties":{"n":{"type":"string","default":"1"}},"required":["n"]}`)
	if old.IsSubsetAsStored(new) {
		t.Error("accepted an integer where a string is required: a default guarantees the key " +
			"is present, and says nothing about what it holds")
	}
}

// ── it still carries everything the fill-paired relation carries ─────────────

func TestStored_KeepsTheAbsentAsNullGaps(t *testing.T) {
	for _, g := range gaps {
		t.Run(g.name, func(t *testing.T) {
			old, new := mustSchema(t, g.old), mustSchema(t, g.new)
			if !old.IsSubsetAsStored(new) {
				t.Error("refused a gap IsSubsetAbsentAsNull accepts: the stored relation is that " +
					"one plus a rule, so it can only ever tolerate more")
			}
		})
	}
}

var _ = schema.Schema{}

// ── the other direction: a stored null the new schema will not hold ───────────

// IsSubsetAsStored and ConformToSchemaExactly are a pair, and the pair closes the
// null-versus-missing gap BOTH ways. This is the direction that used to be refused for want
// of a migration that could remove a key: sub may hold a null, super will not take one, and
// super leaves the property optional — so the conform drops it and the row fits.
func TestStored_ANullTheNewSchemaWillNotHoldIsClosable(t *testing.T) {
	old := mustSchema(t, `{"type":"object","properties":{"note":{"type":["string","null"]}}}`)
	new := mustSchema(t, `{"type":"object","properties":{"note":{"type":"string"}}}`)

	if !old.IsSubsetAsStored(new) {
		t.Fatal("refused: the property is optional on the new side, so removing the key is a " +
			"valid reconciliation — which is exactly what ConformToSchemaExactly does")
	}
	// The claim the relation is making, executed.
	got, err := new.Validate(map[string]any{"note": nil}, schema.ConformToSchemaExactly)
	if err != nil {
		t.Fatalf("the migration could not close the gap the relation accepted: %v", err)
	}
	if _, present := got.(map[string]any)["note"]; present {
		t.Error("the null was kept: it does not satisfy the new schema, and the key had to go")
	}
	if _, err := new.Validate(got, schema.Strict); err != nil {
		t.Errorf("the reconciled value does not pass a strict conform: %v — that is the whole "+
			"claim a migration rests on", err)
	}
}

// Required is the case nothing can fix: absence is not valid there, so the null cannot be
// removed and the type will not take it. The relation must keep refusing it.
func TestStored_ARequiredNullThatDoesNotFitIsRefused(t *testing.T) {
	old := mustSchema(t, `{"type":"object","properties":{"note":{"type":["string","null"]}},"required":["note"]}`)
	new := mustSchema(t, `{"type":"object","properties":{"note":{"type":"string"}},"required":["note"]}`)

	if old.IsSubsetAsStored(new) {
		t.Error("accepted: the property is required on both sides, so a stored null can be " +
			"neither kept nor removed — there is no reconciliation to promise")
	}
}

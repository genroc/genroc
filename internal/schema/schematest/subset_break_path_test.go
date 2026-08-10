package schematest

import "testing"

// The break's Path is a FIELD, reaching Issue.Path and the renderer without ever being
// re-parsed out of a message. These pin its spelling so a caller can rely on it.
func TestExplainSubset_PathNamesWhereTheRelationStopped(t *testing.T) {
	for _, tc := range []struct{ name, sub, super, path, kind string }{
		{
			"nested properties join with a dot",
			`{"type":"object","properties":{"a":{"type":"object","properties":{"b":{"type":"number"}}}}}`,
			`{"type":"object","properties":{"a":{"type":"object","properties":{"b":{"type":"string"}}}}}`,
			"a.b", "type",
		},
		{
			"an array element is [] with no index — every element has the same type",
			`{"type":"array","items":{"type":"number"}}`,
			`{"type":"array","items":{"type":"string"}}`,
			"[]", "type",
		},
		{
			"property, element, property",
			`{"type":"object","properties":{"rows":{"type":"array","items":{"type":"object","properties":{"id":{"type":"number"}}}}}}`,
			`{"type":"object","properties":{"rows":{"type":"array","items":{"type":"object","properties":{"id":{"type":"string"}}}}}}`,
			"rows[].id", "type",
		},
		{
			"nested arrays stack",
			`{"type":"array","items":{"type":"array","items":{"type":"number"}}}`,
			`{"type":"array","items":{"type":"array","items":{"type":"string"}}}`,
			"[][]", "type",
		},
		{
			// A $ref is transparent: the path says where the VALUE is, and a consumer holding
			// the value never sees that its type was factored into a definition.
			"a $ref hop adds no segment",
			`{"$ref":"#/$defs/R","$defs":{"R":{"type":"object","properties":{"v":{"type":"number"}}}}}`,
			`{"$ref":"#/$defs/R","$defs":{"R":{"type":"object","properties":{"v":{"type":"string"}}}}}`,
			"v", "type",
		},
		{
			"a declared key checked against an open map is named",
			`{"type":"object","properties":{"k":{"type":"number"}}}`,
			`{"type":"object","additionalProperties":{"type":"string"}}`,
			"k", "type",
		},
		{
			// Two open maps have no key to name — the break is about every key at once.
			"open map against open map is *",
			`{"type":"object","additionalProperties":{"type":"number"}}`,
			`{"type":"object","additionalProperties":{"type":"string"}}`,
			"*", "type",
		},
		{
			"a missing required property is named at its own path",
			`{"type":"object","properties":{"a":{"type":"object","properties":{}}}}`,
			`{"type":"object","properties":{"a":{"type":"object","properties":{"z":{"type":"string"}},"required":["z"]}}}`,
			"a.z", "missing_required",
		},
		{
			// Union arms keep the value at the same depth, and which arm came closest is a
			// judgement the relation does not have — so the break sits at the union itself.
			"a union names the union, not an arm",
			`{"type":"object","properties":{"a":{"type":"number"}}}`,
			`{"oneOf":[{"type":"object","properties":{"a":{"type":"string"}}},{"type":"boolean"}]}`,
			"", "variants",
		},
		{
			"a name with a space is still a location",
			`{"type":"object","properties":{"my key":{"type":"number"}},"required":["my key"]}`,
			`{"type":"object","properties":{"my key":{"type":"string"}},"required":["my key"]}`,
			"my key", "type",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sub, super := normalize(t, tc.sub), normalize(t, tc.super)
			if sub.IsSubset(super) {
				t.Fatalf("these must not fit; the pair is the whole point of the case")
			}
			brk := explainOne(t, sub, super)
			if brk.Path != tc.path {
				t.Errorf("path = %q, want %q — a caller files the issue under this, so a wrong "+
					"or empty one sends a reader to the wrong place in the schema", brk.Path, tc.path)
			}
			if string(brk.Kind) != tc.kind {
				t.Errorf("kind = %q, want %q — the kind is what lets a caller word the break "+
					"its own way (the contract check reads new ⊆ old and inverts it)", brk.Kind, tc.kind)
			}
			if brk.Sub == "" || brk.Super == "" {
				t.Errorf("break at %q is half-empty (%q → %q); both sides are needed to say what moved",
					brk.Path, brk.Sub, brk.Super)
			}
		})
	}
}

// What each side HOLDS, named — not its type, which renders identically on both sides of a
// constraint narrowing and asserts that nothing changed.
func TestExplainSubset_ConstraintBreaksSayWhatMoved(t *testing.T) {
	for _, tc := range []struct{ name, sub, super, wantSub, wantSuper string }{
		{"enum narrowed", `{"type":"string"}`, `{"type":"string","enum":["a","b"]}`,
			"any string", `enum ["a", "b"]`},
		{"enum shrunk", `{"type":"string","enum":["a","b"]}`, `{"type":"string","enum":["a"]}`,
			`allows ["b"]`, `enum ["a"]`},
		{"minimum added", `{"type":"integer"}`, `{"type":"integer","minimum":3}`,
			"no minimum", "minimum 3"},
		{"minimum raised", `{"type":"integer","minimum":1}`, `{"type":"integer","minimum":3}`,
			"minimum 1", "minimum 3"},
		{"maximum lowered", `{"type":"integer","maximum":9}`, `{"type":"integer","maximum":3}`,
			"maximum 9", "maximum 3"},
		{"maxLength added", `{"type":"string"}`, `{"type":"string","maxLength":2}`,
			"no maxLength", "maxLength 2"},
		{"minLength raised", `{"type":"string","minLength":1}`, `{"type":"string","minLength":4}`,
			"minLength 1", "minLength 4"},
		{"maxItems added", `{"type":"array","items":{"type":"string"}}`,
			`{"type":"array","items":{"type":"string"},"maxItems":2}`,
			"no maxItems", "maxItems 2"},
		{"minItems raised", `{"type":"array","minItems":1}`, `{"type":"array","minItems":5}`,
			"minItems 1", "minItems 5"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sub, super := normalize(t, tc.sub), normalize(t, tc.super)
			if sub.IsSubset(super) {
				t.Fatal("IsSubset accepted a narrowing: a schema change tightening this would be " +
					"reported compatible and then reject data at conform time")
			}
			brk := explainOne(t, sub, super)
			if brk.Sub != tc.wantSub || brk.Super != tc.wantSuper {
				t.Errorf("break reads %q → %q, want %q → %q — two sides that render the same "+
					"tell a reader nothing about what changed",
					brk.Sub, brk.Super, tc.wantSub, tc.wantSuper)
			}
		})
	}
}

// The PERMITTED POOL is capped: an enum has no size limit and a break becomes one line of a
// report. What was dropped is named separately and never truncated — see
// TestExplainSubset_EnumBreakNamesTheValuesThatNoLongerFit for why showing both pools failed.
func TestExplainSubset_LongEnumIsTruncated(t *testing.T) {
	big := `["v0","v1","v2","v3","v4","v5","v6","v7"]`
	brk := explainOne(t, normalize(t, `{"type":"string"}`),
		normalize(t, `{"type":"string","enum":`+big+`}`))
	if want := `enum ["v0", "v1", "v2", "v3", "v4", +3 more]`; brk.Super != want {
		t.Errorf("got %q, want %q — the cap keeps one break to one line, and the remainder count "+
			"is what stops a truncated pool reading as the whole of it", brk.Super, want)
	}
	// At the cap exactly, nothing is elided and no count appears — an off-by-one here would
	// claim "+0 more" or hide a value that fits.
	exact := explainOne(t, normalize(t, `{"type":"string"}`),
		normalize(t, `{"type":"string","enum":["a","b","c","d","e"]}`))
	if want := `enum ["a", "b", "c", "d", "e"]`; exact.Super != want {
		t.Errorf("got %q, want %q — a pool at the cap is rendered whole", exact.Super, want)
	}
}

// Which values stopped fitting, STATED rather than shown: a capped list cannot express an
// absence, since two pools differing by one value truncate to the same five entries.
func TestExplainSubset_EnumBreakNamesTheValuesThatNoLongerFit(t *testing.T) {
	for _, tc := range []struct{ name, sub, super, wantSub, wantSuper string }{
		{
			"one value dropped",
			`{"type":"string","enum":["a","b","c"]}`, `{"type":"string","enum":["a","b"]}`,
			`allows ["c"]`, `enum ["a", "b"]`,
		},
		{
			// The case that motivated this: both pools cap at the same five entries.
			"dropped from a pool that truncates on both sides",
			`{"type":"string","enum":["v0","v1","v2","v3","v4","v5","v6","v7"]}`,
			`{"type":"string","enum":["v0","v1","v2","v3","v4","v5","v6"]}`,
			`allows ["v7"]`, `enum ["v0", "v1", "v2", "v3", "v4", +2 more]`,
		},
		{
			"every dropped value is named, not just the first",
			`{"type":"string","enum":["a","b","c","d"]}`, `{"type":"string","enum":["a"]}`,
			`allows ["b", "c", "d"]`, `enum ["a"]`,
		},
		{
			"disjoint pools",
			`{"type":"string","enum":["a"]}`, `{"type":"string","enum":["z"]}`,
			`allows ["a"]`, `enum ["z"]`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			brk := explainOne(t, normalize(t, tc.sub), normalize(t, tc.super))
			if brk.Sub != tc.wantSub || brk.Super != tc.wantSuper {
				t.Errorf("break reads %q → %q, want %q → %q — a reader must be able to name the "+
					"value that stopped being accepted without diffing two truncated lists",
					brk.Sub, brk.Super, tc.wantSub, tc.wantSuper)
			}
		})
	}
}

// Addition and removal are the SAME edit read from two ends, and the two compat questions
// run in opposite directions — so one enum change is a break on exactly one of them.
func TestExplainSubset_EnumWideningBreaksOnlyTheReversedDirection(t *testing.T) {
	narrow := normalize(t, `{"type":"string","enum":["a"]}`)
	wide := normalize(t, `{"type":"string","enum":["a","b"]}`)

	if !narrow.IsSubset(wide) {
		t.Error("adding a permitted value cannot break a reader: every old value is still valid")
	}
	brk := explainOne(t, wide, narrow)
	if brk.Sub != `allows ["b"]` {
		t.Errorf("added value reported as %q, want %q — the contract break must name what is "+
			"newly produced, which is the same set difference read the other way", brk.Sub, `allows ["b"]`)
	}
}

// An enum member is a VALUE, not a literal. isSubset claims every value valid under sub is
// valid under super, and Validate decides "valid" — so this is differential, because
// agreement with Validate IS the property being tested.
func TestIsSubset_EnumMembershipAgreesWithValidate(t *testing.T) {
	for _, tc := range []struct {
		name, sub, super string
		samples          []string
	}{
		{"1 and 1.0 are one member", `{"enum":[1]}`, `{"enum":[1.0]}`, []string{`1`, `1.0`}},
		{"exponent spelling", `{"enum":[1e2]}`, `{"enum":[100]}`, []string{`100`, `1e2`}},
		{"integer inside a number pool", `{"enum":[2]}`, `{"enum":[1,2,3]}`, []string{`2`}},
		{"a value genuinely dropped", `{"enum":[1,2]}`, `{"enum":[1]}`, []string{`1`, `2`}},
		{"adjacent big integers stay distinct", `{"enum":[9007199254740993]}`,
			`{"enum":[9007199254740992]}`, []string{`9007199254740993`}},
		{"a number is not its string", `{"enum":[1]}`, `{"enum":["1"]}`, []string{`1`}},
		{"true is not 1", `{"enum":[true]}`, `{"enum":[1]}`, []string{`true`}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sub, super := normalize(t, tc.sub), normalize(t, tc.super)
			fits := sub.IsSubset(super)
			witness := false
			for _, raw := range tc.samples {
				v := mustData(t, raw)
				if _, err := sub.Validate(v); err != nil {
					continue // not a value sub admits; says nothing about the relation
				}
				_, superErr := super.Validate(v)
				if fits && superErr != nil {
					t.Errorf("IsSubset said yes but %s is rejected by super: %v — the relation "+
						"promises exactly what Validate will accept", raw, superErr)
				}
				witness = witness || superErr != nil
			}
			// A rejection needs a WITNESS: some value sub admits that super genuinely refuses.
			// "Some sample passes both" proves nothing in a pool that merely shrank.
			if !fits && !witness {
				t.Errorf("IsSubset said no, but every sampled value sub admits is accepted by " +
					"super — the break is spurious, and a report would call a compatible change breaking")
			}
		})
	}
}

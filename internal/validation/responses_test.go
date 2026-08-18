package validation

import (
	"encoding/json"
	"testing"

	"genroc/internal/model"
	"genroc/internal/schema"
	"genroc/internal/shape"
)

func mustSchema(t *testing.T, raw string) *schema.Schema {
	t.Helper()
	var s schema.Schema
	if err := json.Unmarshal([]byte(raw), &s); err != nil {
		t.Fatalf("parse schema %s: %v", raw, err)
	}
	return &s
}

// The nullability of self.result is the whole point of `responses`: a declared status set
// that covers everything accepted types exactly, and any gap between the two admits null.
// Each row here is a rule from specs/fetch-http-surface.md §2 that a refactor can silently
// invert, since an over-wide type only shows up as a downstream expression that stops
// compiling — or worse, one that keeps compiling and reads null.
func TestFetchResultType_NullabilityFollowsCoverage(t *testing.T) {
	obj := `{"type":"object","properties":{"state":{"type":"string"}}}`
	for _, tc := range []struct {
		name         string
		responses    map[string]*schema.Schema
		accepted     any
		wantNullable bool
		wantTyped    bool
	}{
		{
			name:      "a single declared status is exactly its schema",
			responses: map[string]*schema.Schema{"200": mustSchema(t, obj)},
			wantTyped: true,
		},
		{
			name:         "a bodyless sibling adds the null arm",
			responses:    map[string]*schema.Schema{"200": mustSchema(t, obj), "202": nil},
			wantNullable: true,
			wantTyped:    true,
		},
		{
			name:      "a range covers the whole default accepted set",
			responses: map[string]*schema.Schema{"2xx": mustSchema(t, obj)},
			wantTyped: true,
		},
		{
			name:         "accepting more than is described admits null",
			responses:    map[string]*schema.Schema{"200": mustSchema(t, obj)},
			accepted:     []any{"2xx"},
			wantNullable: true,
			wantTyped:    true,
		},
		{
			name:      "an error status is not on this channel at all",
			responses: map[string]*schema.Schema{"200": mustSchema(t, obj), "404": mustSchema(t, obj)},
			wantTyped: true,
		},
		{
			name:         "a runtime accepted set can always land on an undescribed status",
			responses:    map[string]*schema.Schema{"200": mustSchema(t, obj)},
			accepted:     "$: input.accepted",
			wantNullable: true,
			wantTyped:    true,
		},
		{
			name:      "no declaration is untyped, exactly as a fetch without result_schema was",
			responses: nil,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := &model.Action{Type: model.ActionTypeFetch, URL: "http://x", Responses: tc.responses}
			if tc.accepted != nil {
				a.AcceptedStatus = &model.Shape{Raw: tc.accepted}
			}
			got, typed, err := fetchResultType(a, schema.Defs{})
			if err != nil {
				t.Fatalf("fetchResultType: %v", err)
			}
			if typed != tc.wantTyped {
				t.Fatalf("typed = %v, want %v — an untyped result cannot be exported at all", typed, tc.wantTyped)
			}
			if !typed {
				return
			}
			if got.HasNull() != tc.wantNullable {
				t.Fatalf("nullable = %v, want %v (%s) — a spurious null forces every consumer through ??, "+
					"and a missing one lets an expression read a body that was never there",
					got.HasNull(), tc.wantNullable, mustJSON(t, got))
			}
		})
	}
}

// oneOf means EXACTLY one arm matches, so two status bodies that overlap — objects whose
// properties are all optional both admit {} — would make the union reject a value fitting
// both. The same mistake literal-types.md was written to fix.
func TestFetchResultType_MultiStatusUnionIsAnyOf(t *testing.T) {
	a := &model.Action{
		Type: model.ActionTypeFetch,
		URL:  "http://x",
		Responses: map[string]*schema.Schema{
			"200": mustSchema(t, `{"type":"object","properties":{"a":{"type":"string"}}}`),
			"202": mustSchema(t, `{"type":"object","properties":{"b":{"type":"string"}}}`),
		},
		AcceptedStatus: &model.Shape{Raw: []any{"200", "202"}},
	}
	got, _, err := fetchResultType(a, schema.Defs{})
	if err != nil {
		t.Fatalf("fetchResultType: %v", err)
	}
	raw := mustJSON(t, got)
	if !contains(raw, `"anyOf"`) || contains(raw, `"oneOf"`) {
		t.Fatalf("union = %s, want anyOf — overlapping status bodies make oneOf reject a value that fits two arms", raw)
	}
}

func mustJSON(t *testing.T, s schema.Schema) string {
	t.Helper()
	b, err := json.Marshal(s)
	if err != nil {
		t.Fatalf("marshal schema: %v", err)
	}
	return string(b)
}

func contains(haystack, needle string) bool {
	return len(haystack) >= len(needle) && (func() bool {
		for i := 0; i+len(needle) <= len(haystack); i++ {
			if haystack[i:i+len(needle)] == needle {
				return true
			}
		}
		return false
	})()
}

var _ = shape.Shape{}

// A fetch declares per status but produces ONE value, so the contract is the merged union.
// Comparing the statuses one at a time judges something no consumer can read, and it is
// wrong in both directions — these two cases are where, which is why the comparison is the
// union under the same `.result` address every other action type uses.
//
// The direction is the one every result schema runs, old ⊆ new: the schema is a demand on
// the party that PRODUCES the value, so demanding less (a wider union) turns nobody away
// while demanding more breaks the producer that satisfied the old one. What a downstream
// reader of self.result sees is a different question, answered by the task's own output
// comparison — the result only becomes visible outside the task through a projection.
func TestCompat_FetchResultIsComparedAsTheMergedUnion(t *testing.T) {
	def := func(responses string) string {
		return `{"name":"p","tasks":[{"id":"call","action":{"type":"fetch","url":"http://x",
			"responses":` + responses + `},"switch":"end"}]}`
	}
	body := `{"type":"object","properties":{"fee":{"type":"number"}},"required":["fee"]}`

	for _, tc := range []struct {
		name      string
		old, new  string
		wantBreak bool
		why       string
	}{
		{
			name: "dropping a bodyless status narrows what the remote may answer",
			old:  `{"200":` + body + `,"202":null}`,
			new:  `{"200":` + body + `}`,
			// Per status this is a key only the old side carries, which `pair` skips as a
			// removed declaration — so the break went unreported entirely.
			wantBreak: true,
			why:       "a remote that answered 202 with no body no longer conforms; per status this was silent",
		},
		{
			name: "restating the same union over a range changes nothing",
			old:  `{"200":` + body + `}`,
			new:  `{"2xx":` + body + `}`,
			// Per status this reads as one key removed and another added, and an added schema
			// where none was is `added` — a break reported for an edit that changed no type.
			wantBreak: false,
			why:       "both declare exactly this body for every accepted status; per status this was a spurious break",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			report, err := Compare(definitionFromJSON(t, def(tc.old)), definitionFromJSON(t, def(tc.new)))
			if err != nil {
				t.Fatalf("Compare: %v", err)
			}
			if got := len(report.Issues) > 0; got != tc.wantBreak {
				t.Fatalf("break = %v, want %v — %s (issues: %+v)", got, tc.wantBreak, tc.why, report.Issues)
			}
			if tc.wantBreak && report.Issues[0].Address != "call:fetch.result" {
				t.Errorf("address = %q, want \"call:fetch.result\": a status is not separately readable, so it is not separately addressed",
					report.Issues[0].Address)
			}
		})
	}
}

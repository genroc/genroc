package model

import (
	"encoding/json"
	"strings"
	"testing"

	"genroc/internal/schema"
)

func responsesDef(t *testing.T, actionJSON string) *ProcessDefinition {
	t.Helper()
	var d ProcessDefinition
	src := `{"name":"p","tasks":[{"id":"call","action":` + actionJSON + `,"switch":"end"}]}`
	if err := json.Unmarshal([]byte(src), &d); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return &d
}

// Every refusal `responses` can produce, with the wording that tells the author what to do
// instead. A rule that stops firing is silent — the definition is simply accepted and does
// something other than what it says — so each case pins both the rejection and its way out.
func TestValidate_Responses(t *testing.T) {
	body := `{"type":"object","properties":{"fee":{"type":"number"}}}`
	for _, tc := range []struct {
		name, action, wantErr, wantHint string
	}{
		{
			name:   "a status declared twice across keys",
			action: `{"type":"fetch","url":"http://x","responses":{"400, 401":` + body + `,"401, 402":` + body + `}}`,
			// Two schemas for one status has no answer; picking one silently would make the
			// report a guess about which the author meant.
			wantErr: `declares "401" twice`, wantHint: "one status, one schema",
		},
		{
			name:    "a status repeated inside one key",
			action:  `{"type":"fetch","url":"http://x","responses":{"400, 400":` + body + `}}`,
			wantErr: `"400" is listed twice`,
		},
		{
			name:    "a key mixing success and failure statuses",
			action:  `{"type":"fetch","url":"http://x","responses":{"200, 404":` + body + `}}`,
			wantErr: "mixes success and failure statuses", wantHint: "split it into two keys",
		},
		{
			name:    "a malformed pattern",
			action:  `{"type":"fetch","url":"http://x","responses":{"2xxx":` + body + `}}`,
			wantErr: "is not a status pattern", wantHint: `hundred-range`,
		},
		{
			name:    "an empty element in a list",
			action:  `{"type":"fetch","url":"http://x","responses":{"400,":` + body + `}}`,
			wantErr: "empty status pattern",
		},
		{
			name:    "responses on an action with no status to key on",
			action:  `{"type":"external","responses":{"200":` + body + `}}`,
			wantErr: "only valid on a fetch", wantHint: "use result_schema",
		},
		{
			name: "result_schema on a fetch",
			// Not a compatibility shim: nothing reads the field on a fetch, and a field
			// nothing reads is dropped in silence.
			action:  `{"type":"fetch","url":"http://x","result_schema":` + body + `}`,
			wantErr: "not valid on a fetch", wantHint: `responses: {"200": {...}}`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := responsesDef(t, tc.action).Validate()
			if err == nil {
				t.Fatalf("expected a refusal, got none")
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("error %q does not contain %q", err, tc.wantErr)
			}
			if tc.wantHint != "" && !strings.Contains(err.Error(), tc.wantHint) {
				t.Errorf("error %q does not tell the author what to do next (%q)", err, tc.wantHint)
			}
		})
	}

	// The shapes that must stay legal, so the rules above cannot drift into refusing them.
	for _, ok := range []string{
		`{"type":"fetch","url":"http://x","responses":{"200":` + body + `,"404":` + body + `}}`,
		`{"type":"fetch","url":"http://x","responses":{"400, 401":` + body + `,"5xx":` + body + `}}`,
		`{"type":"fetch","url":"http://x","responses":{"202":null}}`,
		`{"type":"fetch","url":"http://x","responses":{"404":` + body + `,"4xx":` + body + `}}`,
		`{"type":"external","result_schema":` + body + `}`,
	} {
		if err := responsesDef(t, ok).Validate(); err != nil {
			t.Errorf("%s should be accepted: %v", ok, err)
		}
	}
}

// Exact beats range, per pattern. The two keys overlap on 404 and are NOT an equal-specificity
// collision, so both must be accepted and 404 must resolve to the exact one — a resolver that
// took either "first" would be order-dependent over a Go map.
func TestResponseFor_ExactBeatsRange(t *testing.T) {
	exact := schema.Object().WithProperty("exact", schema.Type("boolean"), true)
	wide := schema.Object().WithProperty("wide", schema.Type("boolean"), true)
	a := &Action{
		Type: ActionTypeFetch,
		Responses: map[string]*schema.Schema{
			"404": &exact,
			"4xx": &wide,
			"5xx": nil,
			"200": &wide,
		},
	}
	for _, tc := range []struct {
		code     int
		wantProp string
		wantNil  bool
		declared bool
	}{
		{code: 404, wantProp: "exact", declared: true},
		{code: 409, wantProp: "wide", declared: true},
		{code: 503, wantNil: true, declared: true},
		{code: 302, declared: false},
	} {
		sc, declared := a.ResponseFor(tc.code)
		if declared != tc.declared {
			t.Errorf("ResponseFor(%d) declared = %v, want %v", tc.code, declared, tc.declared)
			continue
		}
		if !declared {
			continue
		}
		if tc.wantNil {
			if sc != nil {
				t.Errorf("ResponseFor(%d) = %v, want the bodyless entry", tc.code, sc)
			}
			continue
		}
		if sc == nil {
			t.Fatalf("ResponseFor(%d) lost the schema — a nil here is the bodyless entry, a different claim", tc.code)
		}
		if _, err := sc.Property(tc.wantProp); err != nil {
			t.Errorf("ResponseFor(%d) picked the wrong key: wanted the one declaring %q", tc.code, tc.wantProp)
		}
	}
}

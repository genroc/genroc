package model

import (
	"encoding/json"
	"strings"
	"testing"
)

// A dropped key in a rule is not a cosmetic problem. An on_error rule whose "code" went
// missing becomes a catch-all — the broadest shape there is — and the author is then told
// about a rule they did not write. These are the two lists whose selector keys are easy to
// swap, so each rejects the other's.
func TestRuleDecode_UnknownFields(t *testing.T) {
	tests := []struct {
		name     string
		json     string
		into     json.Unmarshaler
		wantErr  string
		wantHint string
	}{
		{
			name:     "on_error given a switch's case",
			json:     `{"case": ["pre.4%"], "not_reached": true, "goto": "$wait", "retry": 3}`,
			into:     &ErrorCase{},
			wantErr:  `unknown field "case"`,
			wantHint: `an on_error rule selects errors with "code"`,
		},
		{
			name:    "on_error given a stray key",
			json:    `{"code": ["http.500"], "next": "end"}`,
			into:    &ErrorCase{},
			wantErr: `unknown field "next"`,
		},
		{
			name:     "switch given an on_error's code",
			json:     `[{"code": ["http.500"], "goto": "end"}]`,
			into:     &SwitchMap{},
			wantErr:  `unknown field "code"`,
			wantHint: `a switch case selects with "case"`,
		},
		{
			name:    "switch given a stray key",
			json:    `[{"case": "x == 1", "retry": 2, "goto": "end"}]`,
			into:    &SwitchMap{},
			wantErr: `unknown field "retry"`,
		},
		{
			// The rename is only safe because the old key is refused: dropped in silence it
			// leaves a rule that still matches and still routes, and never retries.
			name:     "on_error given the pre-policy retries key",
			json:     `{"code": ["http.500"], "retries": 3}`,
			into:     &ErrorCase{},
			wantErr:  `unknown field "retries"`,
			wantHint: `renamed to "retry"`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.into.UnmarshalJSON([]byte(tt.json))
			if err == nil {
				t.Fatalf("accepted a rule with an unknown field: %s", tt.json)
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("error %q does not contain %q", err, tt.wantErr)
			}
			if tt.wantHint != "" && !strings.Contains(err.Error(), tt.wantHint) {
				t.Errorf("error %q does not point at the right key (%q)", err, tt.wantHint)
			}
		})
	}
}

// The false-positive half: every field each rule does define still decodes.
func TestRuleDecode_KnownFieldsSurvive(t *testing.T) {
	var ec ErrorCase
	if err := ec.UnmarshalJSON([]byte(
		`{"code": ["http.409"], "retry": 2, "goto": "$handler", "not_reached": true}`)); err != nil {
		t.Fatalf("on_error: %v", err)
	}
	if len(ec.Code) != 1 || ec.Retry.Attempts != 2 || ec.Goto != "handler" || ec.NotReached == nil || !*ec.NotReached {
		t.Fatalf("on_error decoded to %+v", ec)
	}
	if err := (&ErrorCase{}).UnmarshalJSON([]byte(
		`{"raise": {"code": "x", "message": "m"}}`)); err != nil {
		t.Fatalf("on_error raise: %v", err)
	}
	if err := (&ErrorCase{}).UnmarshalJSON([]byte(
		`{"panic": {"code": "x", "message": "m"}}`)); err != nil {
		t.Fatalf("on_error panic: %v", err)
	}

	var sm SwitchMap
	if err := sm.UnmarshalJSON([]byte(
		`[{"case": "self.ok", "goto": "$next_task"}, {"raise": {"code": "x", "message": "m"}}, {"goto": "end"}]`)); err != nil {
		t.Fatalf("switch: %v", err)
	}
	if len(sm) != 3 || sm[0].Case != "self.ok" || sm[1].Raise == nil || sm[2].Goto != GotoEnd {
		t.Fatalf("switch decoded to %+v", sm)
	}
	// The scalar shorthand has no keys to check and must keep working.
	if err := (&SwitchMap{}).UnmarshalJSON([]byte(`"next"`)); err != nil {
		t.Fatalf("switch shorthand: %v", err)
	}
}

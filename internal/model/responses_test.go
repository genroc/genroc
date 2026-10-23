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

// Each case pins both the rejection and its way out: a rule that stops firing is silent.
func TestValidate_Responses(t *testing.T) {
	body := `{"type":"object","properties":{"fee":{"type":"number"}}}`
	for _, tc := range []struct {
		name, action, wantErr, wantHint string
	}{
		{
			name:   "a status declared twice across keys",
			action: `{"type":"fetch","method":"post","url":"http://x","responses":{"400, 401":` + body + `,"401, 402":` + body + `}}`,
			// Two schemas for one status has no answer; picking one silently would make the
			// report a guess about which the author meant.
			wantErr: `declares "401" twice`, wantHint: "one status, one schema",
		},
		{
			name:    "a status repeated inside one key",
			action:  `{"type":"fetch","method":"post","url":"http://x","responses":{"400, 400":` + body + `}}`,
			wantErr: `"400" is listed twice`,
		},
		{
			name:    "a key mixing success and failure statuses",
			action:  `{"type":"fetch","method":"post","url":"http://x","responses":{"200, 404":` + body + `}}`,
			wantErr: "mixes success and failure statuses", wantHint: "split it into two keys",
		},
		{
			name:    "a malformed pattern",
			action:  `{"type":"fetch","method":"post","url":"http://x","responses":{"2xxx":` + body + `}}`,
			wantErr: "is not a status pattern", wantHint: `hundred-range`,
		},
		{
			name:    "an empty element in a list",
			action:  `{"type":"fetch","method":"post","url":"http://x","responses":{"400,":` + body + `}}`,
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
			action:  `{"type":"fetch","method":"post","url":"http://x","result_schema":` + body + `}`,
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
		`{"type":"fetch","method":"post","url":"http://x","responses":{"200":` + body + `,"404":` + body + `}}`,
		`{"type":"fetch","method":"post","url":"http://x","responses":{"400, 401":` + body + `,"5xx":` + body + `}}`,
		`{"type":"fetch","method":"post","url":"http://x","responses":{"202":null}}`,
		`{"type":"fetch","method":"post","url":"http://x","responses":{"404":` + body + `,"4xx":` + body + `}}`,
		`{"type":"external","result_schema":` + body + `}`,
	} {
		if err := responsesDef(t, ok).Validate(); err != nil {
			t.Errorf("%s should be accepted: %v", ok, err)
		}
	}
}

// Overlapping on 404 is NOT an equal-specificity collision; a resolver taking either "first"
// would be order-dependent over a Go map.
func TestResponseFor_ExactBeatsRange(t *testing.T) {
	exact := schema.Object().WithProperty("exact", schema.Type("boolean"), true)
	wide := schema.Object().WithProperty("wide", schema.Type("boolean"), true)
	a := &Action{
		Type:   ActionTypeFetch,
		Method: "post",
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

// Off a fetch these slots are read by nobody, so accepting one would drop it in silence.
func TestValidate_FetchOnlySlots(t *testing.T) {
	for _, slot := range []string{
		`"url":"http://x"`,
		`"method":"get"`,
		`"headers":{"a":"1"}`,
		`"query":{"a":"1"}`,
		`"accepted_status":["200"]`,
	} {
		for _, actionType := range []string{"external", "child", "delay"} {
			extra := ""
			switch actionType {
			case "child":
				extra = `,"name":"k"`
			case "delay":
				extra = `,"for":"1s"`
			}
			action := `{"type":"` + actionType + `",` + slot + extra + `}`
			err := responsesDef(t, action).Validate()
			if err == nil {
				t.Errorf("%s on a %s task must be refused — it would be ignored", slot, actionType)
				continue
			}
			if !strings.Contains(err.Error(), "only valid on a fetch") {
				t.Errorf("%s on a %s: message should say where the slot belongs, got %v", slot, actionType, err)
			}
		}
	}
	// And they stay legal on the action that reads them.
	ok := `{"type":"fetch","url":"http://x","method":"get","headers":{"a":"1"},"query":{"b":"2"},"accepted_status":["200"]}`
	if err := responsesDef(t, ok).Validate(); err != nil {
		t.Errorf("a fetch carrying all of them must be accepted: %v", err)
	}
}

// The child-family slots, likewise: the editor schema refuses each off its owners, and the server
// must too, or a field the editor marks wrong is accepted and silently never read.
func TestValidate_ChildSlotsOffTheirOwners(t *testing.T) {
	for _, tc := range []struct{ action, want string }{
		{`{"type":"child_list","name":"k","over":"$: []","input":{"a":1}}`, "each element of `over`"},
		{`{"type":"child_list","name":"k","over":"$: []","children":{"a":{"name":"k"}}}`, `action.children is not valid on a "child_list" task`},
		{`{"type":"child_map","children":{"a":{"name":"k"}},"name":"k"}`, "children[<key>].name"},
		{`{"type":"child_map","children":{"a":{"name":"k"}},"result_schema":{"type":"object"}}`, "children[<key>].result_schema"},
		{`{"type":"child_map","children":{"a":{"name":"k"}},"input":{"a":1}}`, "children[<key>].input"},
		{`{"type":"child","name":"k","over":"$: []"}`, `action.over is not valid on a "child" task`},
		{`{"type":"fetch","method":"get","url":"http://x","input":{"a":1}}`, "a fetch sends a `body`"},
		{`{"type":"external","name":"k"}`, `action.name is not valid on a "external" task`},
		{`{"type":"delay","for":"1s","result_schema":{"type":"object"}}`, "does not read it"},
	} {
		err := responsesDef(t, tc.action).Validate()
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: want a refusal naming %q, got %v", tc.action, tc.want, err)
		}
	}
	for _, ok := range []string{
		`{"type":"child","name":"k","version":2,"input":{"a":1},"result_schema":{"type":"object"}}`,
		`{"type":"child_list","name":"k","version":2,"over":"$: []","result_schema":{"type":"object"}}`,
		`{"type":"external","input":{"a":1},"result_schema":{"type":"object"}}`,
	} {
		if err := responsesDef(t, ok).Validate(); err != nil {
			t.Errorf("%s: every slot here is its owner's: %v", ok, err)
		}
	}
}

// The transport never sends a body with GET or HEAD, so a literal one is a body dropped in silence.
func TestValidate_BodyOnAMethodThatSendsNone(t *testing.T) {
	for _, method := range []string{"get", "GET", "head"} {
		err := responsesDef(t, `{"type":"fetch","url":"http://x","method":"`+method+`","body":{"a":1}}`).Validate()
		if err == nil || !strings.Contains(err.Error(), "never sent with") {
			t.Errorf("method %s: want the body refused, got %v", method, err)
		}
	}
	for _, ok := range []string{
		`{"type":"fetch","url":"http://x","method":"post","body":{"a":1}}`,
		`{"type":"fetch","url":"http://x","method":"get"}`,
		// Known only at runtime, so nothing here can say it will be GET.
		`{"type":"fetch","url":"http://x","method":"$: 'get'","body":{"a":1}}`,
	} {
		if err := responsesDef(t, ok).Validate(); err != nil {
			t.Errorf("%s: %v", ok, err)
		}
	}
}

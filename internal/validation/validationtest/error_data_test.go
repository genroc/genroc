package validationtest

import (
	"strings"
	"testing"
)

const problemSchema = `{"type":"object","properties":{"detail":{"type":"string"}},"required":["detail"]}`

func errDataDef(responses, codes, handlerOutput string) string {
	return `{"name":"p","tasks":[
		{"id":"call","action":{"type":"fetch","url":"http://x","responses":` + responses + `},
		 "on_error":[{"code":` + codes + `,"goto":"$handler"}],"switch":"end"},
		{"id":"handler","output":` + handlerOutput + `,"switch":"end"}
	]}`
}

// A status declared on the error side is readable at the handler its rule routes to, and it
// is readable WITHOUT a null check when the rule can catch nothing else — that narrowing is
// done by the on_error patterns themselves, which is what lets the design need none of the
// deferred discriminated-union work.
func TestGenerate_ErrorData_TypedByTheRuleThatCaughtIt(t *testing.T) {
	out := runGenerate(t, errDataDef(
		`{"200":{"type":"object"},"404":`+problemSchema+`}`,
		`["http.404"]`,
		`{"d":"$: error.data.detail"}`,
	))
	handler, ok := out.Defs.Get("handler_output")
	if !ok {
		t.Fatal("no handler_output schema")
	}
	assertJSON(t, handler, `{"type":"object","properties":{"d":{"type":"string"}},"required":["d"]}`)
}

// Widening the rule widens the type. The union is computed from the on_error patterns, not
// from the responses map, so the moment a pattern can also catch a status nobody described —
// or a code that carries no body at all — error.data admits null. That is the narrowing
// story the design rests on: done by the rules, with no discriminated-union machinery.
func TestGenerate_ErrorData_WidensWithTheRule(t *testing.T) {
	problem := `{"type":"object","properties":{"detail":{"type":"string"}},"required":["detail"]}`
	nullable := `{"oneOf":[{"type":"null"},` + problem + `]}`
	for _, tc := range []struct {
		name, codes, want, why string
	}{
		{
			name: "one status, one schema", codes: `["http.404"]`, want: problem,
			why: "nothing else can arrive here, so nothing widens it",
		},
		{
			name: "a range also catches statuses nobody declared", codes: `["http.4%"]`, want: nullable,
			why: "http.409 is caught and undeclared, so the body may not be a problem document",
		},
		{
			name: "a code that carries no response at all", codes: `["http.404","pre.error"]`, want: nullable,
			why: "a dial failure routes here with nothing in hand",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out := runGenerate(t, errDataDef(
				`{"200":{"type":"object"},"404":`+problem+`}`,
				tc.codes,
				`{"d":"$: error.data"}`,
			))
			handler, ok := out.Defs.Get("handler_output")
			if !ok {
				t.Fatal("no handler_output schema")
			}
			assertJSON(t, handler, `{"type":"object","properties":{"d":`+tc.want+`},"required":["d"]}`)
			_ = tc.why
		})
	}
}

// A task no error edge reaches has no `error` at all, so error.data is not merely null there
// — it does not exist, and reading it is the same "not in schema" error every undeclared
// value gets.
func TestGenerate_ErrorData_AbsentWhereNoRuleReaches(t *testing.T) {
	err := runGenerateErr(t, `{"name":"p","tasks":[
		{"id":"call","action":{"type":"fetch","url":"http://x","responses":{"200":{"type":"object"}}},
		 "output":{"d":"$: error.data"},"switch":"end"}
	]}`)
	if err == nil {
		t.Fatal("error.data must not exist on a task no on_error rule routes to")
	}
}

// A fetch that declares no error status leaves error.data absent even at a handler its rules
// reach: undeclared data is never accessible, and a body nobody described is undeclared.
func TestGenerate_ErrorData_AbsentWithoutAnErrorDeclaration(t *testing.T) {
	err := runGenerateErr(t, errDataDef(
		`{"200":{"type":"object"}}`,
		`["http.404"]`,
		`{"d":"$: error.data"}`,
	))
	if err == nil {
		t.Fatal("error.data must be absent when no reaching rule declares a body")
	}
	if !strings.Contains(err.Error(), "data") {
		t.Errorf("the error should name the slot that is missing, got: %v", err)
	}
}

// `error` is scoped to the task its rule routes to. A task reached from the handler by an
// ordinary transition is not an error handler, so `error` does not exist there at all — the
// engine drops it on that transition, and typing it would promise a value the context no
// longer holds. A handler that wants the failure to travel projects it into its output.
func TestGenerate_ErrorScope_EndsAtTheHandler(t *testing.T) {
	err := runGenerateErr(t, `{"name":"p","tasks":[
		{"id":"call","action":{"type":"fetch","url":"http://x","responses":{"200":{"type":"object"}}},
		 "on_error":[{"code":["http.%"],"goto":"$handler"}],"switch":"next"},
		{"id":"handler","output":{"c":"$: error.code"},"switch":"next"},
		{"id":"after","output":{"c":"$: error.code"},"switch":"end"}
	]}`)
	if err == nil {
		t.Fatal("error.code must not resolve one transition past the handler")
	}
	if !strings.Contains(err.Error(), "after") {
		t.Fatalf("the failure should name the downstream task, not the handler: %v", err)
	}

	// Projecting it into an output is how a later task gets it.
	if err := runGenerateErr(t, `{"name":"p","tasks":[
		{"id":"call","action":{"type":"fetch","url":"http://x","responses":{"200":{"type":"object"}}},
		 "on_error":[{"code":["http.%"],"goto":"$handler"}],"switch":"next"},
		{"id":"handler","output":{"c":"$: error.code"},"switch":"next"},
		{"id":"after","output":{"c":"$: outputs.handler.c"},"switch":"end"}
	]}`); err != nil {
		t.Errorf("projecting the failure into an output must carry it forward: %v", err)
	}
}

// self.result's nullability follows the gap between what a fetch ACCEPTS and what it
// DESCRIBES. The rule is unit-tested against fetchResultType directly; this pins it through
// Generate, where a miswired branch in actionResultType would otherwise leave the unit tests
// green while every definition types its result wrong.
func TestGenerate_FetchResult_NullabilityThroughGenerate(t *testing.T) {
	body := `{"type":"object","properties":{"fee":{"type":"number"}},"required":["fee"]}`
	for _, tc := range []struct {
		name, responses, accepted, want string
	}{
		{
			name: "declared set covers the accepted set", responses: `{"200":` + body + `}`,
			want: body,
		},
		{
			// A nullable single body is spelled `oneOf` — the codebase's existing nullable
			// form, and sound here because null and a body are disjoint. Only a union of two
			// BODIES needs `anyOf`, where the arms can overlap.
			name:      "a bodyless sibling adds the null arm",
			responses: `{"200":` + body + `,"202":null}`,
			want:      `{"oneOf":[{"type":"null"},` + body + `]}`,
		},
		{
			name:      "a range covers every 2xx without enumerating them",
			responses: `{"2xx":` + body + `}`,
			want:      body,
		},
		{
			name:      "accepting more than is described admits null",
			responses: `{"200":` + body + `}`, accepted: `,"accepted_status":["2xx"]`,
			want: `{"oneOf":[{"type":"null"},` + body + `]}`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out := runGenerate(t, `{"name":"p","tasks":[
				{"id":"call","action":{"type":"fetch","url":"http://x","responses":`+tc.responses+tc.accepted+`},
				 "output":"$: self.result","switch":"end"}
			]}`)
			sc, ok := out.Defs.Get("call_output")
			if !ok {
				t.Fatal("no call_output schema")
			}
			assertJSON(t, sc, tc.want)
		})
	}
}

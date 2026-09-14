package model

import (
	"encoding/json"
	"testing"
)

// The scalar shorthand desugars to `for` at decode, so nothing downstream branches on which
// form was written — and the stored definition is the canonical object either way.
func TestTimeout_DecodeForms(t *testing.T) {
	tests := []struct {
		name      string
		in        string
		wantFor   any
		wantUntil any
		wantTZ    string
		wantErr   string
	}{
		{name: "duration shorthand", in: `"30s"`, wantFor: "30s"},
		{name: "bare number shorthand", in: `5000`, wantFor: float64(5000)},
		{name: "expression shorthand", in: `"$: input.budget_ms"`, wantFor: "$: input.budget_ms"},
		{name: "object for", in: `{"for":"2h30m"}`, wantFor: "2h30m"},
		{name: "object for with tz", in: `{"for":"1d","tz":"Europe/Prague"}`, wantFor: "1d", wantTZ: "Europe/Prague"},
		{name: "object until", in: `{"until":"fri 17:00"}`, wantUntil: "fri 17:00"},
		{name: "null is absent", in: `null`},

		// The whole reason the object form rejects unknown keys: each of these decodes to an
		// empty timeout otherwise, and an empty timeout is silently no timeout at all.
		{name: "typo'd slot", in: `{"untill":"fri 17:00"}`, wantErr: "untill"},
		{name: "the removed timeout_ms", in: `{"timeout_ms":5000}`, wantErr: "timeout_ms"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var got Timeout
			err := json.Unmarshal([]byte(tt.in), &got)
			if tt.wantErr != "" {
				if err == nil {
					t.Fatalf("expected an error naming %q, got nil", tt.wantErr)
				}
				if !containsStr(err.Error(), tt.wantErr) {
					t.Fatalf("error %q does not name the offending key %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got.For != tt.wantFor || got.Until != tt.wantUntil || got.TZ != tt.wantTZ {
				t.Errorf("decoded {for:%v until:%v tz:%q}, want {for:%v until:%v tz:%q}",
					got.For, got.Until, got.TZ, tt.wantFor, tt.wantUntil, tt.wantTZ)
			}
		})
	}
}

// SaveDefinition stores json.Marshal of the decoded struct, so the shorthand must survive
// a round trip as the same timeout — canonicalized, not lost.
func TestTimeout_RoundTripsCanonically(t *testing.T) {
	var got Timeout
	if err := json.Unmarshal([]byte(`"30s"`), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	out, err := json.Marshal(got)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	if string(out) != `{"for":"30s"}` {
		t.Fatalf("shorthand re-encoded as %s, want the canonical object form", out)
	}

	var again Timeout
	if err := json.Unmarshal(out, &again); err != nil {
		t.Fatalf("re-decode: %v", err)
	}
	if again.For != "30s" {
		t.Errorf("round trip lost the duration: for=%v", again.For)
	}
}

// An absent timeout must stay absent on the wire. Marshalling it as anything else would
// hand the engine a timeout of zero, which it rejects — turning "no deadline" into a task
// that can never run.
func TestTimeout_AbsentOmittedFromDefinition(t *testing.T) {
	task := Task{ID: "t", Switch: SwitchMap{{Goto: GotoEnd}}}
	out, err := json.Marshal(task)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	if containsStr(string(out), "timeout") {
		t.Errorf("an absent timeout was serialized: %s", out)
	}
}

// Action embeds DelaySpec, so a decoder promoted from it would be handed the whole action
// object and every other field would decode to nothing. This is the regression test for
// that: if DelaySpec ever gains an UnmarshalJSON, url and type below go empty.
func TestAction_DelaySpecDoesNotHijackDecode(t *testing.T) {
	var a Action
	if err := json.Unmarshal([]byte(`{"type":"fetch","method":"post","url":"http://x/y","for":"1h"}`), &a); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if a.Type != ActionTypeFetch || a.URL != "http://x/y" {
		t.Fatalf("embedded DelaySpec swallowed the action: type=%q url=%q", a.Type, a.URL)
	}
	if a.For != "1h" {
		t.Errorf("delay slot did not decode flat: for=%v", a.For)
	}
}

// Which action types honour a timeout, and where `until` is legal. Both rules exist because
// the alternative is silent: a timeout on a child task is simply never applied, and an
// `until` on a fetch reports http.timeout for a request that was never sent. A timeout with
// no call to bound needs no case: the slot lives on the action, so there is nowhere to write
// one — which is why the field sits there rather than on the task.
func TestProcessDefinition_Validate_Timeout(t *testing.T) {
	def := func(a *Action, timeout Timeout) ProcessDefinition {
		withTimeout := *a
		withTimeout.Timeout = timeout
		return ProcessDefinition{Name: "p", Tasks: []*Task{
			{ID: "t", Action: &withTimeout, Switch: SwitchMap{{Goto: GotoEnd}}},
		}}
	}
	fetch := &Action{Type: ActionTypeFetch, Method: "post", URL: "http://x/y"}
	external := &Action{Type: ActionTypeExternal}
	child := &Action{Type: ActionTypeChild, Name: "c"}
	delay := &Action{Type: ActionTypeDelay, DelaySpec: DelaySpec{For: "1h"}}

	tests := []struct {
		name    string
		action  *Action
		timeout Timeout
		wantErr string
	}{
		{name: "fetch with a duration", action: fetch, timeout: TimeoutFor("30s")},
		{name: "fetch with a bare number", action: fetch, timeout: TimeoutFor(float64(5000))},
		{name: "external with a duration", action: external, timeout: TimeoutFor("1h")},
		{name: "external with an until", action: external, timeout: Timeout{DelaySpec{Until: "fri 17:00"}}},
		{name: "child with no timeout", action: child},
		{name: "delay with no timeout", action: delay},

		{
			name: "fetch with an until", action: fetch, timeout: Timeout{DelaySpec{Until: "fri 17:00"}},
			wantErr: "only valid on an external task",
		},
		{name: "child with a timeout", action: child, timeout: TimeoutFor("30s"), wantErr: "not honoured on a \"child\" task"},
		{name: "delay with a timeout", action: delay, timeout: TimeoutFor("30s"), wantErr: "not honoured on a \"delay\" task"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := def(tt.action, tt.timeout)
			err := d.Validate()
			if tt.wantErr == "" {
				if err != nil {
					t.Errorf("unexpected error: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("expected an error containing %q, got nil", tt.wantErr)
			}
			if !containsStr(err.Error(), tt.wantErr) {
				t.Errorf("error %q does not contain %q", err.Error(), tt.wantErr)
			}
		})
	}
}

// `Action` embeds DelaySpec, so `for` / `until` / `tz` decode on EVERY action type and are
// then read by nobody — a deadline the author wrote that never applies. The editor schema's
// variants have always refused them; this is the server agreeing.
func TestProcessDefinition_Validate_DelaySlotsAreDelayOnly(t *testing.T) {
	def := func(a Action) ProcessDefinition {
		return ProcessDefinition{Name: "p", Tasks: []*Task{
			{ID: "t", Action: &a, Switch: SwitchMap{{Goto: GotoEnd}}},
		}}
	}
	fetch := Action{Type: ActionTypeFetch, Method: "post", URL: "http://x/y"}
	external := Action{Type: ActionTypeExternal}

	for _, tt := range []struct {
		name    string
		action  Action
		wantErr string
	}{
		{name: "a delay still takes for", action: Action{Type: ActionTypeDelay, DelaySpec: DelaySpec{For: "1h"}}},
		{name: "a delay still takes until and tz", action: Action{Type: ActionTypeDelay, DelaySpec: DelaySpec{Until: "fri 17:00", TZ: "Europe/Prague"}}},
		{name: "a fetch keeps its own timeout", action: func() Action { a := fetch; a.Timeout = TimeoutFor("30s"); return a }()},

		{name: "for on a fetch", action: func() Action { a := fetch; a.For = "1h"; return a }(), wantErr: `action.for is only valid on a delay`},
		{name: "until on a fetch", action: func() Action { a := fetch; a.Until = "fri 17:00"; return a }(), wantErr: `action.until is only valid on a delay`},
		{name: "tz on a fetch", action: func() Action { a := fetch; a.TZ = "Europe/Prague"; return a }(), wantErr: `action.tz is only valid on a delay`},
		{name: "for on an external", action: func() Action { a := external; a.For = "1h"; return a }(), wantErr: `action.for is only valid on a delay`},
	} {
		t.Run(tt.name, func(t *testing.T) {
			d := def(tt.action)
			err := d.Validate()
			if tt.wantErr == "" {
				if err != nil {
					t.Errorf("unexpected error: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("a %q slot on a %q action is ignored at runtime, so it must be refused here", tt.name, tt.action.Type)
			}
			if !containsStr(err.Error(), tt.wantErr) {
				t.Errorf("error %q does not contain %q", err.Error(), tt.wantErr)
			}
			if !containsStr(err.Error(), "timeout") {
				t.Errorf("error %q must name the slot that does work here", err.Error())
			}
		})
	}
}

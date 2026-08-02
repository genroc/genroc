package model

import (
	"strings"
	"testing"
)

// Retries on an only_once task are governed by three tiers, and the whole point of the
// rules is that an author who hits one is told what to do next. So each row asserts the
// message too, not just the verdict — a rejection that does not name the way forward is a
// failure of this feature even when the verdict is right.
//
// The accepting rows matter as much as the rejecting ones: this validation is the kind
// that quietly over-triggers, and a false positive here means a legitimate retry policy
// cannot be expressed at all.
func TestValidateOnError_OnlyOnceRetries(t *testing.T) {
	yes := true
	def := func(onlyOnce bool, ec ErrorCase) ProcessDefinition {
		task := &Task{
			ID:      "charge",
			Action:  &Action{Type: ActionTypeFetch, URL: "http://x"},
			Switch:  SwitchMap{{Goto: GotoEnd}},
			OnError: []ErrorCase{ec},
		}
		if onlyOnce {
			task.OnlyOnce = &yes
		}
		return ProcessDefinition{Name: "p", Tasks: []*Task{task, {
			ID: "handler", Action: &Action{Type: ActionTypeFetch, URL: "http://x"},
			Switch: SwitchMap{{Goto: GotoEnd}},
		}}}
	}

	tests := []struct {
		name string
		ec   ErrorCase
		// plain runs the same rule on a task without only_once, which must always
		// accept: none of these tiers exist for an idempotent task.
		wantErr  string
		wantHint string // an additional substring the message must carry
	}{
		// ── tier 1: pre.* is safe on its own, no assertion needed ────────────
		{name: "pre.% with retries", ec: ErrorCase{Code: []string{"pre.%"}, Retries: 2}},
		{name: "exact pre codes with retries", ec: ErrorCase{Code: []string{"pre.timeout", "pre.error"}, Retries: 2}},
		{name: "pre.% with a redundant not_reached", ec: ErrorCase{Code: []string{"pre.%"}, NotReached: &yes, Retries: 2}},

		// ── tier 2: a named exception, asserted with not_reached ─────────────
		{name: "exact http code with not_reached", ec: ErrorCase{Code: []string{"http.409"}, NotReached: &yes, Retries: 2}},
		{name: "several exact codes with not_reached", ec: ErrorCase{Code: []string{"http.409", "http.422"}, NotReached: &yes, Retries: 2}},
		{
			// Per-pattern tiers: the self-evidently safe wildcard and the named
			// exception coexist, neither spoiling the other.
			name: "pre.% alongside a named exception",
			ec:   ErrorCase{Code: []string{"pre.%", "http.409"}, NotReached: &yes, Retries: 2},
		},

		// ── not a retry at all: catching is always allowed ───────────────────
		{name: "unknowable codes caught with a goto", ec: ErrorCase{Code: []string{"only_once.interrupted", "http.timeout"}, Goto: "handler"}},
		{name: "bare wildcard caught with a goto", ec: ErrorCase{Code: []string{"%"}, Goto: "handler"}},
		{name: "catch-all caught with a goto", ec: ErrorCase{Goto: "handler"}},
		{name: "wildcard with retries:0", ec: ErrorCase{Code: []string{"http.%"}}},

		// ── tier 1 violation: needs an assertion ─────────────────────────────
		{
			name:     "http.% with retries and no assertion",
			ec:       ErrorCase{Code: []string{"http.%"}, Retries: 2},
			wantErr:  `pattern "http.%" can match errors where the call may have executed`,
			wantHint: "add not_reached:true and name the exact codes",
		},
		{
			name:     "exact http code with retries and no assertion",
			ec:       ErrorCase{Code: []string{"http.500"}, Retries: 2},
			wantErr:  `pattern "http.500" can match errors where the call may have executed`,
			wantHint: "restrict it to pre.% patterns",
		},
		{
			name:     "wildcard crossing namespaces",
			ec:       ErrorCase{Code: []string{"s%"}, Retries: 2},
			wantErr:  `pattern "s%" can match errors where the call may have executed`,
			wantHint: "not_reached:true",
		},
		{
			// The offending pattern is named, not the first one in the list.
			name:     "pre.% mixed with an unasserted wildcard",
			ec:       ErrorCase{Code: []string{"pre.%", "http.%"}, Retries: 2},
			wantErr:  `pattern "http.%" can match errors where the call may have executed`,
			wantHint: "name the exact codes",
		},

		// ── tier 2 violation: an assertion has to be about something specific ─
		{
			name:     "not_reached on a narrow wildcard",
			ec:       ErrorCase{Code: []string{"http.4%"}, NotReached: &yes, Retries: 2},
			wantErr:  `pattern "http.4%" cannot be a wildcard`,
			wantHint: `name the exact codes instead (e.g. "http.409")`,
		},
		{
			name:     "not_reached on a bare wildcard",
			ec:       ErrorCase{Code: []string{"%"}, NotReached: &yes, Retries: 2},
			wantErr:  `pattern "%" cannot be a wildcard`,
			wantHint: "asserts what one specific error means",
		},
		{
			name:     "not_reached on an only_once wildcard",
			ec:       ErrorCase{Code: []string{"only_once.%"}, NotReached: &yes, Retries: 2},
			wantErr:  `pattern "only_once.%" cannot be a wildcard`,
			wantHint: "name the exact codes",
		},
		{
			name:     "catch-all with retries",
			ec:       ErrorCase{Retries: 2},
			wantErr:  "a catch-all rule cannot have retries on an only_once task",
			wantHint: "or add not_reached:true and name the exact codes",
		},
		{
			name:     "catch-all with retries and not_reached",
			ec:       ErrorCase{NotReached: &yes, Retries: 2},
			wantErr:  "a catch-all rule cannot have retries on an only_once task",
			wantHint: "restrict it to pre.% patterns",
		},

		// ── tier 3: the unknowable codes, named exactly ──────────────────────
		// Reported for what they are rather than as a tier-1 or tier-2 problem,
		// because the advice those give ("add not_reached", "name exact codes")
		// leads nowhere here.
		{
			name:     "http.timeout named with not_reached",
			ec:       ErrorCase{Code: []string{"http.timeout"}, NotReached: &yes, Retries: 2},
			wantErr:  "http.timeout can never be retried on an only_once task, with or without not_reached",
			wantHint: "check the system of record instead",
		},
		{
			name:     "http.timeout named without not_reached",
			ec:       ErrorCase{Code: []string{"http.timeout"}, Retries: 2},
			wantErr:  "http.timeout can never be retried on an only_once task",
			wantHint: "Catch it with a goto",
		},
		{
			name:     "only_once.interrupted named with not_reached",
			ec:       ErrorCase{Code: []string{"only_once.interrupted"}, NotReached: &yes, Retries: 2},
			wantErr:  "only_once.interrupted can never be retried on an only_once task",
			wantHint: "unknowable",
		},
		{
			name:     "external.timeout named with not_reached",
			ec:       ErrorCase{Code: []string{"external.timeout"}, NotReached: &yes, Retries: 2},
			wantErr:  "external.timeout can never be retried on an only_once task",
			wantHint: "no response came back",
		},
		{
			// The unknowable one is found wherever it sits in the list.
			name:     "an unknowable code among safe ones",
			ec:       ErrorCase{Code: []string{"pre.%", "http.409", "http.timeout"}, NotReached: &yes, Retries: 2},
			wantErr:  "http.timeout can never be retried",
			wantHint: "unknowable",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := def(true, tt.ec)
			err := d.Validate()
			switch {
			case tt.wantErr == "" && err != nil:
				t.Fatalf("rejected a rule that should be expressible: %v", err)
			case tt.wantErr != "" && err == nil:
				t.Fatalf("accepted a rule that should be rejected (%+v)", tt.ec)
			case tt.wantErr != "":
				if !strings.Contains(err.Error(), tt.wantErr) {
					t.Errorf("error %q does not contain %q", err, tt.wantErr)
				}
				if tt.wantHint != "" && !strings.Contains(err.Error(), tt.wantHint) {
					t.Errorf("error %q does not tell the author what to do next (%q)", err, tt.wantHint)
				}
			}

			// Without only_once none of this applies: every rule above is legal on an
			// ordinary task, which is what keeps the rules scoped to at-most-once.
			plain := def(false, tt.ec)
			if err := plain.Validate(); err != nil {
				t.Errorf("the same rule was rejected on a task without only_once: %v", err)
			}
		})
	}
}

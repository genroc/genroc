package model

import (
	"strings"
	"testing"
)

// Each row asserts the message as well as the verdict: a rejection that does not name the
// way forward is a defect even when the verdict is right. The accepting rows matter as
// much — a false positive here means a legitimate retry policy cannot be expressed.
func TestValidateOnError_OnlyOnceRetries(t *testing.T) {
	yes := true
	def := func(onlyOnce bool, actionType ActionType, ec ErrorCase) ProcessDefinition {
		action := &Action{Type: ActionTypeFetch, Method: "post", URL: "http://x"}
		if actionType == ActionTypeExternal {
			action = &Action{Type: ActionTypeExternal}
		}
		task := &Task{
			ID:      "charge",
			Action:  action,
			Switch:  SwitchMap{{Goto: GotoEnd}},
			OnError: []ErrorCase{ec},
		}
		if onlyOnce {
			task.OnlyOnce = &yes
		}
		return ProcessDefinition{Name: "p", Tasks: []*Task{task, {
			ID: "handler", Action: &Action{Type: ActionTypeFetch, Method: "post", URL: "http://x"},
			Switch: SwitchMap{{Goto: GotoEnd}},
		}}}
	}

	tests := []struct {
		name string
		ec   ErrorCase
		// action is the kind of task the row's codes belong to; a rule naming a code the
		// task cannot report is refused for THAT before any tier is consulted.
		action ActionType
		// plain runs the same rule on a task without only_once, which must always
		// accept: none of these tiers exist for an idempotent task, and reachability
		// asks the same question with or without the flag.
		wantErr  string
		wantHint string // an additional substring the message must carry
	}{
		// ── tier 1: pre.* is safe on its own, no assertion needed ────────────
		{name: "pre.% with retries", ec: ErrorCase{Code: []string{"pre.%"}, Retry: Retries(2)}},
		{name: "exact pre codes with retries", ec: ErrorCase{Code: []string{"pre.timeout", "pre.error"}, Retry: Retries(2)}},
		{name: "pre.% with a redundant not_reached", ec: ErrorCase{Code: []string{"pre.%"}, NotReached: &yes, Retry: Retries(2)}},

		// ── tier 2: a named exception, asserted with not_reached ─────────────
		{name: "exact http code with not_reached", ec: ErrorCase{Code: []string{"http.409"}, NotReached: &yes, Retry: Retries(2)}},
		{name: "several exact codes with not_reached", ec: ErrorCase{Code: []string{"http.409", "http.422"}, NotReached: &yes, Retry: Retries(2)}},
		{
			// Per-pattern tiers: the self-evidently safe wildcard and the named
			// exception coexist, neither spoiling the other.
			name: "pre.% alongside a named exception",
			ec:   ErrorCase{Code: []string{"pre.%", "http.409"}, NotReached: &yes, Retry: Retries(2)},
		},

		// ── not a retry at all: catching is always allowed ───────────────────
		{name: "unknowable codes caught with a goto", ec: ErrorCase{Code: []string{"only_once.interrupted", "http.timeout"}, Goto: "handler"}},
		{name: "bare wildcard caught with a goto", ec: ErrorCase{Code: []string{"%"}, Goto: "handler"}},
		{name: "catch-all caught with a goto", ec: ErrorCase{Goto: "handler"}},
		{name: "wildcard with retries:0", ec: ErrorCase{Code: []string{"http.%"}}},

		// ── tier 1 violation: needs an assertion ─────────────────────────────
		{
			name:     "http.% with retries and no assertion",
			ec:       ErrorCase{Code: []string{"http.%"}, Retry: Retries(2)},
			wantErr:  `pattern "http.%" can match errors where the call may have executed`,
			wantHint: "add not_reached:true and name the exact codes",
		},
		{
			name:     "exact http code with retries and no assertion",
			ec:       ErrorCase{Code: []string{"http.500"}, Retry: Retries(2)},
			wantErr:  `pattern "http.500" can match errors where the call may have executed`,
			wantHint: "restrict it to pre.% patterns",
		},
		{
			// An unanchored prefix: reachable (it catches the result.* family), and
			// nothing about it restricts the rule to pre.*.
			name:     "wildcard crossing namespaces",
			ec:       ErrorCase{Code: []string{"re%"}, Retry: Retries(2)},
			wantErr:  `pattern "re%" can match errors where the call may have executed`,
			wantHint: "not_reached:true",
		},
		{
			// The offending pattern is named, not the first one in the list.
			name:     "pre.% mixed with an unasserted wildcard",
			ec:       ErrorCase{Code: []string{"pre.%", "http.%"}, Retry: Retries(2)},
			wantErr:  `pattern "http.%" can match errors where the call may have executed`,
			wantHint: "name the exact codes",
		},

		// ── tier 2 violation: an assertion has to be about something specific ─
		{
			name:     "not_reached on a narrow wildcard",
			ec:       ErrorCase{Code: []string{"http.4%"}, NotReached: &yes, Retry: Retries(2)},
			wantErr:  `pattern "http.4%" cannot be a wildcard`,
			wantHint: `name the exact codes instead (e.g. "http.409")`,
		},
		{
			name:     "not_reached on a bare wildcard",
			ec:       ErrorCase{Code: []string{"%"}, NotReached: &yes, Retry: Retries(2)},
			wantErr:  `pattern "%" cannot be a wildcard`,
			wantHint: "asserts what one specific error means",
		},
		{
			name:     "not_reached on an only_once wildcard",
			ec:       ErrorCase{Code: []string{"only_once.%"}, NotReached: &yes, Retry: Retries(2)},
			wantErr:  `pattern "only_once.%" cannot be a wildcard`,
			wantHint: "name the exact codes",
		},
		{
			name:     "catch-all with retries",
			ec:       ErrorCase{Retry: Retries(2)},
			wantErr:  "a catch-all rule cannot have retries on an only_once task",
			wantHint: "or add not_reached:true and name the exact codes",
		},
		{
			name:     "catch-all with retries and not_reached",
			ec:       ErrorCase{NotReached: &yes, Retry: Retries(2)},
			wantErr:  "a catch-all rule cannot have retries on an only_once task",
			wantHint: "restrict it to pre.% patterns",
		},

		// ── tier 3: the unknowable codes, named exactly ──────────────────────
		// Reported for what they are rather than as a tier-1 or tier-2 problem,
		// because the advice those give ("add not_reached", "name exact codes")
		// leads nowhere here.
		{
			name:     "http.timeout named with not_reached",
			ec:       ErrorCase{Code: []string{"http.timeout"}, NotReached: &yes, Retry: Retries(2)},
			wantErr:  "http.timeout can never be retried on an only_once task, with or without not_reached",
			wantHint: "check the system of record instead",
		},
		{
			name:     "http.timeout named without not_reached",
			ec:       ErrorCase{Code: []string{"http.timeout"}, Retry: Retries(2)},
			wantErr:  "http.timeout can never be retried on an only_once task",
			wantHint: "Catch it with a goto",
		},
		{
			name:     "only_once.interrupted named with not_reached",
			ec:       ErrorCase{Code: []string{"only_once.interrupted"}, NotReached: &yes, Retry: Retries(2)},
			wantErr:  "only_once.interrupted can never be retried on an only_once task",
			wantHint: "unknowable",
		},
		{
			name:     "external.timeout named with not_reached",
			action:   ActionTypeExternal,
			ec:       ErrorCase{Code: []string{"external.timeout"}, NotReached: &yes, Retry: Retries(2)},
			wantErr:  "external.timeout can never be retried on an only_once task",
			wantHint: "no response came back",
		},
		{
			// The unknowable one is found wherever it sits in the list.
			name:     "an unknowable code among safe ones",
			ec:       ErrorCase{Code: []string{"pre.%", "http.409", "http.timeout"}, NotReached: &yes, Retry: Retries(2)},
			wantErr:  "http.timeout can never be retried",
			wantHint: "unknowable",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := def(true, tt.action, tt.ec)
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
			plain := def(false, tt.action, tt.ec)
			if err := plain.Validate(); err != nil {
				t.Errorf("the same rule was rejected on a task without only_once: %v", err)
			}
		})
	}
}

// Reachability: the fetch/external counterpart of R5. The set is errcode's, the editor offers
// exactly it, and a pattern outside it names a failure the task cannot produce — so the rule
// reads as handled and never runs. Each rejection must name the vocabulary that IS available,
// because "wrong code" without the right list is a guessing game.
func TestValidateOnError_CodeMustBeReachable(t *testing.T) {
	yes := true
	def := func(a *Action, onlyOnce bool, codes ...string) ProcessDefinition {
		task := &Task{ID: "call", Action: a, Switch: SwitchMap{{Goto: GotoEnd}},
			OnError: []ErrorCase{{Code: codes, Goto: GotoEnd}}}
		if onlyOnce {
			task.OnlyOnce = &yes
		}
		return ProcessDefinition{Name: "p", Tasks: []*Task{task}}
	}
	fetch := &Action{Type: ActionTypeFetch, Method: "post", URL: "http://x"}
	external := &Action{Type: ActionTypeExternal}

	for _, tt := range []struct {
		name    string
		def     ProcessDefinition
		wantErr string
	}{
		{name: "a fetch's own codes", def: def(fetch, false, "http.404", "http.timeout", "pre.error", "result.parse")},
		{name: "a fetch wildcard", def: def(fetch, false, "http.4%", "%")},
		{name: "an external's own codes", def: def(external, false, "external.timeout", "external.lost")},
		{name: "only_once.interrupted where the flag is set", def: def(fetch, true, "only_once.interrupted")},
		// Reachability is flag-independent: a rule stays legal when only_once is toggled off,
		// so a handler can be written before the flag is, and debugging without it costs no edits.
		{name: "only_once.interrupted without the flag", def: def(fetch, false, "only_once.interrupted")},

		{
			name: "an external code on a fetch", def: def(fetch, false, "external.lost"),
			wantErr: `"external.lost" is not a code a "fetch" task can report`,
		},
		{
			name: "a fetch code on an external", def: def(external, false, "http.404"),
			wantErr: `"http.404" is not a code a "external" task can report`,
		},
		{
			name: "a status outside the HTTP range", def: def(fetch, false, "http.600"),
			wantErr: `"http.600" is not a code a "fetch" task can report`,
		},
		{
			name: "on_error on a task with no call",
			def: ProcessDefinition{Name: "p", Tasks: []*Task{{ID: "route", Switch: SwitchMap{{Goto: GotoEnd}},
				OnError: []ErrorCase{{Code: []string{"http.404"}, Goto: GotoEnd}}}}},
			wantErr: "there is no call to fail",
		},
		{
			name:    "on_error on a delay",
			def:     def(&Action{Type: ActionTypeDelay, DelaySpec: DelaySpec{For: "1h"}}, false, "pre.error"),
			wantErr: `on_error has no effect on a "delay" task`,
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			d := tt.def
			err := d.Validate()
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("rejected a rule the task can actually match: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatal("accepted a rule that can never fire, which reads as handled and is not")
			}
			if !containsStr(err.Error(), tt.wantErr) {
				t.Errorf("error %q does not contain %q", err, tt.wantErr)
			}
			if !containsStr(err.Error(), "can never fire") && !containsStr(err.Error(), "no effect") {
				t.Errorf("error %q does not say the rule is dead", err)
			}
		})
	}
}

// `only_once` validates the same wherever it sits, including on a task with no action. The
// flag is inert there — OnlyOnceAction needs both halves — but refusing it would make the
// declaration's legality depend on context, and an author mid-edit (action removed, about to
// be replaced) would have to delete the flag and put it back.
func TestValidateOnlyOnce_IsContextIndependent(t *testing.T) {
	yes := true
	for _, tt := range []struct {
		name   string
		action *Action
	}{
		{name: "with a call to protect", action: &Action{Type: ActionTypeFetch, Method: "post", URL: "http://x"}},
		{name: "with no action at all", action: nil},
	} {
		t.Run(tt.name, func(t *testing.T) {
			d := ProcessDefinition{Name: "p", Tasks: []*Task{
				{ID: "t", Action: tt.action, OnlyOnce: &yes, Switch: SwitchMap{{Goto: GotoEnd}}},
			}}
			if err := d.Validate(); err != nil {
				t.Fatalf("only_once must validate the same everywhere: %v", err)
			}
		})
	}
}

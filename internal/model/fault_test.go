package model

import (
	"encoding/json"
	"strings"
	"testing"
)

// Registration rules for raise/panic and the derived raise set (specs/child-error-handling.md
// §2.3, §3). R1/R2 keep a code usable as a filterable discriminator and a message unable to
// carry data across a boundary; R3/R6 stop a definition saying two things at once.

// def wraps tasks in a minimal valid definition.
func def(tasks ...*Task) ProcessDefinition {
	return ProcessDefinition{Name: "p", Tasks: tasks}
}

// raiseTask is a switch-only task whose first case raises and whose last ends.
func raiseTask(id string, f *Fault) *Task {
	return &Task{ID: id, Switch: SwitchMap{{Case: "true", Raise: f}, {Goto: GotoEnd}}}
}

func TestFault_R1_CodeShapeAndMessage(t *testing.T) {
	cases := []struct {
		name, code, message, want string
	}{
		{"uppercase rejected", "Card_Declined", "m", "not a valid error code"},
		{"hyphen rejected", "card-declined", "m", "not a valid error code"},
		{"leading digit rejected", "1_declined", "m", "not a valid error code"},
		{"empty rejected", "", "m", "not a valid error code"},
		{"missing message rejected", "card_declined", "", "message is required"},
		// '.' is reserved for engine codes: an authored error must carry its own semantic
		// name, not re-raise a system code. Its own message.
		{"dot rejected", "http.503", "m", "must not contain '.'"},
		{"dotted namespace rejected", "order.rejected", "m", "must not contain '.'"},
		// '%' is the on_error wildcard, so it can never appear in a raised/panicked code —
		// that is what removes any need to escape '%' in a pattern. Its own message.
		{"percent rejected", "card%declined", "m", "must not contain '%'"},
		{"trailing percent rejected", "declined%", "m", "must not contain '%'"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := def(raiseTask("t", &Fault{Code: tc.code, Message: tc.message}))
			err := d.Validate()
			if err == nil {
				t.Fatalf("expected rejection, got nil")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("want %q in error, got %q", tc.want, err)
			}
		})
	}

	for _, code := range []string{"insufficient_funds2", "card_declined", "poll_timeout"} {
		t.Run("accepted: "+code, func(t *testing.T) {
			// lower_snake_case, no dots.
			d := def(raiseTask("t", &Fault{Code: code, Message: "m"}))
			if err := d.Validate(); err != nil {
				t.Fatalf("unexpected rejection: %v", err)
			}
		})
	}
}

// R2 covers the CODE alone: a computed code would make the raise set uncomputable and
// error_code unqueryable. Applying it to the message too was an oversight
// (specs/child-error-handling.md R2), and the substring test that implemented it missed a
// leaf-leading `$:` while matching `$${` — the escape for a literal `${`, so the one way to
// write that text was refused along with the thing being banned.
func TestFault_R2_OnlyTheCodeMustBeLiteral(t *testing.T) {
	for _, code := range []string{"$: input.code", "${ input.code }", "$${code}", "a${b}", "$$"} {
		t.Run("code rejected: "+code, func(t *testing.T) {
			d := def(raiseTask("t", &Fault{Code: code, Message: "m"}))
			if err := d.Validate(); err == nil {
				t.Fatalf("code %q registered; a code that is not a literal is not analysable", code)
			}
		})
	}

	// The permission, pinned. A message is free text that nothing reads back, so none of
	// these is a reason to refuse a definition — including the escaped form, which a
	// substring test could never have let through.
	for _, msg := range []string{"reason: ${ input.why }", "write $${x} to escape", "$: literal"} {
		t.Run("message allowed: "+msg, func(t *testing.T) {
			d := def(raiseTask("t", &Fault{Code: "declined", Message: msg}))
			if err := d.Validate(); err != nil {
				t.Fatalf("message %q refused: %v", msg, err)
			}
		})
	}
}

// R3 differs by site, deliberately. A switch case must do exactly one thing — it is the
// routing decision. An on_error rule may do none: exhausting retries and then failing
// with the engine's own code is a long-standing, meaningful shape.
func TestFault_R3_TerminalClauseArity(t *testing.T) {
	f := &Fault{Code: "declined", Message: "m"}

	t.Run("switch case with goto and raise rejected", func(t *testing.T) {
		d := def(&Task{ID: "t", Switch: SwitchMap{{Case: "true", Goto: GotoEnd, Raise: f}, {Goto: GotoEnd}}})
		err := d.Validate()
		if err == nil || !strings.Contains(err.Error(), "exactly one") {
			t.Fatalf("want exactly-one rejection, got %v", err)
		}
	})
	t.Run("switch case with raise and panic rejected", func(t *testing.T) {
		d := def(&Task{ID: "t", Switch: SwitchMap{
			{Case: "true", Raise: f, Panic: &Fault{Code: "broken", Message: "m"}},
			{Goto: GotoEnd},
		}})
		err := d.Validate()
		if err == nil || !strings.Contains(err.Error(), "exactly one") {
			t.Fatalf("want exactly-one rejection, got %v", err)
		}
	})
	t.Run("switch case with none rejected", func(t *testing.T) {
		d := def(&Task{ID: "t", Switch: SwitchMap{{Case: "true"}, {Goto: GotoEnd}}})
		err := d.Validate()
		if err == nil || !strings.Contains(err.Error(), "exactly one") {
			t.Fatalf("want exactly-one rejection, got %v", err)
		}
	})
	t.Run("on_error rule with goto and raise rejected", func(t *testing.T) {
		d := def(&Task{
			ID: "t", Action: &Action{Type: ActionTypeFetch, URL: "http://x"},
			OnError: []ErrorCase{{Code: []string{"http.%"}, Goto: GotoEnd, Raise: f}},
			Switch:  SwitchMap{{Goto: GotoEnd}},
		})
		err := d.Validate()
		if err == nil || !strings.Contains(err.Error(), "at most one") {
			t.Fatalf("want at-most-one rejection, got %v", err)
		}
	})
	t.Run("on_error rule with none accepted", func(t *testing.T) {
		d := def(&Task{
			ID: "t", Action: &Action{Type: ActionTypeFetch, URL: "http://x"},
			OnError: []ErrorCase{{Code: []string{"http.%"}, Retry: RetryAttempts(3)}},
			Switch:  SwitchMap{{Goto: GotoEnd}},
		})
		if err := d.Validate(); err != nil {
			t.Fatalf("a retry-then-fail rule must stay legal: %v", err)
		}
	})
}

// R4: on a child task the on_error codes are LIKE patterns matched against the child's
// raised codes (same syntax as an action task's), so wildcards and dots are allowed; only
// the parent-side-retry fields (retries, not_reached) are rejected (D7). Reachability of a
// pattern against the child raise set is R5, tested in the validation package.
func TestFault_R4_ChildTaskOnError(t *testing.T) {
	childTask := func(onError []ErrorCase) *Task {
		return &Task{
			ID: "pay",
			Action: &Action{
				Type:     ActionTypeChildMap,
				Children: map[string]ChildEntry{"a": {Name: "child"}},
			},
			OnError: onError,
			Switch:  SwitchMap{{Goto: GotoEnd}},
		}
	}

	for _, code := range []string{"card_declined", "card_%", "out_of_stock"} {
		t.Run("pattern accepted: "+code, func(t *testing.T) {
			// Patterns (with the '%' wildcard) are legal on a child task; whether one can
			// actually match a raise is R5, not R4.
			d := def(childTask([]ErrorCase{{Code: []string{code}, Goto: GotoEnd}}))
			if err := d.Validate(); err != nil {
				t.Fatalf("pattern %q must be legal on a child task: %v", code, err)
			}
		})
	}
	t.Run("catch-all accepted as last rule", func(t *testing.T) {
		d := def(childTask([]ErrorCase{{Goto: GotoEnd}}))
		if err := d.Validate(); err != nil {
			t.Fatalf("catch-all must be legal on a child task: %v", err)
		}
	})
	t.Run("empty pattern string rejected", func(t *testing.T) {
		d := def(childTask([]ErrorCase{{Code: []string{""}, Goto: GotoEnd}}))
		if err := d.Validate(); err == nil || !strings.Contains(err.Error(), "code pattern must not be empty") {
			t.Fatalf("want empty-pattern rejection, got %v", err)
		}
	})
	t.Run("retry accepted — a child is a call, and a call retries", func(t *testing.T) {
		// D7 reversed 2026-08-26: retrying a raised slot re-spawns it, which re-runs the
		// upstream tasks that produced the decision. specs/child-error-handling.md R4.
		d := def(childTask([]ErrorCase{{Code: []string{"card_declined"}, Retry: RetryAttempts(3), Goto: GotoEnd}}))
		if err := d.Validate(); err != nil {
			t.Fatalf("retry must be legal on a child task: %v", err)
		}
	})
	t.Run("retry rejected on an only_once child task", func(t *testing.T) {
		// Not left to isRetryAllowed, which would decline it silently at runtime: every code
		// a child task can catch means the child already ran, so the policy could never fire.
		task := childTask([]ErrorCase{{Code: []string{"card_declined"}, Retry: RetryAttempts(3), Goto: GotoEnd}})
		once := true
		task.OnlyOnce = &once
		d := def(task)
		if err := d.Validate(); err == nil || !strings.Contains(err.Error(), "only_once child task") {
			t.Fatalf("want only_once retry rejection, got %v", err)
		}
	})
	t.Run("not_reached rejected", func(t *testing.T) {
		nr := true
		d := def(childTask([]ErrorCase{{Code: []string{"card_declined"}, NotReached: &nr, Goto: GotoEnd}}))
		if err := d.Validate(); err == nil || !strings.Contains(err.Error(), "not_reached has no meaning") {
			t.Fatalf("want not_reached rejection, got %v", err)
		}
	})
	t.Run("action task still allows patterns and retries", func(t *testing.T) {
		// The same fields are legal on an action task — R4 must not leak across.
		d := def(&Task{
			ID:      "call",
			Action:  &Action{Type: ActionTypeFetch, URL: "http://x"},
			OnError: []ErrorCase{{Code: []string{"http.5%"}, Retry: RetryAttempts(3), Goto: GotoEnd}},
			Switch:  SwitchMap{{Goto: GotoEnd}},
		})
		if err := d.Validate(); err != nil {
			t.Fatalf("action-task patterns/retries must stay legal: %v", err)
		}
	})
}

// R6: error_code must mean one thing per process. The same value appearing on 'raised'
// and 'failed' instances of the same definition would make it ambiguous for exactly the
// observers it exists to serve.
func TestFault_R6_CodeIsRaisedOrPanicked(t *testing.T) {
	d := def(
		raiseTask("poll", &Fault{Code: "timeout", Message: "gave up waiting"}),
		&Task{ID: "submit", Switch: SwitchMap{
			{Case: "true", Panic: &Fault{Code: "timeout", Message: "impossible"}},
			{Goto: GotoEnd},
		}},
	)
	err := d.Validate()
	if err == nil {
		t.Fatal("expected rejection")
	}
	for _, want := range []string{"already raised by task", "poll", "timeout"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("message should mention %q, got %q", want, err)
		}
	}
}

// §2.3: the raise set is a syntactic scan, so it is sorted, deduped, and terminates on
// a self-referencing definition. Panic codes are excluded because no on_error rule can
// ever match one — a panicking child is 'failed' and never reaches its parent's
// resolution at all.
func TestRaises_SortedDedupedAndPanicFree(t *testing.T) {
	d := def(
		&Task{ID: "a", Switch: SwitchMap{
			{Case: "1 == 1", Raise: &Fault{Code: "zebra", Message: "m"}},
			{Case: "1 == 2", Raise: &Fault{Code: "alpha", Message: "m"}},
			{Case: "1 == 3", Panic: &Fault{Code: "broken_contract", Message: "m"}},
			{Goto: GotoEnd},
		}},
		&Task{
			ID: "b", Action: &Action{Type: ActionTypeFetch, URL: "http://x"},
			// A duplicate of a's code, and one only reachable through on_error.
			OnError: []ErrorCase{
				{Code: []string{"http.500"}, Raise: &Fault{Code: "zebra", Message: "m"}},
				{Code: []string{"http.404"}, Raise: &Fault{Code: "missing", Message: "m"}},
			},
			Switch: SwitchMap{{Goto: GotoEnd}},
		},
	)
	got := d.Raises()
	want := []string{"alpha", "missing", "zebra"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v, want %v", got, want)
		}
	}
}

// The wire format is hand-written on both sides, so a round-trip is the only thing that
// catches the two halves drifting apart.
func TestSwitchMap_RaisePanicRoundTrip(t *testing.T) {
	src := `[{"case":"a","raise":{"code":"declined","message":"m1"}},` +
		`{"case":"b","panic":{"code":"broken","message":"m2"}},` +
		`{"goto":"end"}]`

	var sm SwitchMap
	if err := json.Unmarshal([]byte(src), &sm); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(sm) != 3 {
		t.Fatalf("got %d cases, want 3", len(sm))
	}
	if sm[0].Raise == nil || sm[0].Raise.Code != "declined" || sm[0].Goto != "" {
		t.Errorf("case 0: %+v", sm[0])
	}
	if sm[1].Panic == nil || sm[1].Panic.Message != "m2" {
		t.Errorf("case 1: %+v", sm[1])
	}
	if !sm[0].Terminates() || !sm[1].Terminates() || !sm[2].Terminates() {
		t.Error("all three cases terminate")
	}

	out, err := json.Marshal(sm)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var again SwitchMap
	if err := json.Unmarshal(out, &again); err != nil {
		t.Fatalf("re-unmarshal: %v", err)
	}
	if again[0].Raise == nil || again[0].Raise.Code != "declined" || again[1].Panic == nil {
		t.Errorf("round-trip lost a clause: %s", out)
	}
}

func TestErrorCase_RaisePanicRoundTrip(t *testing.T) {
	var ec ErrorCase
	if err := json.Unmarshal([]byte(`{"code":["http.402"],"raise":{"code":"declined","message":"m"}}`), &ec); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if ec.Raise == nil || ec.Raise.Code != "declined" {
		t.Fatalf("raise not decoded: %+v", ec)
	}
	out, err := json.Marshal(ec)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(out), `"raise":{"code":"declined"`) {
		t.Errorf("raise not re-encoded: %s", out)
	}
}

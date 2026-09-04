package validation

import (
	"fmt"
	"slices"

	"genroc/internal/expression"
	"genroc/internal/model"
	"genroc/internal/shape"
)

// The task scope in one place: which members of `self` each slot may name, and what
// outputs.<own id> means. The three members come into existence at different moments —
// previous when the task is entered, result when the action answers, output when the output
// map has run — so a slot may only name the ones that already exist where it is evaluated.
// specs/task-scopes.md.
//
// outputs.<own id> is previous at EVERY slot, the switch included: engine.buildEnv shadows the
// stored value there, because a task is not complete until its switch has routed.
type selfScope struct{ result, output bool }

var (
	beforeOutput = selfScope{}                           // action slots, on_error rules
	afterAction  = selfScope{result: true}               // the output map
	afterOutput  = selfScope{result: true, output: true} // the switch
)

// checkSelfScope rejects a slot naming a member that does not exist where it is evaluated.
// The schema would reject it too, with "field not found" — which names the member but not the
// reason, and reads as a typo rather than a rule.
func checkSelfScope(s *model.Task, label string, loops bool, sc selfScope, refs expression.Roots) error {
	switch {
	case refs.SelfResult && !sc.result:
		return fmt.Errorf("%s: self.result is not available here — it exists only in the task's output and switch, once the action has answered", label)
	case refs.SelfOutput && !sc.output:
		return fmt.Errorf("%s: self.output is not available here — it exists only in the switch, once the output map has been evaluated", label)
	case !refs.SelfPrevious && !slices.Contains(refs.Outputs, s.ID):
		return nil
	// The two ways a previous output fails to exist read as different mistakes, so they get
	// different sentences: one is a missing `output`, the other a task nothing returns to.
	case !s.Output.Present():
		return fmt.Errorf("%s: there is no previous output to read here — task %q declares no output", label, s.ID)
	case !loops:
		return fmt.Errorf("%s: there is no previous output to read here — no path returns to task %q, so it runs at most once", label, s.ID)
	}
	return nil
}

// slotRoots is the availability rule for one phase of a task, as the Roots hook every check of
// that phase installs: which `self` members exist there, whether a previous output exists at
// all, and — where `self.result` is in scope — whether the action types it. typedResult is read
// only in that last case. One constructor, so an expression refused at registration and the same
// expression typed by `genctl schema context -e` are refused for the same reason.
func slotRoots(s *model.Task, label string, loops, typedResult bool, sc selfScope) func(expression.Roots) error {
	return func(refs expression.Roots) error {
		if sc.result && !typedResult && refs.SelfResult {
			return fmt.Errorf("%s: references self.result, but %s", label, untypedResultAdvice(s.Action))
		}
		return checkSelfScope(s, label, loops, sc, refs)
	}
}

// preOutputSlot is one expression evaluated before this task's own output exists.
type preOutputSlot struct {
	label string
	raw   any
	expr  bool // a bare expression (a case) rather than a template
}

// preOutputSlots enumerates every slot evaluated before the task's output map runs. It is the
// list engine-side scope construction must match: a slot missing here keeps a scope the
// runtime does not populate, and reads null where the schema promised a value.
func preOutputSlots(s *model.Task) []preOutputSlot {
	var out []preOutputSlot
	add := func(label string, raw any, expr bool) {
		if raw == nil || raw == "" {
			return
		}
		out = append(out, preOutputSlot{label: label, raw: raw, expr: expr})
	}
	addShape := func(label string, sh *model.Shape) {
		if sh.Present() {
			add(label, sh.Raw, false)
		}
	}
	if a := s.Action; a != nil {
		addShape(fmt.Sprintf("task %q input", s.ID), a.Input)
		addShape(fmt.Sprintf("task %q body", s.ID), a.Body)
		add(fmt.Sprintf("task %q url", s.ID), a.URL, false)
		add(fmt.Sprintf("task %q method", s.ID), a.Method, false)
		addShape(fmt.Sprintf("task %q headers", s.ID), a.Headers)
		addShape(fmt.Sprintf("task %q query", s.ID), a.Query)
		addShape(fmt.Sprintf("task %q accepted_status", s.ID), a.AcceptedStatus)
		add(fmt.Sprintf("task %q over", s.ID), a.Over, false)
		add(fmt.Sprintf("task %q delay for", s.ID), a.For, false)
		add(fmt.Sprintf("task %q delay until", s.ID), a.Until, false)
		for key, entry := range a.Children {
			addShape(fmt.Sprintf("task %q children[%q] input", s.ID, key), entry.Input)
		}
	}
	add(fmt.Sprintf("task %q timeout for", s.ID), s.Timeout.For, false)
	add(fmt.Sprintf("task %q timeout until", s.ID), s.Timeout.Until, false)
	for i, ec := range s.OnError {
		where := fmt.Sprintf("task %q on_error[%d]", s.ID, i)
		add(where+" case", ec.Case, true)
		for _, r := range []struct{ name, expr string }{
			{"attempts", ec.Retry.Attempts.Expr()},
			{"delay", ec.Retry.Delay.Expr()},
			{"factor", ec.Retry.Factor.Expr()},
			{"max_delay", ec.Retry.MaxDelay.Expr()},
		} {
			add(fmt.Sprintf("%s retry.%s", where, r.name), r.expr, false)
		}
		out = append(out, faultSlots(where, ec.Raise, ec.Panic)...)
	}
	return out
}

func faultSlots(where string, faults ...*model.Fault) []preOutputSlot {
	var out []preOutputSlot
	for _, f := range faults {
		if f == nil {
			continue
		}
		if f.Message != "" {
			out = append(out, preOutputSlot{label: where + " message", raw: f.Message})
		}
		if f.Data.Present() {
			out = append(out, preOutputSlot{label: where + " data", raw: f.Data.Raw})
		}
	}
	return out
}

// checkPreOutputScopes runs the scope guard over every pre-output slot, BEFORE the per-slot
// type checks, so the reason beats the schema's "field not found".
func checkPreOutputScopes(s *model.Task, loops bool) error {
	for _, slot := range preOutputSlots(s) {
		sh := shape.Shape{Raw: slot.raw, Expr: slot.expr}
		refs, err := sh.Roots()
		if err != nil {
			continue // a parse failure is the per-slot check's to report, with its own message
		}
		// beforeOutput has no self.result, so typedResult cannot be reached from here.
		if err := slotRoots(s, slot.label, loops, false, beforeOutput)(refs); err != nil {
			return err
		}
	}
	return nil
}

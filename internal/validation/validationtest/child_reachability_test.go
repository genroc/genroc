package validationtest

import (
	"testing"

	"genroc/internal/model"
	"genroc/internal/validation"
)

// R5 (rule reachability): every code an on_error rule names on a child task must be
// raisable by some child of that task. The check runs one direction only — a typo'd or
// orphaned rule is caught; a raisable code with no rule is allowed (D3, §3.1).

// raisingChild builds a child definition that can raise the given codes from a switch.
func raisingChild(name string, codes ...string) *model.ProcessDefinition {
	cases := make(model.SwitchMap, 0, len(codes)+1)
	for _, c := range codes {
		cases = append(cases, model.SwitchCase{
			Case:  "false",
			Raise: &model.Fault{Code: c, Message: "m"},
		})
	}
	cases = append(cases, model.SwitchCase{Goto: model.GotoEnd})
	return &model.ProcessDefinition{
		Name:  name,
		Tasks: []*model.Task{{ID: "decide", Switch: cases}},
	}
}

// childMapParent builds a parent with a single child_map task over the named children,
// carrying the given on_error rules.
func childMapParent(childName string, onError []model.ErrorCase) *model.ProcessDefinition {
	def := &model.ProcessDefinition{
		Name: "parent",
		Tasks: []*model.Task{
			{
				ID: "pay",
				Action: &model.Action{
					Type:     model.ActionTypeChildMap,
					Children: map[string]model.ChildEntry{"a": {Name: childName}},
				},
				OnError: onError,
				Switch:  model.SwitchMap{{Goto: model.GotoEnd}},
			},
		},
	}
	return def
}

// childActionParent builds a parent with a standalone `child` action over the named
// child, carrying the given on_error rules — the single-child analogue of childMapParent.
func childActionParent(childName string, onError []model.ErrorCase) *model.ProcessDefinition {
	return &model.ProcessDefinition{
		Name: "parent",
		Tasks: []*model.Task{
			{
				ID:      "pay",
				Action:  &model.Action{Type: model.ActionTypeChild, Name: childName},
				OnError: onError,
				Switch:  model.SwitchMap{{Goto: model.GotoEnd}},
			},
		},
	}
}

func TestR5_ChildAction_RuleForRaisableCode_OK(t *testing.T) {
	getter := stubGetter{"charge-card": raisingChild("charge-card", "card_declined")}
	def := childActionParent("charge-card", []model.ErrorCase{
		{Code: []string{"card_declined"}, Goto: model.GotoEnd},
	})
	if err := def.Normalize(); err != nil {
		t.Fatalf("normalize: %v", err)
	}
	assertValidateOK(t, def, getter)
}

func TestR5_ChildAction_RuleForUnraisableCode_Rejected(t *testing.T) {
	getter := stubGetter{"charge-card": raisingChild("charge-card", "card_declined")}
	def := childActionParent("charge-card", []model.ErrorCase{
		{Code: []string{"card_expired"}, Goto: model.GotoEnd},
	})
	if err := def.Normalize(); err != nil {
		t.Fatalf("normalize: %v", err)
	}
	assertValidateErr(t, def, getter, `no child of this task can raise a code matching "card_expired"`)
}

func TestR5_RuleForRaisableCode_OK(t *testing.T) {
	getter := stubGetter{"charge-card": raisingChild("charge-card", "card_declined", "insufficient_funds")}
	def := childMapParent("charge-card", []model.ErrorCase{
		{Code: []string{"card_declined"}, Goto: model.GotoEnd},
		{Code: []string{"insufficient_funds"}, Raise: &model.Fault{Code: "payment_failed", Message: "m"}},
	})
	if err := def.Normalize(); err != nil {
		t.Fatalf("normalize: %v", err)
	}
	assertValidateOK(t, def, getter)
}

func TestR5_RuleForUnraisableCode_Rejected(t *testing.T) {
	getter := stubGetter{"charge-card": raisingChild("charge-card", "card_declined")}
	// card_expired is a typo / not raised by the child.
	def := childMapParent("charge-card", []model.ErrorCase{
		{Code: []string{"card_expired"}, Goto: model.GotoEnd},
	})
	if err := def.Normalize(); err != nil {
		t.Fatalf("normalize: %v", err)
	}
	assertValidateErr(t, def, getter, `no child of this task can raise a code matching "card_expired"`)
}

// A LIKE pattern is accepted as long as it matches at least one raised code — it need not
// be a literal member of the raise set.
func TestR5_LikePatternMatchingRaiseAccepted(t *testing.T) {
	getter := stubGetter{"charge-card": raisingChild("charge-card", "card_declined", "card_expired")}
	def := childMapParent("charge-card", []model.ErrorCase{
		{Code: []string{"card_%"}, Goto: model.GotoEnd},
	})
	if err := def.Normalize(); err != nil {
		t.Fatalf("normalize: %v", err)
	}
	assertValidateOK(t, def, getter)
}

// A pattern that matches no raised code is still rejected — the reachability guarantee
// holds for patterns, not just literals.
func TestR5_LikePatternMatchingNothingRejected(t *testing.T) {
	getter := stubGetter{"charge-card": raisingChild("charge-card", "card_declined")}
	def := childMapParent("charge-card", []model.ErrorCase{
		{Code: []string{"shipping_%"}, Goto: model.GotoEnd},
	})
	if err := def.Normalize(); err != nil {
		t.Fatalf("normalize: %v", err)
	}
	assertValidateErr(t, def, getter, `no child of this task can raise a code matching "shipping_%"`)
}

// A child_map takes the union of raises across all entries (E11).
func TestR5_UnionAcrossChildMapEntries(t *testing.T) {
	getter := stubGetter{
		"charge-card": raisingChild("charge-card", "card_declined"),
		"ship-order":  raisingChild("ship-order", "out_of_stock"),
	}
	def := &model.ProcessDefinition{
		Name: "parent",
		Tasks: []*model.Task{
			{
				ID: "pay",
				Action: &model.Action{
					Type: model.ActionTypeChildMap,
					Children: map[string]model.ChildEntry{
						"pay":  {Name: "charge-card"},
						"ship": {Name: "ship-order"},
					},
				},
				// Each code is raised by a different entry — both must be reachable.
				OnError: []model.ErrorCase{
					{Code: []string{"card_declined", "out_of_stock"}, Goto: model.GotoEnd},
				},
				Switch: model.SwitchMap{{Goto: model.GotoEnd}},
			},
		},
	}
	if err := def.Normalize(); err != nil {
		t.Fatalf("normalize: %v", err)
	}
	assertValidateOK(t, def, getter)
}

// A self-referencing child: R5 checks the definition's own rules against its own raise
// set, and terminates because Raises() is a syntactic scan (E10).
func TestR5_SelfReferenceTerminates(t *testing.T) {
	def := &model.ProcessDefinition{
		Name: "recur",
		Tasks: []*model.Task{
			{
				ID: "step",
				Switch: model.SwitchMap{
					{Case: "false", Raise: &model.Fault{Code: "gave_up", Message: "m"}},
					{Goto: model.GotoEnd},
				},
			},
			{
				ID: "recurse",
				Action: &model.Action{
					Type:     model.ActionTypeChildMap,
					Children: map[string]model.ChildEntry{"self": {Name: "recur"}},
				},
				OnError: []model.ErrorCase{{Code: []string{"gave_up"}, Goto: model.GotoEnd}},
				Switch:  model.SwitchMap{{Goto: model.GotoEnd}},
			},
		},
	}
	if err := def.Normalize(); err != nil {
		t.Fatalf("normalize: %v", err)
	}
	assertValidateOK(t, def, stubGetter{"recur": def})
}

// A panic code is not in the raise set, so a rule naming it is unreachable (D6): no
// on_error rule can ever match a panic, because the panicking child is 'failed'.
func TestR5_PanicCodeIsNotRaisable(t *testing.T) {
	child := &model.ProcessDefinition{
		Name: "submit",
		Tasks: []*model.Task{{ID: "check", Switch: model.SwitchMap{
			{Case: "false", Panic: &model.Fault{Code: "broken_contract", Message: "m"}},
			{Goto: model.GotoEnd},
		}}},
	}
	getter := stubGetter{"submit": child}
	def := childMapParent("submit", []model.ErrorCase{
		{Code: []string{"broken_contract"}, Goto: model.GotoEnd},
	})
	if err := def.Normalize(); err != nil {
		t.Fatalf("normalize: %v", err)
	}
	assertValidateErr(t, def, getter, `no child of this task can raise a code matching "broken_contract"`)
}

// Sanity: ValidateChildProcessRefs is the entry point R5 rides on, and it composes with
// the existing input-compatibility checks without interfering.
func TestR5_CoexistsWithInputCheck(t *testing.T) {
	getter := stubGetter{"charge-card": raisingChild("charge-card", "card_declined")}
	def := childMapParent("charge-card", []model.ErrorCase{
		{Code: []string{"card_declined"}, Goto: model.GotoEnd},
	})
	if err := def.Normalize(); err != nil {
		t.Fatalf("normalize: %v", err)
	}
	if err := validation.ValidateChildProcessRefs(def, 1, getter); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

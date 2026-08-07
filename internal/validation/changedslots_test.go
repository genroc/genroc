package validation

import (
	"reflect"
	"testing"

	"genroc/internal/model"
)

// The changed-slot lists are hand-maintained, and forgetting an entry fails SILENTLY:
// the slot simply stops being reported, and every comparison still comes back
// well-formed. These tests are the only thing that makes that loud, so they enumerate
// the structs rather than checking a hand-written list against itself.

// notASlot names the fields deliberately absent from a slot list, each with the reason
// it is absent. A field added to one of these structs matches nothing here and fails.
var notASlot = map[string]string{
	"Task.ID":                "the identity tasks are matched by, not a slot that can differ",
	"Task.Action":            "covered field-by-field by actionSlots",
	"ProcessDefinition.Name": "the identity processes are paired by",
	"ProcessDefinition.Tasks": "compared per task, not as one blob — the per-task verdicts " +
		"are the report, and a whole-list diff would say nothing about where an instance sits",
	"ProcessDefinition.ConfigSchema": "not part of either check. It is a runtime check that " +
		"the environment is set — no contract with a party, and config is stripped from every " +
		"context — and where it DOES reach the data, through an expression reading config.x, " +
		"validation catches it better by type-checking that expression against the new schema. " +
		"Cost: a `secret: true` dropped from it is reported nowhere (compat-command.md §6b)",
	"ProcessDefinition.Defs": "a shared pool, not a place in the process. Normalize BAKES a " +
		"referenced definition into every schema that uses it, so an edit already reports at " +
		"each schema addressing it, under a path an operator can navigate. Reporting it here " +
		"too would be the same edit twice, at a name no instance and no caller can be pointed " +
		"at; a definition nobody references is silent, which is right (compat-command.md §6a)",
}

func assertEveryFieldIsASlot[T any](t *testing.T, typeName string, covered map[string]bool) {
	t.Helper()
	var zero T
	rt := reflect.TypeOf(zero)
	if rt.Kind() == reflect.Ptr {
		rt = rt.Elem()
	}
	var walk func(reflect.Type)
	walk = func(rt reflect.Type) {
		for i := range rt.NumField() {
			f := rt.Field(i)
			if f.Anonymous {
				// An embedded struct is flat on the wire (Action embeds DelaySpec), so
				// its fields are slots of the outer type.
				walk(f.Type)
				continue
			}
			if covered[f.Name] {
				continue
			}
			if _, exempt := notASlot[typeName+"."+f.Name]; exempt {
				continue
			}
			t.Errorf("%s.%s is reported by no slot: add it to the slot list in changedslots.go, "+
				"or to notASlot with the reason it cannot differ", typeName, f.Name)
		}
	}
	walk(rt)
}

func coveredFields[T any](slots []slot[T]) map[string]bool {
	out := make(map[string]bool, len(slots))
	for _, s := range slots {
		out[s.field] = true
	}
	return out
}

func TestChangedSlots_EveryTaskFieldIsReported(t *testing.T) {
	assertEveryFieldIsASlot[*model.Task](t, "Task", coveredFields(taskSlots))
}

func TestChangedSlots_EveryActionFieldIsReported(t *testing.T) {
	assertEveryFieldIsASlot[*model.Action](t, "Action", coveredFields(actionSlots))
}

func TestChangedSlots_EveryDefinitionFieldIsReported(t *testing.T) {
	assertEveryFieldIsASlot[*model.ProcessDefinition](t, "ProcessDefinition", coveredFields(definitionSlots))
}

// Every slot must actually read the field it claims to, or the coverage test above
// passes while the slot reports something else.
func TestChangedSlots_EachSlotReadsItsOwnField(t *testing.T) {
	for _, s := range actionSlots {
		if _, ok := reflect.TypeOf(model.Action{}).FieldByName(s.field); !ok {
			t.Errorf("action slot %q names field %q, which model.Action does not have", s.name, s.field)
		}
	}
	for _, s := range taskSlots {
		if _, ok := reflect.TypeOf(model.Task{}).FieldByName(s.field); !ok {
			t.Errorf("task slot %q names field %q, which model.Task does not have", s.name, s.field)
		}
	}
	for _, s := range definitionSlots {
		if _, ok := reflect.TypeOf(model.ProcessDefinition{}).FieldByName(s.field); !ok {
			t.Errorf("definition slot %q names field %q, which model.ProcessDefinition does not have", s.name, s.field)
		}
	}
}

// A task compared against itself must report nothing: the slot comparison is a byte
// comparison over JSON encodings, so a slot whose type marshals unstably (a map iterated
// without sorting, say) would report a spurious difference on every single task.
func TestChangedSlots_NothingDiffersAgainstItself(t *testing.T) {
	task := &model.Task{
		ID: "charge",
		Action: &model.Action{
			Type: model.ActionTypeChildMap,
			Children: map[string]model.ChildEntry{
				"a": {Name: "one"}, "b": {Name: "two"}, "c": {Name: "three"},
			},
		},
		Timeout:  model.TimeoutFor("5s"),
		OnlyOnce: func() *bool { b := true; return &b }(),
		Switch:   model.SwitchMap{{Goto: model.GotoEnd}},
	}
	if changed := changedTaskSlots(task, task); len(changed) != 0 {
		t.Fatalf("a task differs from itself at %v", changed)
	}
}

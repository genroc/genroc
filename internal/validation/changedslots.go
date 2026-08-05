package validation

import (
	"bytes"
	"encoding/json"

	"genroc/internal/model"
)

// Changed slots are a FIELD comparison, not a schema question, and that is why they live
// in their own file: "which slots differ" must never acquire opinions about which
// differences matter. The verdicts elsewhere are blind to meaning — dollars → cents is
// `number` on both sides — so this list is what lets a reader apply the judgement the
// machine cannot.
//
// Adding a field to model.Action or model.Task means adding it here too, and nothing
// fails if you forget: the slot silently stops being reported. TestChangedSlots_*
// enumerates both structs to make that loud.
//
// Two slots differ when their JSON encodings differ. Stored definitions are canonical
// (SaveDefinition writes json.Marshal of the decoded struct, so `retry: 3` is already the
// object form), which is what makes a byte comparison a fair one.

// slot is one named, comparable field of a task or an action. field is the Go field it
// reads, carried so the coverage test can enumerate the struct against this list.
type slot[T any] struct {
	name  string
	field string
	get   func(T) any
}

// taskSlots are the slots read off model.Task itself. Task.ID is the identity tasks are
// matched by, not a slot; Task.Action is covered by actionSlots.
var taskSlots = []slot[*model.Task]{
	{"output", "Output", func(t *model.Task) any { return t.Output }},
	{"switch", "Switch", func(t *model.Task) any { return t.Switch }},
	{"on_error", "OnError", func(t *model.Task) any { return t.OnError }},
	{"timeout", "Timeout", func(t *model.Task) any { return t.Timeout }},
	{"only_once", "OnlyOnce", func(t *model.Task) any { return t.OnlyOnce }},
}

// actionSlots are the slots read off model.Action. A task with no action reads every one
// of them as null, so gaining or losing an action reports as action.type changing —
// which is the slot an operator would look for.
var actionSlots = []slot[*model.Action]{
	{"action.type", "Type", func(a *model.Action) any { return a.Type }},
	{"action.url", "URL", func(a *model.Action) any { return a.URL }},
	{"action.method", "Method", func(a *model.Action) any { return a.Method }},
	{"action.headers", "Headers", func(a *model.Action) any { return a.Headers }},
	{"action.accepted_status", "AcceptedStatus", func(a *model.Action) any { return a.AcceptedStatus }},
	{"action.body", "Body", func(a *model.Action) any { return a.Body }},
	{"action.input", "Input", func(a *model.Action) any { return a.Input }},
	{"action.result_schema", "ResultSchema", func(a *model.Action) any { return a.ResultSchema }},
	{"action.name", "Name", func(a *model.Action) any { return a.Name }},
	{"action.version", "Version", func(a *model.Action) any { return a.Version }},
	{"action.children", "Children", func(a *model.Action) any { return a.Children }},
	{"action.over", "Over", func(a *model.Action) any { return a.Over }},
	{"action.for", "For", func(a *model.Action) any { return a.For }},
	{"action.until", "Until", func(a *model.Action) any { return a.Until }},
	{"action.tz", "TZ", func(a *model.Action) any { return a.TZ }},
}

// definitionSlots are the process-level slots. They are not symmetry with the per-task
// list: input_schema and config_schema belong to no task and would otherwise have no
// entry anywhere, and a schema changing nothing but `secret: true` compares equal to
// every schema verdict, so this is the only place such a change is reported.
var definitionSlots = []slot[*model.ProcessDefinition]{
	{"input_schema", "InputSchema", func(d *model.ProcessDefinition) any { return d.InputSchema }},
	{"config_schema", "ConfigSchema", func(d *model.ProcessDefinition) any { return d.ConfigSchema }},
	{"output", "Output", func(d *model.ProcessDefinition) any { return d.Output }},
	{"$defs", "Defs", func(d *model.ProcessDefinition) any { return d.Defs }},
}

func changedTaskSlots(old, new *model.Task) []string {
	var changed []string
	for _, s := range taskSlots {
		if !sameJSON(s.get(old), s.get(new)) {
			changed = append(changed, s.name)
		}
	}
	for _, s := range actionSlots {
		if !sameJSON(actionSlotValue(old.Action, s), actionSlotValue(new.Action, s)) {
			changed = append(changed, s.name)
		}
	}
	return changed
}

func changedDefinitionSlots(old, new *model.ProcessDefinition) []string {
	var changed []string
	for _, s := range definitionSlots {
		if !sameJSON(s.get(old), s.get(new)) {
			changed = append(changed, s.name)
		}
	}
	return changed
}

func actionSlotValue(a *model.Action, s slot[*model.Action]) any {
	if a == nil {
		return nil
	}
	return s.get(a)
}

// sameJSON compares two slot values by their encodings. A value that will not marshal is
// reported changed rather than silently equal: the report exists to surface differences,
// so an unreadable slot must not read as agreement.
func sameJSON(a, b any) bool {
	x, errA := json.Marshal(a)
	y, errB := json.Marshal(b)
	if errA != nil || errB != nil {
		return false
	}
	return bytes.Equal(x, y)
}

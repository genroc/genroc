package validation

import (
	"bytes"
	"encoding/json"
	"slices"
	"strings"

	"genroc/internal/model"
)

// Changed slots are a FIELD comparison, never a schema judgement (see CLAUDE.md). A new
// Action/Task field must be added here, and nothing fails if you forget —
// TestChangedSlots_* enumerates both structs to make the omission loud.

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

// definitionSlots are the process-level slots. They belong to no task and would otherwise
// have no entry anywhere, and a schema changing nothing but `secret: true` compares equal to
// every schema verdict, so this is the only place such a change is reported.
//
// `config_schema` and `$defs` are deliberately absent — see notASlot in the test.
var definitionSlots = []slot[*model.ProcessDefinition]{
	{"input_schema", "InputSchema", func(d *model.ProcessDefinition) any { return d.InputSchema }},
	{"output", "Output", func(d *model.ProcessDefinition) any { return d.Output }},
}

// Addresses that name a place rather than a task. specs/compat-command.md §6a.
const (
	addressInput  = "input"
	addressOutput = "output"
)

// slotAddress is where the report files a change to a slot: the place the slot defines, so
// an edit lands where its consequences are read. An action's slots are addressed by the
// ACTION TYPE rather than the word `action` — they are polymorphic, `url` existing only on a
// fetch and `name` only on a child, so the type says which vocabulary the name comes from —
// and `result_schema` drops its suffix, naming what it describes (§6a).
func slotAddress(task, slot, actionType string) string {
	name := slot
	// `action.type` is the one action slot that is NOT polymorphic: every action has a type,
	// and it is the discriminator the others are named by. Addressing it under a type would
	// be circular — `go:fetch.type` — so it keeps the generic name.
	if rest, ok := strings.CutPrefix(slot, "action."); ok && rest != "type" {
		if rest == "result_schema" {
			rest = "result"
		}
		name = actionType + "." + rest
	}
	if task == "" {
		if name == "input_schema" {
			return addressInput
		}
		return name
	}
	return task + ":" + name
}

// slotAffects is the question a slot BEARS ON — a property of the slot itself, not of any
// comparison — so a change that broke nothing still says which column it could have moved.
// The rule is §3a's: who submits the value, and what conform stands between the parties.
//
// **Empty is a real answer, not an omission.** A URL repointed, an `only_once` flipped, a
// `switch` rerouted: no verdict covers them, and saying so is what `(not judged)` renders. A
// slot added to the tables above and forgotten here reads that way too, which is the safe
// direction — it appears, unjudged, rather than vanishing.
func slotAffects(slot string) []Member {
	switch slot {
	// The stored input is read by every task; the same schema is what ValidateInput holds
	// the next caller to. One slot, both questions — the case §3b is entirely about.
	case "input_schema":
		return []Member{MemberUpgrade, MemberContract}
	// A task's output projects into `outputs.<id>`, which every later task reads.
	case "output":
		return []Member{MemberUpgrade}
	// What a response must satisfy: narrowing means OUR conform starts rejecting responses
	// the service still sends. Whether it is ALSO an upgrade concern depends on the action
	// type, not the slot — a parked task carries the result across a version change (§2c) —
	// so that half is decided by the caller.
	case "action.result_schema":
		return []Member{MemberContract}
	}
	return nil
}

func changedTaskSlots(old, new *model.Task) []SlotChange {
	// The OLD side's action type names the address: a task whose type changed is addressed
	// by what it was, which is the side an instance is parked under (§6a).
	actionType := actionTypeOf(old)
	var changed []SlotChange
	emit := func(name string) {
		changed = append(changed, SlotChange{
			Address: slotAddress(old.ID, name, actionType),
			Task:    old.ID,
			Affects: taskSlotAffects(name, actionType),
		})
	}
	for _, s := range taskSlots {
		if !sameJSON(s.get(old), s.get(new)) {
			emit(s.name)
		}
	}
	// A task that became a different KIND of action reports that and nothing else. Its other
	// action slots are not comparable across types — a `url` that vanished did so because the
	// task stopped being a fetch — and listing them would file consequences beside the cause
	// as if they were independent edits, under an address (`go:external.url`) naming a slot
	// that type does not have.
	if typeChanged(old, new) {
		emit("action.type")
		return changed
	}
	for _, s := range actionSlots {
		if !sameJSON(actionSlotValue(old.Action, s), actionSlotValue(new.Action, s)) {
			emit(s.name)
		}
	}
	return changed
}

func typeChanged(old, new *model.Task) bool {
	return actionTypeOf(old) != actionTypeOf(new)
}

// taskSlotAffects is slotAffects plus the one rule that depends on the action rather than
// the slot: a result schema is ALSO an upgrade concern where the task can park mid-flight,
// because the instance is holding state the entry context does not describe (§2c). A fetch
// cannot park — request and response happen inside one advance — which is why this is a rule
// about parking rather than a list of types.
func taskSlotAffects(slot, actionType string) []Member {
	affects := slotAffects(slot)
	if slot == "action.result_schema" && parksMidTask(actionType) {
		affects = append([]Member{MemberUpgrade}, affects...)
	}
	return affects
}

func parksMidTask(actionType string) bool {
	switch model.ActionType(actionType) {
	case model.ActionTypeExternal, model.ActionTypeChild, model.ActionTypeChildMap, model.ActionTypeChildList:
		return true
	}
	return false
}

func actionTypeOf(t *model.Task) string {
	if t.Action == nil {
		return ""
	}
	return string(t.Action.Type)
}

// documentsDiffer: the content half of "nothing changed" (versions can carry identical
// content; submitted documents carry no number). Task ORDER counts — `switch: next`
// routes by position, so a move changes control flow while every slot compares equal.
func documentsDiffer(old, new *model.ProcessDefinition) bool {
	if len(changedDefinitionSlots(old, new)) > 0 {
		return true
	}
	if !slices.Equal(taskIDs(old), taskIDs(new)) {
		return true
	}
	newTasks := tasksByID(new)
	for _, t := range old.Tasks {
		if len(changedTaskSlots(t, newTasks[t.ID])) > 0 {
			return true
		}
	}
	return false
}

func taskIDs(def *model.ProcessDefinition) []string {
	out := make([]string, len(def.Tasks))
	for i, t := range def.Tasks {
		out[i] = t.ID
	}
	return out
}

func changedDefinitionSlots(old, new *model.ProcessDefinition) []SlotChange {
	var changed []SlotChange
	for _, s := range definitionSlots {
		if !sameJSON(s.get(old), s.get(new)) {
			changed = append(changed, SlotChange{
				Address: slotAddress("", s.name, ""),
				Affects: slotAffects(s.name),
			})
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

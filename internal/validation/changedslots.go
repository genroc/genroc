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
	{"action.over", "Over", func(a *model.Action) any { return a.Over }},
	{"action.for", "For", func(a *model.Action) any { return a.For }},
	{"action.until", "Until", func(a *model.Action) any { return a.Until }},
	{"action.tz", "TZ", func(a *model.Action) any { return a.TZ }},
}

// childEntrySlots decompose a `child_map`'s children. A key is a CALL, so its slots are
// addressed the way an action's are — the same address a per-key break carries, which is what
// lets §6b suppress the slot row where one broke.
var childEntrySlots = []slot[model.ChildEntry]{
	{"name", "Name", func(c model.ChildEntry) any { return c.Name }},
	{"version", "Version", func(c model.ChildEntry) any { return c.Version }},
	{"input", "Input", func(c model.ChildEntry) any { return c.Input }},
	{"result_schema", "ResultSchema", func(c model.ChildEntry) any { return c.ResultSchema }},
}

// definitionSlots are the process-level slots. `config_schema` and `$defs` are here to be
// REPORTED, never judged: no verdict covers them, so without a row an edit to either comes
// back as two clean verdicts with nothing under them. A `$defs` break ALSO lands wherever
// Normalize baked the definition in — a different fact, and only that one can be missing.
var definitionSlots = []slot[*model.ProcessDefinition]{
	{"input_schema", "InputSchema", func(d *model.ProcessDefinition) any { return d.InputSchema }},
	{"config_schema", "ConfigSchema", func(d *model.ProcessDefinition) any { return d.ConfigSchema }},
	{"$defs", "Defs", func(d *model.ProcessDefinition) any { return d.Defs }},
	{"output", "Output", func(d *model.ProcessDefinition) any { return d.Output }},
}

// Addresses that name a place rather than a task. specs/compat-command.md §6a.
const (
	addressInput  = "input"
	addressOutput = "output"
	// addressTasks is the task LIST, which is a slot of its own only for its order — the
	// tasks themselves are compared one at a time, under their own addresses.
	addressTasks = "tasks"
)

// slotAddress is where the report files a change to a slot: the place the slot defines, so
// an edit lands where its consequences are read. An action's slots are polymorphic, so they
// are addressed by the ACTION TYPE rather than the word `action` (§6a).
func slotAddress(task, slot, actionType string) string {
	name := slot
	// `action.type` is the discriminator the others are named by, so addressing it under a
	// type would be circular — `go:fetch.type`. It keeps the generic name.
	if rest, ok := strings.CutPrefix(slot, "action."); ok && rest != "type" {
		name = actionType + "." + slotLeafName(rest)
	}
	if task == "" {
		if name == "input_schema" {
			return addressInput
		}
		return name
	}
	return task + ":" + name
}

// slotLeafName is the last segment of an address. `result_schema` drops its suffix, naming
// what it describes rather than what it is (§6a); everything else is written as declared.
func slotLeafName(slot string) string {
	if slot == "result_schema" {
		return "result"
	}
	return slot
}

// childKeyAddress is where a child_map's key is reported: the key names a call, so it sits
// under the action type the same way an action's own slots do. compat.go builds break
// addresses from it too — the two must agree, or §6b's suppression sees two places.
func childKeyAddress(task, actionType, key string) string {
	return task + ":" + actionType + "." + key
}

// slotAffects is the question a slot BEARS ON — a property of the slot itself, not of any
// comparison. Empty is a real answer, not an omission: it is what `(not judged)` renders, and
// a slot added to the tables above and forgotten here reads that way too, which is the safe
// direction. The rule is §3a's: who submits the value, and what conform stands between.
func slotAffects(slot string) []Member {
	switch slot {
	// One slot, both questions — the case §3b is entirely about.
	case "input_schema":
		return []Member{MemberUpgrade, MemberContract}
	case "output":
		return []Member{MemberUpgrade}
	// Whether it is ALSO an upgrade concern depends on the action type rather than the slot,
	// so that half is decided by the caller — see taskSlotAffects.
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
	// A task that became a different KIND of action reports that and nothing else: its other
	// slots are not comparable across types, and listing them would file consequences beside
	// the cause under an address (`go:external.url`) naming a slot that type does not have.
	if typeChanged(old, new) {
		emit("action.type")
		return changed
	}
	for _, s := range actionSlots {
		if !sameJSON(actionSlotValue(old.Action, s), actionSlotValue(new.Action, s)) {
			emit(s.name)
		}
	}
	return append(changed, changedChildKeySlots(old, new)...)
}

// changedChildKeySlots reports a child_map's keys one call at a time: the slots of the keys
// both sides carry, and the existence of one only the old side does.
func changedChildKeySlots(old, new *model.Task) []SlotChange {
	if old.Action == nil || new.Action == nil || old.Action.Type != model.ActionTypeChildMap {
		return nil
	}
	actionType := string(model.ActionTypeChildMap)
	var changed []SlotChange
	for _, key := range sortedChildKeys(old.Action.Children) {
		newChild, ok := new.Action.Children[key]
		if !ok {
			// The call went. Nothing FAILS — the orphan output lands under an undeclared key and
			// the output conform strips it — but a call that stopped being made is an edit no
			// verdict covers, which is what this channel exists to report.
			changed = append(changed, SlotChange{
				Address: childKeyAddress(old.ID, actionType, key),
				Task:    old.ID,
			})
			continue
		}
		oldChild := old.Action.Children[key]
		for _, s := range childEntrySlots {
			if sameJSON(s.get(oldChild), s.get(newChild)) {
				continue
			}
			changed = append(changed, SlotChange{
				Address: childKeyAddress(old.ID, actionType, key) + "." + slotLeafName(s.name),
				Task:    old.ID,
				Affects: childEntrySlotAffects(s.name),
			})
		}
	}
	return changed
}

// childEntrySlotAffects is slotAffects for one key of a child_map, which always parks — so
// its `result_schema` bears on both questions. The rest are judged by nothing: §2c has why a
// key's `name` needs no rule, and a pinned `version` is the child's own row to report.
func childEntrySlotAffects(slot string) []Member {
	if slot == "result_schema" {
		return []Member{MemberUpgrade, MemberContract}
	}
	return nil
}

func typeChanged(old, new *model.Task) bool {
	return actionTypeOf(old) != actionTypeOf(new)
}

// taskSlotAffects is slotAffects plus the one rule that depends on the action rather than the
// slot: a result schema is ALSO an upgrade concern where the task can park mid-flight, since
// the instance holds state the entry context does not describe (§2c).
func taskSlotAffects(slot, actionType string) []Member {
	affects := slotAffects(slot)
	if slot == "action.result_schema" && parksMidTask(actionType) {
		affects = append([]Member{MemberUpgrade}, affects...)
	}
	return affects
}

// parksMidTask reports whether this action leaves a VALUE the entry context does not
// describe. Derived from model.ActionType.Holds rather than restated — the engine and this
// comparison need the same answer, and two copies of it drift silently.
func parksMidTask(actionType string) bool {
	return model.ActionType(actionType).Holds().Result
}

// holdsAnInstance is the wider question: can an instance be SITTING in this action at all?
// True for a delay too, which holds a live instance and no data — its timer was computed
// under the old definition.
func holdsAnInstance(actionType string) bool {
	return model.ActionType(actionType).Holds().Anything()
}

func actionTypeOf(t *model.Task) string {
	if t.Action == nil {
		return ""
	}
	return string(t.Action.Type)
}

// documentsDiffer: the content half of "unchanged" (versions can carry identical
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
	if reordered(old, new) {
		changed = append(changed, SlotChange{Address: addressTasks})
	}
	return changed
}

// reordered reports whether the tasks BOTH versions carry appear in a different order —
// `switch: next` routes by position, so a move reroutes while every slot compares equal.
// Only the tasks in common: an insertion shifts everything after it, and would otherwise
// report a reorder beside the `(added)` row it already caused.
func reordered(old, new *model.ProcessDefinition) bool {
	inOld, inNew := tasksByID(old), tasksByID(new)
	shared := func(def *model.ProcessDefinition, other map[string]*model.Task) []string {
		var out []string
		for _, t := range def.Tasks {
			if _, both := other[t.ID]; both {
				out = append(out, t.ID)
			}
		}
		return out
	}
	return !slices.Equal(shared(old, inNew), shared(new, inOld))
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

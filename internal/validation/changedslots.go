package validation

import (
	"bytes"
	"encoding/json"
	"slices"
	"strings"

	"genroc/internal/model"
	"genroc/internal/schema"
)

// Changed slots are a FIELD comparison, never a schema judgement (CLAUDE.md). A new Action/Task
// field must be added here; TestChangedSlots_* enumerates both structs so forgetting is loud.

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
	{"only_once", "OnlyOnce", func(t *model.Task) any { return t.OnlyOnce }},
	{"output_schema", "OutputSchema", func(t *model.Task) any { return t.OutputSchema }},
}

// A task with no action reads every one of these as null, so gaining or losing an action
// reports as action.type changing.
var actionSlots = []slot[*model.Action]{
	{"action.type", "Type", func(a *model.Action) any { return a.Type }},
	{"action.url", "URL", func(a *model.Action) any { return a.URL }},
	{"action.method", "Method", func(a *model.Action) any { return a.Method }},
	{"action.headers", "Headers", func(a *model.Action) any { return a.Headers }},
	{"action.query", "Query", func(a *model.Action) any { return a.Query }},
	{"action.accepted_status", "AcceptedStatus", func(a *model.Action) any { return a.AcceptedStatus }},
	{"action.timeout", "Timeout", func(a *model.Action) any { return a.Timeout }},
	{"action.body", "Body", func(a *model.Action) any { return a.Body }},
	{"action.body_schema", "BodySchema", func(a *model.Action) any { return a.BodySchema }},
	{"action.query_schema", "QuerySchema", func(a *model.Action) any { return a.QuerySchema }},
	{"action.input", "Input", func(a *model.Action) any { return a.Input }},
	{"action.input_schema", "InputSchema", func(a *model.Action) any { return a.InputSchema }},
	{"action.result_schema", "ResultSchema", func(a *model.Action) any { return a.ResultSchema }},
	{"action.responses", "Responses", func(a *model.Action) any { return a.Responses }},
	{"action.raises", "Raises", func(a *model.Action) any { return a.Raises }},
	{"action.name", "Name", func(a *model.Action) any { return a.Name }},
	{"action.version", "Version", func(a *model.Action) any { return a.Version }},
	{"action.over", "Over", func(a *model.Action) any { return a.Over }},
	{"action.for", "For", func(a *model.Action) any { return a.For }},
	{"action.until", "Until", func(a *model.Action) any { return a.Until }},
	{"action.tz", "TZ", func(a *model.Action) any { return a.TZ }},
}

// A child_map key is a CALL, so its slots are addressed as an action's are — the address a
// per-key break carries, which lets §6b suppress the slot row.
var childEntrySlots = []slot[model.ChildEntry]{
	{"name", "Name", func(c model.ChildEntry) any { return c.Name }},
	{"version", "Version", func(c model.ChildEntry) any { return c.Version }},
	{"input", "Input", func(c model.ChildEntry) any { return c.Input }},
	{"input_schema", "InputSchema", func(c model.ChildEntry) any { return c.InputSchema }},
	{"result_schema", "ResultSchema", func(c model.ChildEntry) any { return c.ResultSchema }},
	{"raises", "Raises", func(c model.ChildEntry) any { return c.Raises }},
}

// `config_schema` and `$defs` are here to be REPORTED, never judged: without a row an edit to
// either reads as two clean verdicts. A `$defs` break also lands wherever Normalize baked the
// definition in — a different fact.
var definitionSlots = []slot[*model.ProcessDefinition]{
	{"input_schema", "InputSchema", func(d *model.ProcessDefinition) any { return d.InputSchema }},
	{"config_schema", "ConfigSchema", func(d *model.ProcessDefinition) any { return d.ConfigSchema }},
	{"$defs", "Defs", func(d *model.ProcessDefinition) any { return d.Defs }},
	{"output", "Output", func(d *model.ProcessDefinition) any { return d.Output }},
	{"output_schema", "OutputSchema", func(d *model.ProcessDefinition) any { return d.OutputSchema }},
}

// Addresses that name a place rather than a task. specs/compat-command.md §6a.
const (
	addressInput  = "input"
	addressOutput = "output"
	// addressTasks is the task LIST, which is a slot of its own only for its order — the
	// tasks themselves are compared one at a time, under their own addresses.
	addressTasks = "tasks"
)

// slotAddress files an edit where its consequences are read. Action slots are addressed by the
// ACTION TYPE, not the word `action` (§6a).
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

// `responses` must share `result_schema`'s leaf (§6a): a fetch's statuses are compared as one
// union under `.result`, and a separate leaf would print a slot row beside its own break.
func slotLeafName(slot string) string {
	if slot == "result_schema" || slot == "responses" {
		return "result"
	}
	return slot
}

// compat.go builds break addresses from this too; the two must agree, or §6b's suppression sees
// two places.
func childKeyAddress(task, actionType, key string) string {
	return task + ":" + actionType + "." + key
}

// slotAffects is the question a slot BEARS ON (§3a). Empty renders `(not judged)` — which is
// also how a slot forgotten here reads, the safe direction.
func slotAffects(slot string) []Member {
	switch slot {
	// One slot, both questions — the case §3b is entirely about.
	case "input_schema":
		return []Member{MemberUpgrade, MemberContract}
	case "output", "output_schema":
		return []Member{MemberUpgrade}
	// Whether it is ALSO an upgrade concern depends on the action type rather than the slot,
	// so that half is decided by the caller — see taskSlotAffects.
	case "action.result_schema", "action.responses", "action.raises":
		return []Member{MemberContract}
	}
	return nil
}

// definitionSlotAffects is slotAffects at process level, where an output is what callers read:
// compareOutput judges it as the contract. A task's output bears on the contexts after it instead.
func definitionSlotAffects(slot string) []Member {
	if slot == "output" || slot == "output_schema" {
		return []Member{MemberContract}
	}
	return slotAffects(slot)
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
	// A changed KIND reports that alone: the other slots are not comparable across types, and
	// `go:external.url` would name a slot the new type lacks.
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

// Iterate KEY PRESENCE throughout: a declared "202": null is a nil schema, which is also what
// an undeclared status reads as.
func sortedResponseKeys(m map[string]*schema.Schema) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
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
			// Nothing FAILS (the conform strips the orphan output), but a call no longer made is
			// an edit no verdict covers.
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

// A child_map key always parks, so its `result_schema` and `raises` bear on both questions.
// `name` needs no rule (§2c), and a pinned `version` is the child's own row to report.
func childEntrySlotAffects(slot string) []Member {
	if slot == "result_schema" || slot == "raises" {
		return []Member{MemberUpgrade, MemberContract}
	}
	return nil
}

func typeChanged(old, new *model.Task) bool {
	return actionTypeOf(old) != actionTypeOf(new)
}

// taskSlotAffects adds the one action-dependent rule: `result_schema` and `raises` are ALSO an
// upgrade concern where the task parks mid-flight (§2c).
func taskSlotAffects(slot, actionType string) []Member {
	affects := slotAffects(slot)
	if (slot == "action.result_schema" || slot == "action.raises") && parksMidTask(actionType) {
		affects = append([]Member{MemberUpgrade}, affects...)
	}
	return affects
}

// parksMidTask: does the action leave a VALUE the entry context does not describe? Derived from
// ActionType.Holds, which the engine shares — a restated copy would drift silently.
func parksMidTask(actionType string) bool {
	return model.ActionType(actionType).Holds().Result
}

// holdsAnInstance is wider: true for a delay too, whose timer was computed under the old
// definition though it holds no data.
func holdsAnInstance(actionType string) bool {
	return model.ActionType(actionType).Holds().Anything()
}

func actionTypeOf(t *model.Task) string {
	if t.Action == nil {
		return ""
	}
	return string(t.Action.Type)
}

// documentsDiffer is the content half of "unchanged". Task ORDER counts: `switch: next` routes
// by position, so a move reroutes while every slot compares equal.
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
				Affects: definitionSlotAffects(s.name),
			})
		}
	}
	if reordered(old, new) {
		changed = append(changed, SlotChange{Address: addressTasks})
	}
	return changed
}

// Only tasks BOTH versions carry: an insertion shifts everything after it and would report a
// reorder beside the `(added)` row it already caused.
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

// A value that will not marshal reads as changed: an unreadable slot must not read as agreement.
func sameJSON(a, b any) bool {
	x, errA := json.Marshal(a)
	y, errB := json.Marshal(b)
	if errA != nil || errB != nil {
		return false
	}
	return bytes.Equal(x, y)
}

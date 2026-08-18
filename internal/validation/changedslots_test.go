package validation

import (
	"reflect"
	"testing"

	"genroc/internal/model"
	"genroc/internal/schema"
)

// The changed-slot lists are hand-maintained, and a forgotten entry fails SILENTLY — the
// slot stops being reported and every comparison still comes back well-formed. These
// enumerate the structs rather than checking a hand-written list against itself.

// notASlot names the fields deliberately absent from a slot list, each with the reason
// it is absent. A field added to one of these structs matches nothing here and fails.
var notASlot = map[string]string{
	"Task.ID":     "the identity tasks are matched by, not a slot that can differ",
	"Task.Action": "covered field-by-field by actionSlots",
	"Action.Children": "decomposed per key by childEntrySlots. A key is a CALL — one row " +
		"for the whole map cannot say which key moved, and its addresses would not meet the " +
		"per-key addresses a break carries, so §6b could not tell they are the same place",
	"ProcessDefinition.Name": "the identity processes are paired by",
	"ProcessDefinition.Tasks": "compared per task, not as one blob — the per-task verdicts " +
		"are the report, and a whole-list diff would say nothing about where an instance sits. " +
		"Its ORDER is the one thing no per-task comparison can see, and `reordered` reports " +
		"that under the `tasks` address",
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

// A child_map key is a call of its own, so its fields are enumerated like an action's — a
// field added to ChildEntry and forgotten here stops being reported for every key.
func TestChangedSlots_EveryChildEntryFieldIsReported(t *testing.T) {
	assertEveryFieldIsASlot[model.ChildEntry](t, "ChildEntry", coveredFields(childEntrySlots))
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
	for _, s := range childEntrySlots {
		if _, ok := reflect.TypeOf(model.ChildEntry{}).FieldByName(s.field); !ok {
			t.Errorf("child entry slot %q names field %q, which model.ChildEntry does not have", s.name, s.field)
		}
	}
}

// everySlotDoc exercises every field of every struct the comparison walks, so a mutation
// below can move exactly one of them. It analyses on its own: no child it names has to
// exist, because Generate reads the declared result schemas rather than resolving the call.
const everySlotDoc = `{"name":"total",
 "input_schema":{"type":"object","properties":{"id":{"type":"string"}},"required":["id"]},
 "config_schema":{"type":"object","properties":{"region":{"type":"string"}}},
 "$defs":{"Spare":{"type":"object","properties":{"code":{"type":"string"}}}},
 "output":{"ok":"$: true"},
 "tasks":[
  {"id":"call","timeout":"30s","only_once":true,
   "on_error":[{"code":["http.500"],"goto":"$wait"}],
   "action":{"type":"fetch","url":"http://x/call","method":"POST",
             "headers":{"trace":"alpha"},"body":{"n":"$: 1"},"accepted_status":["4xx"],
             "responses": { "200": {"type":"object","properties":{"fee":{"type":"number"}}} }},
   "switch":"next"},
  {"id":"wait","action":{"type":"delay","for":"1h","tz":"UTC"},
   "output":{"n":"$: 1"},"switch":"next"},
  {"id":"hold","action":{"type":"delay","until":"+1d 08:00"},"switch":"next"},
  {"id":"work","action":{"type":"child","name":"kid","version":1,"input":{"seed":"$: 1"},
             "result_schema":{"type":"object","properties":{"id":{"type":"string"}}}},
   "switch":"next"},
  {"id":"each","action":{"type":"child_list","name":"kid","over":"$: [1, 2]"},"switch":"next"},
  {"id":"fan","action":{"type":"child_map","children":{
             "a":{"name":"kid","version":1,"input":{"seed":"$: 1"},
                  "result_schema":{"type":"object","properties":{"id":{"type":"string"}}}},
             "b":{"name":"kid"}}},
   "switch":"end"}]}`

// A document that DIFFERS must produce a row. The reflection tests above catch a field the
// slot lists forgot; this catches an edit those lists cannot see at all — task order, and a
// child_map key that stopped existing. The oracle is the marshalled document, so it shares
// nothing with the machinery under test: if the bytes moved, the report says something.
func TestChangedSlots_EveryDifferentDocumentIsReported(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*model.ProcessDefinition)
	}{
		{"input_schema", func(d *model.ProcessDefinition) { d.InputSchema = nil }},
		{"config_schema", func(d *model.ProcessDefinition) { d.ConfigSchema = nil }},
		{"$defs nothing references", func(d *model.ProcessDefinition) { d.Defs = schema.Defs{} }},
		{"output", func(d *model.ProcessDefinition) { d.Output = nil }},
		{"task order", func(d *model.ProcessDefinition) {
			d.Tasks[1], d.Tasks[2] = d.Tasks[2], d.Tasks[1]
		}},
		// Prepended rather than appended: a new entry task is reachable without rerouting
		// anything, so the edit stays one edit.
		{"a task added", func(d *model.ProcessDefinition) {
			entry := &model.Task{
				ID:     "extra",
				Action: &model.Action{Type: model.ActionTypeFetch, URL: "http://x/extra"},
				Switch: model.SwitchMap{{Goto: model.GotoNext}},
			}
			d.Tasks = append([]*model.Task{entry}, d.Tasks...)
		}},
		{"a task removed", func(d *model.ProcessDefinition) { d.Tasks = d.Tasks[1:] }},
		{"timeout", func(d *model.ProcessDefinition) { d.Tasks[0].Timeout = model.TimeoutFor("60s") }},
		{"only_once", func(d *model.ProcessDefinition) { d.Tasks[0].OnlyOnce = nil }},
		{"on_error", func(d *model.ProcessDefinition) { d.Tasks[0].OnError = nil }},
		{"task output", func(d *model.ProcessDefinition) { d.Tasks[1].Output = nil }},
		{"switch", func(d *model.ProcessDefinition) {
			d.Tasks[0].Switch = model.SwitchMap{{Goto: model.GotoEnd}}
		}},
		{"action.type", func(d *model.ProcessDefinition) { d.Tasks[0].Action.Type = model.ActionTypeExternal }},
		{"action.url", func(d *model.ProcessDefinition) { d.Tasks[0].Action.URL = "http://y/call" }},
		{"action.method", func(d *model.ProcessDefinition) { d.Tasks[0].Action.Method = "PUT" }},
		{"action.headers", func(d *model.ProcessDefinition) { d.Tasks[0].Action.Headers = nil }},
		{"action.accepted_status", func(d *model.ProcessDefinition) { d.Tasks[0].Action.AcceptedStatus = nil }},
		{"action.body", func(d *model.ProcessDefinition) { d.Tasks[0].Action.Body = nil }},
		{"action.result_schema", func(d *model.ProcessDefinition) { d.Tasks[3].Action.ResultSchema = nil }},
		{"action.responses", func(d *model.ProcessDefinition) { delete(d.Tasks[0].Action.Responses, "200") }},
		{"a responses schema", func(d *model.ProcessDefinition) {
			d.Tasks[0].Action.Responses["200"] = nil
		}},
		// A status the new version declares with NO body carries no schema to compare, so
		// nothing but this channel can report it — and it changes what the task accepts.
		{"a bodyless status added", func(d *model.ProcessDefinition) {
			d.Tasks[0].Action.Responses["202"] = nil
		}},
		{"action.for", func(d *model.ProcessDefinition) { d.Tasks[1].Action.For = "2h" }},
		{"action.tz", func(d *model.ProcessDefinition) { d.Tasks[1].Action.TZ = "Europe/Prague" }},
		{"action.until", func(d *model.ProcessDefinition) { d.Tasks[2].Action.Until = "+2d 08:00" }},
		{"action.name", func(d *model.ProcessDefinition) { d.Tasks[3].Action.Name = "other" }},
		{"action.version", func(d *model.ProcessDefinition) { d.Tasks[3].Action.Version = 0 }},
		{"action.input", func(d *model.ProcessDefinition) { d.Tasks[3].Action.Input = nil }},
		{"action.over", func(d *model.ProcessDefinition) { d.Tasks[4].Action.Over = "$: [1, 2, 3]" }},
		{"a child_map key removed", func(d *model.ProcessDefinition) {
			delete(d.Tasks[5].Action.Children, "b")
		}},
		{"a child_map key added", func(d *model.ProcessDefinition) {
			d.Tasks[5].Action.Children["c"] = model.ChildEntry{Name: "kid"}
		}},
		{"a child_map key's name", func(d *model.ProcessDefinition) {
			d.Tasks[5].Action.Children["b"] = model.ChildEntry{Name: "other"}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			old, updated := definitionFromJSON(t, everySlotDoc), definitionFromJSON(t, everySlotDoc)
			tc.mutate(updated)
			if sameJSON(old, updated) {
				t.Fatal("the mutation left the document identical; the case proves nothing")
			}
			report, err := Compare(old, updated)
			if err != nil {
				t.Fatalf("Compare: %v", err)
			}
			if len(report.Changed)+len(report.Added)+len(report.Issues) == 0 {
				t.Error("the documents differ and the report is empty: two clean verdicts over " +
					"an edit reported nowhere is the answer a comparison exists to prevent")
			}
		})
	}
}

// A task compared against itself must report nothing: the slot comparison is over JSON
// bytes, so a slot whose type marshals unstably would differ on every single task.
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

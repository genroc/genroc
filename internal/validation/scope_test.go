package validation

import (
	"reflect"
	"strings"
	"testing"

	"genroc/internal/model"
)

// A slot missing from preOutputSlots is checked in a scope the engine never populates. The
// reflection is the point: a new model.Action field fails here.
func TestPreOutputSlotsCoversEveryActionSlot(t *testing.T) {
	// Fields that carry no expression. Each is data the engine reads directly, so there is
	// nothing to type-check against a context.
	carriesNoExpression := map[string]bool{
		"Type": true, "Name": true, "Version": true,
		"Responses": true, "ResultSchema": true, "Raises": true,
		"TZ": true, // an IANA name, parsed by delayspec rather than evaluated
		// The declared slot schemas: documents describing the shape BESIDE them, never
		// evaluated against a context. specs/declared-slot-schemas.md.
		"BodySchema": true, "QuerySchema": true, "InputSchema": true,
	}

	action := &model.Action{Type: model.ActionTypeFetch, Method: "post"}
	want := map[string]string{} // field name -> the sentinel planted in it
	av := reflect.ValueOf(action).Elem()
	at := av.Type()
	for i := 0; i < at.NumField(); i++ {
		f := at.Field(i)
		if f.Anonymous {
			continue // DelaySpec: its own fields are visited below
		}
		if carriesNoExpression[f.Name] {
			continue
		}
		plantSentinel(t, av.Field(i), f.Name, want)
	}
	dv := reflect.ValueOf(&action.DelaySpec).Elem()
	for i := 0; i < dv.Type().NumField(); i++ {
		f := dv.Type().Field(i)
		if carriesNoExpression[f.Name] {
			continue
		}
		plantSentinel(t, dv.Field(i), f.Name, want)
	}

	task := &model.Task{ID: "t", Action: action}

	var found []string
	for _, slot := range preOutputSlots(task) {
		found = append(found, describe(slot.raw))
	}
	joined := strings.Join(found, "\n")
	for field, sentinel := range want {
		if !strings.Contains(joined, sentinel) {
			t.Errorf("Action.%s carries an expression but preOutputSlots does not list it; "+
				"a slot missing there is checked in a scope the engine never populates.\nlisted:\n%s",
				field, joined)
		}
	}
}

// An unhandled field type fails, so a new kind of slot must be taught here before
// preOutputSlots can forget it.
func plantSentinel(t *testing.T, v reflect.Value, name string, want map[string]string) {
	t.Helper()
	sentinel := "$: sentinel_" + name
	switch {
	case v.Type() == reflect.TypeOf((*model.Shape)(nil)):
		v.Set(reflect.ValueOf(&model.Shape{Raw: sentinel}))
	case v.Kind() == reflect.String:
		v.SetString(sentinel)
	case v.Kind() == reflect.Interface: // For / Until, typed `any`
		v.Set(reflect.ValueOf(sentinel))
	case v.Type() == reflect.TypeOf(model.Timeout{}):
		v.Set(reflect.ValueOf(model.TimeoutFor(sentinel)))
	case v.Type() == reflect.TypeOf(map[string]model.ChildEntry(nil)):
		v.Set(reflect.ValueOf(map[string]model.ChildEntry{
			"k": {Name: "c", Input: &model.Shape{Raw: sentinel}},
		}))
	default:
		t.Fatalf("Action.%s is a %s, which this test does not know how to plant an expression in; "+
			"teach it the new slot kind, then check preOutputSlots lists it", name, v.Type())
	}
	want[name] = sentinel
}

func describe(raw any) string {
	switch v := raw.(type) {
	case string:
		return v
	case map[string]any:
		var parts []string
		for _, val := range v {
			parts = append(parts, describe(val))
		}
		return strings.Join(parts, " ")
	default:
		return ""
	}
}

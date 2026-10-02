package api

import (
	"reflect"
	"testing"
)

// Exempt only where the externalized value IS what the caller came for; a new exemption must
// argue for itself here. CLAUDE.md, "A list row must be complete".
var listRowsMayBeIncomplete = map[string]bool{
	"ExternalTaskResp": true,
	"LogEntryResp":     true,
}

func TestListRowsCarryNothingIncomplete(t *testing.T) {
	seen := 0
	for _, a := range registry {
		if a.Resp == nil {
			continue
		}
		row := listRowType(a.Resp)
		if row == nil {
			continue // not a listing
		}
		seen++
		if _, hasObjects := row.FieldByName("Objects"); hasObjects && !listRowsMayBeIncomplete[row.Name()] {
			t.Errorf("%s (%s) lists %s, which carries `objects` — a list row must be complete, "+
				"or be named in listRowsMayBeIncomplete with the reason", a.Name, a.Path, row.Name())
		}
	}
	// The registry is walked reflectively, so a listing that stops being recognised as one would
	// take its own coverage with it silently.
	if seen < 4 {
		t.Errorf("only %d listing responses found; the walk has stopped seeing them", seen)
	}
}

// listRowType sees through the one map-shaped page only because Resp is a VALUE, not just a
// type.
func listRowType(resp any) reflect.Type {
	rv := reflect.ValueOf(resp)
	switch rv.Kind() {
	case reflect.Struct:
		items, ok := rv.Type().FieldByName("Items")
		if !ok || items.Type.Kind() != reflect.Slice {
			return nil
		}
		return items.Type.Elem()
	case reflect.Map:
		v := rv.MapIndex(reflect.ValueOf("items"))
		if !v.IsValid() {
			return nil
		}
		inner := reflect.ValueOf(v.Interface())
		if inner.Kind() != reflect.Slice {
			return nil
		}
		return inner.Type().Elem()
	}
	return nil
}

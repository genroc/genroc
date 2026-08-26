package api

import (
	"reflect"
	"testing"
)

// A LIST response must not carry a value that can be externalized, and therefore must not carry
// an `objects` listing to explain one. A row that came back with a slot silently emptied is a
// row a caller computes on and gets wrong -- and unlike a single-instance fetch there is no
// obvious place to notice it. The single-instance endpoints are where an incomplete value
// belongs, because they can list what was cut and the caller is asking about one thing.
//
// Two endpoints are exempt, and for the same reason: the externalized value IS what the caller
// came for, so omitting it does not make the response complete, it makes it useless.
//
//	external-tasks  a worker claims a task to get its input
//	logs            a log entry exists to carry its payload
//
// Anything else growing an `objects` field is a new exception and needs to argue for itself
// here rather than arrive by inheritance from a shared struct.
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

// listRowType returns the row type of a paginated response, or nil if the response is not a
// listing. Two shapes reach here: PageResp[T], and the one endpoint that spells its page out as
// a map -- which reflection can only see through because Resp is a VALUE, not just a type.
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

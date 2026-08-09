package schema

import (
	"reflect"
	"testing"
)

// This test lives in package schema, not schematest, because node and the walk table are
// unexported and the check is reflection over the struct.

func TestMapChildrenVisitsEverySubSchemaField(t *testing.T) {
	rt := reflect.TypeOf(node{})
	for i := range rt.NumField() {
		f := rt.Field(i)
		if !holdsSubSchema(f.Type) {
			continue
		}
		sentinel := &node{Description: "sentinel"}
		n := &node{}
		reflect.ValueOf(n).Elem().Field(i).Set(sampleOf(f.Type, sentinel))

		visited := false
		for _, c := range children(n) {
			if c == sentinel {
				visited = true
			}
		}
		if !visited {
			t.Errorf("mapChildren does not visit %s (%s): every structural walk reads that table, so a "+
				"subtree there is silently skipped — an unstripped $defs cycles the marshaler, an "+
				"uncanonicalized node stops the inference fixpoint converging. Add the slot in walk.go.",
				f.Name, f.Tag.Get("json"))
		}
	}
}

func holdsSubSchema(t reflect.Type) bool {
	nodePtr := reflect.TypeOf((*node)(nil))
	switch t.Kind() {
	case reflect.Ptr:
		return t == nodePtr
	case reflect.Slice:
		return t.Elem() == nodePtr
	case reflect.Map:
		return t.Elem() == nodePtr
	}
	return false
}

func sampleOf(t reflect.Type, child *node) reflect.Value {
	switch t.Kind() {
	case reflect.Slice:
		v := reflect.MakeSlice(t, 1, 1)
		v.Index(0).Set(reflect.ValueOf(child))
		return v
	case reflect.Map:
		v := reflect.MakeMap(t)
		v.SetMapIndex(reflect.ValueOf("k"), reflect.ValueOf(child))
		return v
	default:
		return reflect.ValueOf(child)
	}
}

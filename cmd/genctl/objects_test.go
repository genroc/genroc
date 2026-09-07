package main

import (
	"encoding/json"
	"testing"
)

// The `objects` section names paths into the WHOLE response, so a value being displayed is one
// branch of it: `get` renders state, whose paths start ["state", …], beside error_data's.
func TestWithObjectRefs(t *testing.T) {
	entry := func(size int64, path ...any) objectEntry {
		return objectEntry{Path: path, Ref: "dcc0d73035af6d62f007e55698fd971f", Size: size}
	}
	marker := `{"ref":"dcc0d73035af6d62f007e55698fd971f","size":9}`

	for _, tc := range []struct {
		name    string
		value   string
		objects []objectEntry
		at      []any
		want    string
	}{
		{
			name:    "a leaf is marked where it was cut from",
			value:   `{"input":{"who":"world"}}`,
			objects: []objectEntry{entry(9, "state", "input", "blob")},
			at:      []any{"state"},
			want:    `{"input":{"blob":` + marker + `,"who":"world"}}`,
		},
		{
			name:    "a path outside the rendered value belongs to nobody here",
			value:   `{"input":{"who":"world"}}`,
			objects: []objectEntry{entry(9, "error_data", "blob")},
			at:      []any{"state"},
			want:    `{"input":{"who":"world"}}`,
		},
		{
			name:    "a value externalized whole replaces the value itself",
			value:   `{"who":"world"}`,
			objects: []objectEntry{entry(9, "data")},
			at:      []any{"data"},
			want:    marker,
		},
		{
			name:    "an array position is a path segment like any other",
			value:   `{"items":["a","b"]}`,
			objects: []objectEntry{entry(9, "state", "items", float64(1))},
			at:      []any{"state"},
			want:    `{"items":["a",` + marker + `]}`,
		},
		{
			name:  "nothing externalized leaves the value alone",
			value: `{"who":"world"}`,
			at:    []any{"state"},
			want:  `{"who":"world"}`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var v any
			if err := json.Unmarshal([]byte(tc.value), &v); err != nil {
				t.Fatalf("bad test value: %v", err)
			}
			got, err := json.Marshal(withObjectRefs(v, tc.objects, tc.at...))
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			if string(got) != tc.want {
				t.Errorf("withObjectRefs:\n got %s\nwant %s", got, tc.want)
			}
		})
	}
}

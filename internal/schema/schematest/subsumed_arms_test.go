package schematest

import (
	"encoding/json"
	"testing"
)

func TestWithoutSubsumedArms(t *testing.T) {
	for _, tc := range []struct {
		name, in, want string
		// value fits the result; under `in` it matched two arms, and oneOf refused it.
		value string
	}{
		{
			"a ref naming an array absorbs the empty array",
			`{"$defs":{"T":{"type":"array","items":{"type":"integer"}}},
			  "oneOf":[{"$ref":"#/$defs/T"},{"type":"array","maxItems":0}]}`,
			`{"$ref":"#/$defs/T"}`, `[]`,
		},
		{
			"refs naming one scalar collapse to the inline arm",
			`{"$defs":{"A":{"type":"string"},"B":{"type":"string"}},
			  "oneOf":[{"$ref":"#/$defs/A"},{"$ref":"#/$defs/B"},{"type":"string"}]}`,
			`{"type":"string"}`, `"x"`,
		},
		{
			"a union below a property is pruned too",
			`{"$defs":{"T":{"type":"array","items":{"type":"integer"}}},
			  "type":"object","required":["ids"],
			  "properties":{"ids":{"oneOf":[{"$ref":"#/$defs/T"},{"type":"array","maxItems":0}]}}}`,
			`{"type":"object","properties":{"ids":{"$ref":"#/$defs/T"}},"required":["ids"]}`, `{"ids":[]}`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			in := mustAssumed(t, tc.in)
			got := in.WithoutSubsumedArms(in.DefsHandle())
			if s := mustJSON(t, got.WithoutDefs()); s != tc.want {
				t.Errorf("got %s, want %s", s, tc.want)
			}
			var v any
			if err := json.Unmarshal([]byte(tc.value), &v); err != nil {
				t.Fatal(err)
			}
			if _, err := got.Validate(v); err != nil {
				t.Errorf("%s does not validate against the pruned type: %v — a $process caller "+
					"conforms the child's output to it", tc.value, err)
			}
		})
	}
}

func TestWithoutSubsumedArms_KeepsWhatAConformWouldStrip(t *testing.T) {
	in := mustAssumed(t, `{"$defs":{"O":{"type":"object","properties":{"x":{"type":"integer"},"y":{"type":"string"}},"required":["x","y"]}},
	  "oneOf":[{"$ref":"#/$defs/O"},{"type":"object","properties":{"x":{"type":"integer"}},"required":["x"]}]}`)
	got := in.WithoutSubsumedArms(in.DefsHandle())
	if len(got.Variants()) != 2 {
		t.Errorf("got %s: O's values fit {x} only once `y` is stripped, so dropping O loses `y`",
			mustJSON(t, got.WithoutDefs()))
	}
}

func TestWithoutSubsumedArms_LeavesAReferenceCycleAlone(t *testing.T) {
	pool := mustAssumed(t, `{"$defs":{
	  "A":{"oneOf":[{"$ref":"#/$defs/B"},{"type":"string"}]},
	  "B":{"oneOf":[{"$ref":"#/$defs/A"},{"type":"string"}]}}}`).DefsHandle()
	for _, name := range []string{"A", "B"} {
		d, _ := pool.Get(name)
		if got := d.WithoutSubsumedArms(pool); len(got.Variants()) != 2 {
			t.Errorf("%s pruned to %s: each covers the other's string arm, so pruning both leaves "+
				"a cycle with no base case", name, mustJSON(t, got))
		}
	}
}

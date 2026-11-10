package schematest

import (
	"encoding/json"
	"testing"
)

func TestWithoutOverlappingArms(t *testing.T) {
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
			// {x} takes {x: 1, y: "s"} by stripping `y`: dropping either arm loses `y` or refuses {x}.
			"an object another takes only by losing keys is joined with it",
			`{"$defs":{"T":{"type":"object","properties":{"x":{"type":"integer"},"y":{"type":"string"}},"required":["x","y"]}},
			  "oneOf":[{"$ref":"#/$defs/T"},{"type":"object","properties":{"x":{"type":"integer"}},"required":["x"]}]}`,
			`{"type":"object","properties":{"x":{"type":"integer"},"y":{"type":["null","string"]}},"required":["x"]}`,
			`{"x":1,"y":"s"}`,
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
			got := in.WithoutOverlappingArms(in.DefsHandle(), "")
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

// `input.o ?? {b: 2}` offers both shapes on purpose: each requires a key the other lacks, so no
// value fits both, and joining them would admit {} where neither arm does.
func TestWithoutOverlappingArms_KeepsShapesNoValueShares(t *testing.T) {
	in := mustAssumed(t, `{"oneOf":[
	  {"type":"object","properties":{"a":{"type":"integer"}},"required":["a"]},
	  {"type":"object","properties":{"b":{"type":"integer"}},"required":["b"]}]}`)
	if got := in.WithoutOverlappingArms(in.DefsHandle(), ""); len(got.Variants()) != 2 {
		t.Errorf("got %s, want both shapes kept", mustJSON(t, got))
	}
}

// A loop's output names itself; the union a caller builds over it sits outside that cycle, so
// joining there cannot change the definition the proof reads.
func TestWithoutOverlappingArms_JoinsARecursiveArmFromOutsideItsCycle(t *testing.T) {
	in := mustAssumed(t, `{"$defs":{"T":{"type":"object","required":["count","prev"],
	    "properties":{"count":{"type":"integer"},"prev":{"oneOf":[{"$ref":"#/$defs/T"},{"type":"null"}]}}}},
	  "oneOf":[{"$ref":"#/$defs/T"},{"type":"object","properties":{"count":{"type":"integer"}},"required":["count"]}]}`)
	got := in.WithoutOverlappingArms(in.DefsHandle(), "")
	if _, err := got.Validate(decodeValue(t, `{"count":2,"prev":{"count":1,"prev":null}}`)); err != nil {
		t.Errorf("got %s, which refuses a value of T: %v — the {count} arm takes it too, by stripping prev",
			mustJSON(t, got.WithoutDefs()), err)
	}
}

func TestWithoutOverlappingArms_LeavesAReferenceCycleAlone(t *testing.T) {
	pool := mustAssumed(t, `{"$defs":{
	  "A":{"oneOf":[{"$ref":"#/$defs/B"},{"type":"string"}]},
	  "B":{"oneOf":[{"$ref":"#/$defs/A"},{"type":"string"}]}}}`).DefsHandle()
	for _, name := range []string{"A", "B"} {
		d, _ := pool.Get(name)
		if got := d.WithoutOverlappingArms(pool, name); len(got.Variants()) != 2 {
			t.Errorf("%s pruned to %s: each covers the other's string arm, so pruning both leaves "+
				"a cycle with no base case", name, mustJSON(t, got))
		}
	}
}

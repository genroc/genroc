// Generation-level tests for recursive output types: exact collapse of
// degenerate cycles, kept structural recursion (self and mutual), and mixed
// computational/structural output maps. See specs/recursive-type-inference.md.
package validationtest

import (
	"strings"
	"testing"
)

// A bare `$: self.previous ?? input` output is the coinductive tautology
// X = X ∨ I: it collapses to the input type exactly — no fixpoint widening, no
// recursive wrapper. The input itself was passed through whole, so the
// collapsed type is the reference to the input definition.
func TestGenerate_DegenerateSelfOutputCollapsesToInput(t *testing.T) {
	out := runGenerate(t, `{
		"name": "p",
		"input_schema": {"type":"object","properties":{"seed":{"type":"integer"}},"required":["seed"]},
		"tasks": [
			{
				"id": "loop",
				"output": "$: self.previous ?? input",
				"switch": [{"case":"(self.output.seed ?? 0) < 10","goto":"$loop"},{"goto":"end"}]
			}
		]
	}`)
	assertJSON(t, defOf(out, "loop_output"), `{"$ref":"#/$defs/input"}`)
	assertJSON(t, defOf(out, "input"),
		`{"type":"object","properties":{"seed":{"type":"integer"}},"required":["seed"]}`)
}

// A bare `$: self.previous` with no base case is X = X ∨ null (the wrapper
// adds the null of "no previous iteration"), which collapses to exactly null —
// the value it will always hold at runtime.
func TestGenerate_PureSelfOutputCollapsesToNull(t *testing.T) {
	out := runGenerate(t, `{
		"name": "p",
		"input_schema": {"type":"object","properties":{"n":{"type":"integer"}},"required":["n"]},
		"tasks": [
			{
				"id": "loop",
				"output": "$: self.previous",
				"switch": [{"case":"input.n < 10","goto":"$loop"},{"goto":"end"}]
			}
		]
	}`)
	assertJSON(t, defOf(out, "loop_output"), `{"type":"null"}`)
}

// A mixed output map: one field is a computational accumulator (fixpointed to
// its scalar type), the other passes the previous value through whole (kept as
// a recursive $ref). Both coexist in one definition.
func TestGenerate_MixedComputationalAndStructuralRecursion(t *testing.T) {
	out := runGenerate(t, `{
		"name": "p",
		"tasks": [
			{
				"id": "loop",
				"output": {
					"count": "$: (self.previous.count ?? 0) + 1",
					"trail": "$: self.previous"
				},
				"switch": [{"case":"self.output.count < 10","goto":"$loop"},{"goto":"end"}]
			}
		]
	}`)
	got := mustMarshal(defOf(out, "loop_output").AsMap())
	if !strings.Contains(got, `"count":{"type":"integer"}`) {
		t.Errorf("count did not fixpoint to integer: %s", got)
	}
	if !strings.Contains(got, `"$ref":"#/$defs/loop_output"`) {
		t.Errorf("trail did not keep the recursive $ref: %s", got)
	}
}

// Mutual structural recursion across two tasks in one loop: each passes the
// other's output through whole, yielding a pair of mutually-recursive
// definitions — legal, because the references sit under properties.
func TestGenerate_MutualStructuralRecursionKept(t *testing.T) {
	out := runGenerate(t, `{
		"name": "p",
		"input_schema": {"type":"object","properties":{"n":{"type":"integer"}},"required":["n"]},
		"tasks": [
			{
				"id": "a",
				"output": {"prev_b": "$: outputs.b", "n": "$: (self.previous.n ?? 0) + 1"},
				"switch": "next"
			},
			{
				"id": "b",
				"output": {"prev_a": "$: outputs.a"},
				"switch": [{"case":"(outputs.a.n ?? 0) < input.n","goto":"$a"},{"goto":"end"}]
			}
		]
	}`)
	a := mustMarshal(defOf(out, "a_output").AsMap())
	b := mustMarshal(defOf(out, "b_output").AsMap())
	if !strings.Contains(a, `"$ref":"#/$defs/b_output"`) {
		t.Errorf("a_output does not reference b_output: %s", a)
	}
	if !strings.Contains(b, `"$ref":"#/$defs/a_output"`) {
		t.Errorf("b_output does not reference a_output: %s", b)
	}
}

// Generation is deterministic: the same definition generated twice yields
// byte-identical schema files, recursive types included.
func TestGenerate_RecursiveGenerationDeterministic(t *testing.T) {
	def := `{
		"name": "p",
		"input_schema": {"type":"object","properties":{"seed":{"type":"integer"}},"required":["seed"]},
		"tasks": [
			{
				"id": "loop",
				"output": {
					"count": "$: (self.previous.count ?? 0) + 1",
					"trail": "$: self.previous",
					"snap":  "$: self.previous ?? input"
				},
				"switch": [{"case":"self.output.count < 10","goto":"$loop"},{"goto":"end"}]
			}
		]
	}`
	first := mustMarshal(runGenerate(t, def))
	second := mustMarshal(runGenerate(t, def))
	if first != second {
		t.Errorf("generation is not deterministic:\n first:  %s\n second: %s", first, second)
	}
}

// The recursive definitions a generation emits must themselves be well-formed
// schema documents — every kept cycle productive — so a stored definition
// re-parses cleanly.
func TestGenerate_EmittedRecursiveDefsPassCheckDoc(t *testing.T) {
	out := runGenerate(t, `{
		"name": "p",
		"input_schema": {"type":"object","properties":{"seed":{"type":"integer"}},"required":["seed"]},
		"tasks": [
			{
				"id": "loop",
				"output": {"count": "$: (self.previous.count ?? 0) + 1", "trail": "$: self.previous"},
				"switch": [{"case":"self.output.count < 10","goto":"$loop"},{"goto":"end"}]
			}
		]
	}`)
	for _, name := range defKeys(out) {
		root := defOf(out, name).WithMergedDefs(out.Defs)
		if err := root.CheckDoc(); err != nil {
			t.Errorf("emitted definition %q fails CheckDoc: %v", name, err)
		}
	}
}

// MUTUAL recursion between two tasks' outputs, which is the only shape that hands `??` a BARE
// `$ref` onto a definition still being solved. `self.previous` never does: its nullability is a
// use-site wrapper the strip removes, so by the time `??` sees it the reference is gone.
//
// A read that closes the cycle is served the running estimate, wrapped nullable at the use site
// (`estimateNode`) — and `inferNullCoalesce` has to unwrap that, or the recovery's own result is
// nullable and the fixpoint describes a value that cannot be null as if it could. Both shapes
// below are accepted either way, so what this pins is the TYPE: without the unwrap the arm that
// carries the recursion is dropped and the definition collapses to the base case.
func TestGenerate_MutualOutputRecursionUnwrapsTheEstimate(t *testing.T) {
	def := func(aOut, bOut string) string {
		return `{"name":"p","tasks":[
		 {"id":"a","action":{"type":"fetch","method":"get","url":"http://x",
		   "responses":{"200":{"type":"object","properties":{"s":{"type":"string"},"more":{"type":"boolean"}},"required":["s","more"]}}},
		  "output":` + aOut + `,
		  "switch":[{"case":"self.result.more","goto":"$b"},{"goto":"end"}]},
		 {"id":"b","action":{"type":"fetch","method":"get","url":"http://y",
		   "responses":{"200":{"type":"object","properties":{"s":{"type":"string"},"more":{"type":"boolean"}},"required":["s","more"]}}},
		  "output":` + bOut + `,
		  "switch":[{"case":"self.result.more","goto":"$a"},{"goto":"end"}]}]}`
	}

	// The whole output, so the operand reaches `??` as the bare reference.
	t.Run("the recursive arm survives", func(t *testing.T) {
		out := runGenerate(t, def(`"$: outputs.b ?? self.result.s"`, `"$: outputs.a ?? self.result.s"`))
		if got := mustMarshal(defOf(out, "a_output")); got != `{"oneOf":[{"$ref":"#/$defs/b_output"},{"type":"string"}]}` {
			t.Errorf("a_output = %s\nwant the recursion kept beside the base case", got)
		}
	})

	// A PROPERTY of it navigates first, and navigation materializes the null into the property's
	// own type — so this one never reaches the unwrap, and says so by being unaffected by it.
	t.Run("a property navigates the null out first", func(t *testing.T) {
		out := runGenerate(t, def(`{"v":"$: outputs.b.v ?? self.result.s"}`, `{"v":"$: outputs.a.v ?? self.result.s"}`))
		if got := mustMarshal(defOf(out, "a_output")); got != `{"type":"object","properties":{"v":{"type":"string"}},"required":["v"]}` {
			t.Errorf("a_output = %s, want the collapsed base case", got)
		}
	})
}

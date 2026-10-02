// Recursive output types through Generate: degenerate cycles collapse, structural recursion is
// kept. specs/recursive-type-inference.md.
package validationtest

import (
	"strings"
	"testing"
)

// `$: self.previous ?? input` is X = X ∨ I: it collapses to the input reference exactly — no
// widening, no recursive wrapper.
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

// X = X ∨ null collapses to exactly null, what it always holds at runtime.
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

// An accumulator fixpoints to its scalar; a pass-through stays a recursive $ref.
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

// Legal because the references sit under properties.
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

// Every kept cycle must be productive, so a stored definition re-parses cleanly.
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

// Only MUTUAL recursion hands `??` a BARE `$ref` to a definition still being solved. Both shapes
// pass even if inferNullCoalesce stops unwrapping the use-site estimate (`estimateNode`), so this
// pins the TYPE: without the unwrap the recursive arm drops and it collapses to the base case.
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

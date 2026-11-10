package validationtest

import (
	"encoding/json"
	"fmt"
	"testing"

	"genroc/internal/validation"
)

// `t` sets {x, y} or does not run, and `v` falls back to {x}. Each builder of a union is spelled
// twice: the arm read inline (`outputs.t.o`) and read whole (`outputs.t`, a `$ref`).
func unionDef(inTask, whole bool) string {
	tOut, read := `{"o": {"x": 1, "y": "s"}}`, "outputs.t.o"
	if whole {
		tOut, read = `{"x": 1, "y": "s"}`, "outputs.t"
	}
	expr := fmt.Sprintf("$: %s ?? {x: 2}", read)
	tSwitch, tail, out := `"end"`, `{"id": "tail", "switch": "end"}`, expr
	if inTask {
		tSwitch, tail, out = `"next"`, fmt.Sprintf(`{"id": "tail", "output": %q, "switch": "end"}`, expr), "$: outputs.tail"
	}
	return fmt.Sprintf(`{
		"name": "u",
		"input_schema": {"type": "object", "properties": {"skip": {"type": "boolean"}}, "required": ["skip"]},
		"tasks": [
			{"id": "start", "switch": [{"case": "input.skip", "goto": "$tail"}, {"goto": "next"}]},
			{"id": "t", "output": %s, "switch": %s},
			%s
		],
		"output": {"v": %q}
	}`, tOut, tSwitch, tail, out)
}

func TestUnionOfObjectsDoesNotTurnOnARef(t *testing.T) {
	for _, b := range []struct {
		name   string
		inTask bool
	}{{"joined over endings", false}, {"?? inside one task", true}} {
		t.Run(b.name, func(t *testing.T) {
			inline := runGenerate(t, unionDef(b.inTask, false))
			ref := runGenerate(t, unionDef(b.inTask, true))
			if a, r := outputV(t, inline), outputV(t, ref); a != r {
				t.Errorf("v read inline types as\n  %s\nand read as a $ref as\n  %s\n"+
					"— a $ref is a fact about the document, not about the value", a, r)
			}
			for spelling, sf := range map[string]validation.SchemaFile{"inline": inline, "$ref": ref} {
				for _, v := range []string{`{"v": {"x": 1, "y": "s"}}`, `{"v": {"x": 2}}`} {
					var val any
					if err := json.Unmarshal([]byte(v), &val); err != nil {
						t.Fatal(err)
					}
					if _, err := sf.ProcessOutput.WithDefs(sf.Defs).Validate(val); err != nil {
						t.Errorf("%s: the process outputs %s and its own published type refuses it: %v "+
							"— a $process caller conforms the child's output to that type", spelling, v, err)
					}
				}
			}
		})
	}
}

// outputV is the type of `v` with every $ref inlined, so the two spellings compare.
func outputV(t *testing.T, sf validation.SchemaFile) string {
	t.Helper()
	raw, err := json.Marshal(sf.ProcessOutput.WithDefs(sf.Defs))
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	defs, _ := doc["$defs"].(map[string]any)
	delete(doc, "$defs")
	var inline func(n any, depth int) any
	inline = func(n any, depth int) any {
		if depth > 16 {
			t.Fatal("a recursive $ref; this comparison covers none")
		}
		switch m := n.(type) {
		case map[string]any:
			if ref, ok := m["$ref"].(string); ok && len(m) == 1 {
				return inline(defs[ref[len("#/$defs/"):]], depth+1)
			}
			out := map[string]any{}
			for k, v := range m {
				out[k] = inline(v, depth)
			}
			return out
		case []any:
			out := make([]any, len(m))
			for i, v := range m {
				out[i] = inline(v, depth)
			}
			return out
		}
		return n
	}
	v := inline(doc, 0).(map[string]any)["properties"].(map[string]any)["v"]
	b, _ := json.Marshal(v)
	return string(b)
}

// `t` loops, so t_output names itself; the process output is outside that cycle, so joining the
// `{count}` fallback with it cannot change t_output.
func TestUnionWithARecursiveArmTakesWhatTheLoopOutputs(t *testing.T) {
	sf := runGenerate(t, `{
		"name": "r",
		"input_schema": {"type": "object", "properties": {"skip": {"type": "boolean"}}, "required": ["skip"]},
		"tasks": [
			{"id": "start", "switch": [{"case": "input.skip", "goto": "$tail"}, {"goto": "next"}]},
			{"id": "t", "output": {"count": "$: (self.previous.count ?? 0) + 1", "prev": "$: self.previous"},
			 "switch": [{"case": "self.output.count < 3", "goto": "$t"}, {"goto": "end"}]},
			{"id": "tail", "switch": "end"}
		],
		"output": {"v": "$: outputs.t ?? {count: 0}"}
	}`)
	for _, v := range []string{`{"v": {"count": 3, "prev": {"count": 2, "prev": {"count": 1, "prev": null}}}}`, `{"v": {"count": 0}}`} {
		var val any
		if err := json.Unmarshal([]byte(v), &val); err != nil {
			t.Fatal(err)
		}
		if _, err := sf.ProcessOutput.WithDefs(sf.Defs).Validate(val); err != nil {
			t.Errorf("the process outputs %s and its own published type refuses it: %v — a $process "+
				"caller conforms the child's output to that type", v, err)
		}
	}
}

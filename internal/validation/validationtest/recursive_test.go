package validationtest

import (
	"encoding/json"
	"strings"
	"testing"
)

// A self-referential output types by fixpoint, each case a whole process through Generate: a loop
// back creates the recursion, `?? <default>` the base case. specs/recursive-type-inference.md.

// loopingDef: task `id` outputs exprs and loops to itself. A sibling, if given, runs first, so its
// output is required where the task's own is not.
func loopingDef(id string, exprs map[string]string, sibling string) string {
	tasks := []any{}
	if sibling != "" {
		tasks = append(tasks, map[string]any{
			"id":     sibling,
			"output": map[string]any{"value": "$: input.v"},
			"switch": []any{map[string]any{"goto": "next"}},
		})
	}
	tasks = append(tasks, map[string]any{
		"id":     id,
		"output": exprs,
		"switch": []any{
			map[string]any{"case": "input.again", "goto": "$" + id},
			map[string]any{"goto": "end"},
		},
	})
	def := map[string]any{
		"name": "p",
		"input_schema": map[string]any{
			"type":       "object",
			"properties": map[string]any{"v": map[string]any{"type": "number"}, "again": map[string]any{"type": "boolean"}},
			"required":   []string{"v", "again"},
		},
		"tasks": tasks,
	}
	b, _ := json.Marshal(def)
	return string(b)
}

func TestRecursiveOutputTypesByFixpoint(t *testing.T) {
	// sobj builds the expected {type:object, required, properties} for a flat output type whose
	// fields are all primitives.
	sobj := func(props map[string]string, req ...string) string {
		p := make(map[string]any, len(props))
		for k, typ := range props {
			p[k] = map[string]any{"type": typ}
		}
		b, _ := json.Marshal(map[string]any{"type": "object", "properties": p, "required": req})
		return string(b)
	}

	tests := []struct {
		name    string
		id      string
		exprs   map[string]string
		sibling string
		want    string
	}{
		{
			name:  "counter via outputs.<self>",
			id:    "count",
			exprs: map[string]string{"n": "$: (outputs.count.n ?? 0) + 1"},
			want:  sobj(map[string]string{"n": "integer"}, "n"),
		},
		{
			name:  "counter via self.previous",
			id:    "count",
			exprs: map[string]string{"n": "$: (self.previous.n ?? 0) + 1"},
			want:  sobj(map[string]string{"n": "integer"}, "n"),
		},
		{
			name:  "string accumulator",
			id:    "cat",
			exprs: map[string]string{"s": `$: (outputs.cat.s ?? "") + "x"`},
			want:  sobj(map[string]string{"s": "string"}, "s"),
		},
		{
			name:  "boolean toggle via self.previous",
			id:    "tog",
			exprs: map[string]string{"f": "$: !(self.previous.f ?? false)"},
			want:  sobj(map[string]string{"f": "boolean"}, "f"),
		},
		{
			name:    "sum folding a sibling output",
			id:      "acc",
			exprs:   map[string]string{"total": "$: (outputs.acc.total ?? 0) + outputs.item.value"},
			sibling: "item",
			want:    sobj(map[string]string{"total": "number"}, "total"),
		},
		{
			name: "multiple fields mixing both self references",
			id:   "s",
			exprs: map[string]string{
				"n": "$: (outputs.s.n ?? 0) + 1",
				"f": "$: !(self.previous.f ?? false)",
			},
			want: sobj(map[string]string{"n": "integer", "f": "boolean"}, "f", "n"),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			out := runGenerate(t, loopingDef(tt.id, tt.exprs, tt.sibling))
			assertJSON(t, defOf(out, tt.id+"_output"), tt.want)
		})
	}
}

// Refused as a NULLABILITY error, before the fixpoint runs — what an author meets. The solver's
// own "no base case" refusal is pinned in schematest/solver_test.go.
func TestRecursiveOutputWithNoBaseCaseIsRefused(t *testing.T) {
	err := runGenerateErr(t, loopingDef("c", map[string]string{"n": "$: outputs.c.n + 1"}, ""))
	if err == nil {
		t.Fatal("a recursion with no base case must be refused, not typed")
	}
	if !strings.Contains(err.Error(), "non-nullable operands") {
		t.Errorf("the refusal must point at the missing default; got: %v", err)
	}
}

// An accumulation with no base case cannot converge at all: each round adds a layer, so the
// estimate grows without bound and the size cap is what stops it. The message names the cause.
func TestRecursiveOutputThatGrowsIsRefused(t *testing.T) {
	err := runGenerateErr(t, loopingDef("c", map[string]string{"n": "$: [outputs.c.n]"}, ""))
	if err == nil {
		t.Fatal("an unbounded recursion must be refused, not typed")
	}
	if !strings.Contains(err.Error(), "without converging") {
		t.Errorf("the refusal must name the non-convergence; got: %v", err)
	}
}

package validationtest

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"genroc/internal/model"
	"genroc/internal/validation"
)

// The task scope, pinned slot by slot. `self` has three members and they come into existence
// at different moments — previous when the task is entered, result when the action answers,
// output when the output map has run — so each slot may name only the ones that already
// exist. outputs.<own id> is previous everywhere, the switch included. specs/task-scopes.md.

// loopingTask wraps a slot fragment in a task that gotos itself, so `previous` exists.
func loopingTask(taskJSON string) string {
	return fmt.Sprintf(`{"name":"p","tasks":[%s]}`, taskJSON)
}

// preOutputCases are the slots evaluated before the task's own output exists — every one of
// them, so a slot that quietly acquires a scope it should not have fails here. `form` says how
// the expression under test is spelled in that slot: a $: leaf, a ${ } interpolation, or the
// bare boolean an on_error case takes.
type slotForm int

const (
	leaf     slotForm = iota // "$: expr"
	interp                   // "${ expr }"
	bareExpr                 // "expr"
)

// loop is the tail every case shares: an output to have a previous, and a switch that returns
// to the task so that previous exists.
const loop = `"output":{"n":"$: 1","text":"$: 'x'","list":"$: [1]"},` +
	`"switch":[{"case":"self.output.n < 2","goto":"$t"},{"goto":"end"}]}`

var preOutputCases = map[string]struct {
	tmpl string
	form slotForm
	// ok wraps the member under test so it satisfies the slot's own type rule — a number for
	// a delay, a boolean for a case, an array for `over`. %[1]s is the base (self.previous or
	// outputs.t). Without it the positive cases would fail on nullability and prove nothing
	// about scope.
	ok string
}{
	"input":          {`{"id":"t","action":{"type":"external","input":{"v":"%s"}},` + loop, leaf, "%[1]s.n"},
	"body":           {`{"id":"t","action":{"type":"fetch","url":"http://x","body":{"v":"%s"}},` + loop, leaf, "%[1]s.n"},
	"url":            {`{"id":"t","action":{"type":"fetch","url":"http://x/%s"},` + loop, interp, "%[1]s.n ?? 0"},
	"method":         {`{"id":"t","action":{"type":"fetch","url":"http://x","method":"%s"},` + loop, interp, "%[1]s.text ?? 'GET'"},
	"headers":        {`{"id":"t","action":{"type":"fetch","url":"http://x","headers":{"h":"%s"}},` + loop, interp, "%[1]s.n ?? 0"},
	"query":          {`{"id":"t","action":{"type":"fetch","url":"http://x","query":{"q":"%s"}},` + loop, interp, "%[1]s.n ?? 0"},
	"acceptedStatus": {`{"id":"t","action":{"type":"fetch","url":"http://x","accepted_status":["%s"]},` + loop, interp, "%[1]s.text ?? '2xx'"},
	"over":           {`{"id":"t","action":{"type":"child_list","name":"p","over":"%s"},` + loop, leaf, "%[1]s.list ?? []"},
	"delay for":      {`{"id":"t","action":{"type":"delay","for":"%s"},` + loop, leaf, "%[1]s.n ?? 0"},
	"delay until":    {`{"id":"t","action":{"type":"delay","until":"%s"},` + loop, leaf, "%[1]s.n ?? 0"},
	"timeout":        {`{"id":"t","action":{"type":"external"},"timeout":"%s",` + loop, leaf, "%[1]s.n ?? 0"},
	"on_error retry": {`{"id":"t","action":{"type":"fetch","url":"http://x"},
	                     "on_error":[{"code":["http.500"],"retry":{"attempts":"%s"},"goto":"end"}],` + loop, leaf, "%[1]s.n ?? 0"},
	"on_error case": {`{"id":"t","action":{"type":"fetch","url":"http://x"},
	                    "on_error":[{"code":["http.500"],"case":"%s","goto":"end"},{"code":[],"goto":"end"}],` + loop, bareExpr, "(%[1]s.n ?? 0) > 0"},
	"on_error raise message": {`{"id":"t","action":{"type":"fetch","url":"http://x"},
	                            "on_error":[{"code":["http.500"],"raise":{"code":"boom","message":"%s"}}],` + loop, interp, "%[1]s.n ?? 0"},
	"on_error raise data": {`{"id":"t","action":{"type":"fetch","url":"http://x"},
	                         "on_error":[{"code":["http.500"],"raise":{"code":"boom","message":"m","data":{"v":"%s"}}}],` + loop, leaf, "%[1]s.n"},
}

// spell renders expr the way the slot's form requires. A $: leaf inside a ${ } is a parse
// error, not a scope error, so getting this wrong would pass the test for the wrong reason.
func spell(form slotForm, expr string) string {
	switch form {
	case interp:
		return "${ " + expr + " }"
	case bareExpr:
		return expr
	default:
		return "$: " + expr
	}
}

// eachPreOutputSlotRejects asserts that naming member in every pre-output slot is refused.
// The scope guard runs ahead of the per-slot type checks, so no type wrapper is needed.
func eachPreOutputSlotRejects(t *testing.T, member string, fn func(t *testing.T, slot string, err error)) {
	t.Helper()
	for slot, c := range preOutputCases {
		t.Run(slot, func(t *testing.T) {
			fn(t, slot, runGenerateErr(t, loopingTask(fmt.Sprintf(c.tmpl, spell(c.form, member)))))
		})
	}
}

// eachPreOutputSlotAccepts asserts that reading base is valid in every pre-output slot, with
// each slot's own type rule satisfied.
func eachPreOutputSlotAccepts(t *testing.T, base string) {
	t.Helper()
	for slot, c := range preOutputCases {
		t.Run(slot, func(t *testing.T) {
			expr := fmt.Sprintf(c.ok, base)
			if err := runGenerateErr(t, loopingTask(fmt.Sprintf(c.tmpl, spell(c.form, expr)))); err != nil {
				t.Errorf("%s must be readable in %s: %v", base, slot, err)
			}
		})
	}
}

// A pre-output slot cannot name self.result: the action has not answered where the slot is
// evaluated, and where it has (an on_error rule) it answered with a failure.
func TestScopes_SelfResultRejectedBeforeTheOutput(t *testing.T) {
	eachPreOutputSlotRejects(t, "self.result.x", func(t *testing.T, slot string, err error) {
		if err == nil {
			t.Fatalf("self.result in %s must be rejected: the action has not answered there", slot)
		}
		if !strings.Contains(err.Error(), "self.result is not available here") {
			t.Errorf("message must give the rule, not a bare field lookup; got: %v", err)
		}
	})
}

// self.output exists only in the switch — the output map is what produces it, so not even
// that map can read it.
func TestScopes_SelfOutputOnlyInTheSwitch(t *testing.T) {
	eachPreOutputSlotRejects(t, "self.output.n", func(t *testing.T, slot string, err error) {
		if err == nil {
			t.Fatalf("self.output in %s must be rejected: the output map has not run there", slot)
		}
		if !strings.Contains(err.Error(), "self.output is not available here") {
			t.Errorf("message must give the rule, not a bare field lookup; got: %v", err)
		}
	})

	t.Run("output map", func(t *testing.T) {
		err := runGenerateErr(t, `{"name":"p","tasks":[
			{"id":"t","action":{"type":"delay","for":"1s"},"output":{"n":"$: self.output.n"},
			 "switch":[{"case":"self.output.n < 2","goto":"$t"},{"goto":"end"}]}
		]}`)
		if err == nil {
			t.Fatal("the output map cannot read the output it is producing")
		}
		if !strings.Contains(err.Error(), "self.output is not available here") {
			t.Errorf("message must give the rule; got: %v", err)
		}
	})

	t.Run("switch accepts it", func(t *testing.T) {
		if err := runGenerateErr(t, `{"name":"p","tasks":[
			{"id":"t","action":{"type":"delay","for":"1s"},"output":{"n":"$: 1"},
			 "switch":[{"case":"self.output.n < 2","goto":"$t"},{"goto":"end"}]}
		]}`); err != nil {
			t.Errorf("the switch is where self.output exists: %v", err)
		}
	})
}

// self.previous is readable in every slot of a looping task — including the ones that build
// the call, which is the whole point: a loop feeds its last output into its next attempt.
func TestScopes_SelfPreviousReadableInEverySlot(t *testing.T) {
	eachPreOutputSlotAccepts(t, "self.previous")
}

// outputs.<own id> is the SAME value as self.previous, in every slot. The two names must
// therefore be accepted and rejected together — a slot where one works and the other does
// not is the ambiguity this rule exists to remove.
func TestScopes_OwnOutputsIsPrevious(t *testing.T) {
	eachPreOutputSlotAccepts(t, "outputs.t")

	t.Run("switch", func(t *testing.T) {
		if err := runGenerateErr(t, `{"name":"p","tasks":[
			{"id":"t","action":{"type":"delay","for":"1s"},"output":{"n":"$: (self.previous.n ?? 0) + 1"},
			 "switch":[{"case":"(outputs.t.n ?? 0) < 2","goto":"$t"},{"goto":"end"}]}
		]}`); err != nil {
			t.Errorf("outputs.t in the switch is the previous output, not an error: %v", err)
		}
	})
}

// Without a path back to the task there is never a previous output, so both names for it are
// refused — and the message says why rather than reading as a typo. A task that DOES loop but
// declares no output fails for the other reason, and must not be told it never loops.
func TestScopes_NoPreviousWithoutALoop(t *testing.T) {
	nonLooping := map[string]string{
		"self.previous in input":  `{"id":"t","action":{"type":"external","input":{"v":"$: self.previous.n"}},"output":{"n":"$: 1"},"switch":"end"}`,
		"outputs.t in input":      `{"id":"t","action":{"type":"external","input":{"v":"$: outputs.t.n"}},"output":{"n":"$: 1"},"switch":"end"}`,
		"self.previous in output": `{"id":"t","action":{"type":"delay","for":"1s"},"output":{"n":"$: self.previous.n"},"switch":"end"}`,
		"outputs.t in output":     `{"id":"t","action":{"type":"delay","for":"1s"},"output":{"n":"$: outputs.t.n"},"switch":"end"}`,
		"outputs.t in switch":     `{"id":"t","action":{"type":"delay","for":"1s"},"output":{"n":"$: 1"},"switch":[{"case":"outputs.t.n > 0","goto":"end"},{"goto":"end"}]}`,
	}
	t.Run("loops but declares no output", func(t *testing.T) {
		err := runGenerateErr(t, `{"name":"p","input_schema":{"type":"object","properties":{"n":{"type":"integer"}}},"tasks":[
			{"id":"t","action":{"type":"delay","for":"$: self.previous.n ?? 10"},
			 "switch":[{"case":"input.n > 0","goto":"$t"},{"goto":"end"}]}
		]}`)
		if err == nil {
			t.Fatal("a task with no output has no previous output either")
		}
		if !strings.Contains(err.Error(), "declares no output") {
			t.Errorf("the task does loop; the message must blame the missing output: %v", err)
		}
	})

	for name, taskJSON := range nonLooping {
		t.Run(name, func(t *testing.T) {
			err := runGenerateErr(t, loopingTask(taskJSON))
			if err == nil {
				t.Fatal("a task nothing returns to has no previous output")
			}
			if !strings.Contains(err.Error(), "no path returns to task") {
				t.Errorf("message must say the task never loops; got: %v", err)
			}
		})
	}
}

// The pre-output scope must reach a CHILD task's input, which a second pass re-checks against
// the child's declared input_schema. That pass rebuilds the context from scratch, so it is
// where an uninferred placeholder silently replaces a task's real output type.
func TestScopes_ChildInputSeesInferredOutputTypes(t *testing.T) {
	child := childDef(t, "kid", `{"type":"object","properties":{"x":{"type":"integer"}},"required":["x"]}`)
	assertChildRefsOK(t, child, `{"name":"parent","tasks":[
		{"id":"a","action":{"type":"delay","for":"1s"},"output":{"n":"$: 1"},"switch":"next"},
		{"id":"b","action":{"type":"child","name":"kid","input":{"x":"$: outputs.a.n"}},"switch":"end"}
	]}`)
}

// The same pass must see self.previous, whose type resolves through the very $defs entry a
// placeholder would have shadowed.
func TestScopes_ChildInputSeesSelfPrevious(t *testing.T) {
	child := childDef(t, "kid", `{"type":"object","properties":{"x":{"type":"integer"}},"required":["x"]}`)
	assertChildRefsOK(t, child, `{"name":"parent","tasks":[
		{"id":"a","action":{"type":"child","name":"kid","input":{"x":"$: (self.previous.n ?? 0) + 1"}},
		 "output":{"n":"$: 1"},"switch":[{"case":"self.output.n < 2","goto":"$a"},{"goto":"end"}]}
	]}`)
}

// assertChildRefsOK runs the pair a registration runs: Generate, then the child-reference pass.
func assertChildRefsOK(t *testing.T, child *model.ProcessDefinition, defJSON string) {
	t.Helper()
	var def model.ProcessDefinition
	if err := json.Unmarshal([]byte(defJSON), &def); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if _, err := validation.Generate(&def); err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if err := validation.ValidateChildProcessRefs(&def, 1, stubGetter{child.Name: child}); err != nil {
		t.Errorf("the child pass must see the inferred output types, not placeholders: %v", err)
	}
}

// A migration layer is the shape of the stored ROW, and `self` is never in it: previous,
// result and output are transient scopes the engine builds per slot. If contextSchema ever
// grew a `self`, MigrateState would try to conform a value no row carries — and compat would
// report a difference in something that is not stored. specs/version-compatibility.md.
func TestScopes_MigrationLayersCarryNoSelf(t *testing.T) {
	var def model.ProcessDefinition
	if err := json.Unmarshal([]byte(`{"name":"p","tasks":[
		{"id":"t","action":{"type":"external","input":{"v":"$: (self.previous.n ?? 0) + 1"}},
		 "output":{"n":"$: 1"},"switch":[{"case":"self.output.n < 2","goto":"$t"},{"goto":"end"}]}
	]}`), &def); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	layers, err := validation.TaskContexts(&def)
	if err != nil {
		t.Fatalf("TaskContexts: %v", err)
	}
	for task, layer := range layers {
		if _, ok := layer.Properties()["self"]; ok {
			t.Errorf("task %q migration layer carries `self`; it is transient and no row holds it", task)
		}
		// The row's real slots must still be there, so the assertion above cannot pass by
		// the layer being empty.
		if _, ok := layer.Properties()["outputs"]; !ok {
			t.Errorf("task %q migration layer lost `outputs`", task)
		}
	}
}

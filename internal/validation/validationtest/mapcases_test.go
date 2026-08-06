package validationtest

import (
	"encoding/json"
	"strings"
	"testing"

	"genroc/internal/model"
	"genroc/internal/validation"
)

// Fixtures and builders for map_test.go and infer_test.go, which are both about which
// definition SHAPE is accepted, rejected or inferred — so each test stays short enough to read
// as "build this shape, expect this". Nothing here asserts on its own.

// --- map fixtures -----------------------------------------------------------

// mapRowsInput is the process input reused across the map tests: an array of
// rows with a string code and an integer count. Reshaping it with map + an
// object literal is the motivating use case for the feature
// (specs/map-expressions.md).
const mapRowsInput = `{
	"type": "object",
	"properties": {
		"rows": {
			"type": "array",
			"items": {
				"type": "object",
				"properties": {"code": {"type": "string"}, "count": {"type": "integer"}},
				"required": ["code", "count"]
			}
		}
	},
	"required": ["rows"]
}`

// mapNullableRowsInput is mapRowsInput's optional twin: `rows` is not required,
// so `input.rows` is nullable and a bare map over it has no non-null source.
const mapNullableRowsInput = `{
	"type": "object",
	"properties": {
		"rows": {
			"type": "array",
			"items": {"type": "object", "properties": {"code": {"type": "string"}}, "required": ["code"]}
		}
	}
}`

// mapPrepareTask is an inert first task, for the error-attribution tests where
// the *second* task must be named as the broken one.
const mapPrepareTask = `{"id": "prepare", "action": {"type": "fetch", "url": "http://x"}, "switch": "next"}`

// mapNoopTask is an inert only-task, for definitions whose subject is the
// process-level output rather than anything a task does.
const mapNoopTask = `{"id": "noop", "action": {"type": "fetch", "url": "http://x"}, "switch": "end"}`

// --- map definition builders ------------------------------------------------

// mapDef assembles a definition from its name, input schema and task list.
func mapDef(name, inputSchema, tasksJSON string) string {
	return `{
		"name": "` + name + `",
		"input_schema": ` + inputSchema + `,
		"tasks": [` + tasksJSON + `]
	}`
}

// mapRowsDef is mapDef over the standard mapRowsInput.
func mapRowsDef(name, tasksJSON string) string {
	return mapDef(name, mapRowsInput, tasksJSON)
}

// mapFetchTask builds a single fetch task with the given id, from the raw
// action JSON — the part a fetch test is actually about.
func mapFetchTask(id, actionJSON string) string {
	return `{"id": "` + id + `", "action": ` + actionJSON + `, "switch": "end"}`
}

// mapRowsFetchDef is the one-fetch-task-over-mapRowsInput shape: a task "push"
// whose action is under test.
func mapRowsFetchDef(name, actionJSON string) string {
	return mapRowsDef(name, mapFetchTask("push", actionJSON))
}

// mapChildListTask builds a child_list task fanning out over `over`.
func mapChildListTask(id, childName, over string) string {
	return `{
		"id": "` + id + `",
		"action": {"type": "child_list", "name": "` + childName + `", "over": "` + over + `"},
		"switch": "end"
	}`
}

// mapChildListEchoTask is mapChildListTask with the same expression exported as
// the task output, so a test can inspect the element type `over` produced.
func mapChildListEchoTask(id, childName, over string) string {
	return `{
		"id": "` + id + `",
		"action": {"type": "child_list", "name": "` + childName + `", "over": "` + over + `"},
		"switch": "end",
		"output": "` + over + `"
	}`
}

// mapFanoutDef is a child_list whose `over` reshapes the process input with map.
// Parameterised on the child name so the same definition can be pointed at
// compatible and incompatible children.
func mapFanoutDef(childName string) string {
	return mapRowsDef("map-fanout",
		mapChildListTask("fanout", childName, "$: map(input.rows, r => {sku: r.code, qty: r.count + 1})"))
}

// mapChildMapDef builds a definition whose single task spawns one child_map
// entry ("worker") with the given input shape.
func mapChildMapDef(name, childName, inputJSON string) string {
	return mapRowsDef(name, `{
		"id": "batch",
		"action": {
			"type": "child_map",
			"children": {"worker": {"name": "`+childName+`", "input": `+inputJSON+`}}
		},
		"switch": "end"
	}`)
}

// mapProcessOutputDef builds a definition with a single inert task whose only
// interesting part is the process-level output map.
func mapProcessOutputDef(name, inputSchema, outputJSON string) string {
	return `{
		"name": "` + name + `",
		"input_schema": ` + inputSchema + `,
		"tasks": [` + mapNoopTask + `],
		"output": ` + outputJSON + `
	}`
}

// --- map assertions ---------------------------------------------------------

// mapGenerateErr runs Generate, fails the test if it succeeded, and returns the
// error text so the caller can pin what the message must tell the author.
func mapGenerateErr(t *testing.T, defJSON, why string) string {
	t.Helper()
	err := runGenerateErr(t, defJSON)
	if err == nil {
		t.Fatalf("expected an error for %s, got nil", why)
	}
	return err.Error()
}

// mapErrMentions asserts the error text contains want; `should` completes the
// sentence "error should ...".
func mapErrMentions(t *testing.T, got, want, should string) {
	t.Helper()
	if !strings.Contains(got, want) {
		t.Errorf("error should %s, got: %s", should, got)
	}
}

// mapChildRefs runs the full registration pipeline — Generate (which must
// succeed) followed by ValidateChildProcessRefs — and returns the child-ref
// error. The child input type derived from a `map` is only observable through
// the second phase, which subset-checks it against the child's input_schema.
func mapChildRefs(t *testing.T, defJSON string, getter validation.DefinitionGetter) error {
	t.Helper()
	var def model.ProcessDefinition
	if err := json.Unmarshal([]byte(defJSON), &def); err != nil {
		t.Fatalf("unmarshal definition: %v", err)
	}
	if _, err := validation.Generate(&def); err != nil {
		t.Fatalf("Generate: %v", err)
	}
	return validation.ValidateChildProcessRefs(&def, 1, getter)
}

// assertMapChildRefsOK requires the mapped input to satisfy the child's
// input_schema; `subject` names what should have been accepted.
func assertMapChildRefsOK(t *testing.T, defJSON string, getter validation.DefinitionGetter, subject string) {
	t.Helper()
	if err := mapChildRefs(t, defJSON, getter); err != nil {
		t.Errorf("%s should satisfy the child input_schema: %v", subject, err)
	}
}

// assertMapChildRefsIncompatible requires the mismatch to be reported; `why`
// records the mismatch the case sets up.
func assertMapChildRefsIncompatible(t *testing.T, defJSON string, getter validation.DefinitionGetter, why string) {
	t.Helper()
	err := mapChildRefs(t, defJSON, getter)
	if err == nil || !strings.Contains(err.Error(), "not compatible") {
		t.Fatalf("err = %v, want an incompatibility error (%s)", err, why)
	}
}

// --- inference definition builders ------------------------------------------

// Raw JSON fragments the inference tests reuse verbatim.
const (
	// inferSelfResult exports the raw action result as the task output.
	inferSelfResult = `"$: self.result"`
	inferNext       = `"next"`
	inferEnd        = `"end"`
)

// inferTask describes one task of a definition under inference. Only the fields
// a test cares about are set; the rest are omitted from the JSON, so the call
// site shows exactly what the case is made of. All string fields are raw JSON.
type inferTask struct {
	id      string
	fetch   bool   // give the task a fetch action against http://x
	result  string // fetch result_schema (implies an action)
	body    string // fetch body (implies an action)
	output  string // output template or output map
	sw      string // switch; omitted when empty (falls through to the next task)
	onError string // on_error
}

func (tk inferTask) json() string {
	parts := []string{`"id":"` + tk.id + `"`}
	if tk.fetch || tk.result != "" || tk.body != "" {
		action := `{"type":"fetch","url":"http://x"`
		if tk.result != "" {
			action += `,"result_schema":` + tk.result
		}
		if tk.body != "" {
			action += `,"body":` + tk.body
		}
		parts = append(parts, `"action":`+action+`}`)
	}
	if tk.onError != "" {
		parts = append(parts, `"on_error":`+tk.onError)
	}
	if tk.sw != "" {
		parts = append(parts, `"switch":`+tk.sw)
	}
	if tk.output != "" {
		parts = append(parts, `"output":`+tk.output)
	}
	return "{" + strings.Join(parts, ",") + "}"
}

// inferDef wraps tasks into a process definition named "p". Pass "" for no
// input schema.
func inferDef(inputSchema string, tasks ...inferTask) string {
	return inferDefWithOutput(inputSchema, "", tasks...)
}

// inferDefWithOutput is inferDef plus a process-level output map ("" for none).
func inferDefWithOutput(inputSchema, outputJSON string, tasks ...inferTask) string {
	def := `{"name":"p"`
	if inputSchema != "" {
		def += `,"input_schema":` + inputSchema
	}
	encoded := make([]string, len(tasks))
	for i, tk := range tasks {
		encoded[i] = tk.json()
	}
	def += `,"tasks":[` + strings.Join(encoded, ",") + `]`
	if outputJSON != "" {
		def += `,"output":` + outputJSON
	}
	return def + `}`
}

// inferObjSchema builds an object schema from a property list and the names of
// the required properties.
func inferObjSchema(propsJSON string, required ...string) string {
	s := `{"type":"object","properties":{` + propsJSON + `}`
	if len(required) > 0 {
		s += `,"required":["` + strings.Join(required, `","`) + `"]`
	}
	return s + `}`
}

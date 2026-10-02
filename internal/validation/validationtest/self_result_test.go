package validationtest

import (
	"strings"
	"testing"
)

// The message must name the slot that would FIX it: a fetch is sent to `responses`, since it
// refuses result_schema.
func TestGenerate_OutputOfUntypedResult_Errors(t *testing.T) {
	// Bare self.result in an output, no result_schema → error mentioning result_schema.
	err := runGenerateErr(t, `{
		"name": "p",
		"tasks": [
			{ "id": "call", "action": { "type": "fetch", "method": "post", "url": "http://x" }, "output": "$: self.result", "switch": "end" }
		]
	}`)
	if err == nil {
		t.Fatal("expected an error exporting self.result without a result_schema")
	}
	if !strings.Contains(err.Error(), "responses") {
		t.Errorf("error should point a fetch at the missing responses, got: %v", err)
	}

	// A member access under an output map is the same error.
	if err := runGenerateErr(t, `{"name":"p","tasks":[
		{"id":"call","action":{"type":"fetch","method":"post","url":"http://x"},"output":{"v":"$: self.result.x"},"switch":"end"}
	]}`); err == nil {
		t.Error("expected an error exporting self.result.x without a result_schema")
	}

	// With a declared status the output is well-typed and accepted.
	if err := runGenerateErr(t, `{"name":"p","tasks":[
		{"id":"call","action":{"type":"fetch","method":"post","url":"http://x","responses": { "200": {"type":"object","properties":{"ok":{"type":"boolean"}}} }},"output":"$: self.result","switch":"end"}
	]}`); err != nil {
		t.Errorf("exporting self.result with a declared status should be valid: %v", err)
	}

	// Routing on self.result in a switch without a result_schema is ALSO an error: an untyped
	// result does not exist in the context — there is no transient/raw-value routing.
	if err := runGenerateErr(t, `{"name":"p","tasks":[
		{"id":"call","action":{"type":"fetch","method":"post","url":"http://x"},"switch":[{"case":"self.result == null","goto":"end"}]}
	]}`); err == nil {
		t.Error("expected an error routing on self.result in a switch without a result_schema")
	}
}

// For an external task, result_schema is what makes the submitted result readable.
func TestGenerate_ExternalUntypedResult_Errors(t *testing.T) {
	// Output export of the raw result → error.
	if err := runGenerateErr(t, `{"name":"p","tasks":[
		{"id":"wait","action":{"type":"external"},"output":"$: self.result","switch":"end"}
	]}`); err == nil {
		t.Error("expected an error exporting an external self.result without a result_schema")
	}

	// Routing on the raw result in a switch → error.
	if err := runGenerateErr(t, `{"name":"p","tasks":[
		{"id":"wait","action":{"type":"external"},"switch":[{"case":"self.result == null","goto":"end"}]}
	]}`); err == nil {
		t.Error("expected an error routing on an external self.result in a switch without a result_schema")
	}

	// With a result_schema the result is well-typed and accessible in both.
	if err := runGenerateErr(t, `{"name":"p","tasks":[
		{"id":"wait","action":{"type":"external","result_schema":{"type":"object","properties":{"ok":{"type":"boolean"}},"required":["ok"]}},
		 "output":"$: self.result","switch":[{"case":"self.result.ok","goto":"end"},{"goto":"end"}]}
	]}`); err != nil {
		t.Errorf("external self.result with a result_schema should be accepted: %v", err)
	}
}

// The last task's output is reachable only under its id. docs reference/definition/expressions.mdx.
func TestGenerate_ProcessOutputHasNoSelf(t *testing.T) {
	const task = `{"id":"call","action":{"type":"fetch","method":"post","url":"http://x",
		"responses":{"200":{"type":"object","properties":{"ok":{"type":"boolean"}}}}},
		"output":"$: self.result","switch":"end"}`
	def := func(procOutput string) string {
		return `{"name":"p","tasks":[` + task + `],"output":"$: ` + procOutput + `"}`
	}
	for _, member := range []string{"self.previous", "self.result", "self.output"} {
		err := runGenerateErr(t, def(member))
		if err == nil {
			t.Errorf("a process-level output naming %s was accepted; it is not a task slot and has no self", member)
		} else if !strings.Contains(err.Error(), `field "self" not found`) {
			t.Errorf("%s must be refused for having no self, got: %v", member, err)
		}
	}
	if err := runGenerateErr(t, def("outputs.call")); err != nil {
		t.Errorf("a process-level output must still reach a task's output under its id: %v", err)
	}
}

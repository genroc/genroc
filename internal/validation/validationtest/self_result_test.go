package validationtest

import (
	"strings"
	"testing"
)

// A fetch with no `responses` has no self.result at all, so referencing it -- in an output or a
// switch -- is a "not in schema" error. The message must name the slot that would FIX it, which
// differs by action type: a fetch is sent to `responses`, and result_schema is a field it refuses.
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

// The same rule for an external task: with no result_schema the submitted result is untyped
// and does not exist in the context — referencing it in an output OR a switch is an error;
// declaring a result_schema types it and makes it accessible.
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

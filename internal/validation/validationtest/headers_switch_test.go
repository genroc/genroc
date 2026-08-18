package validationtest

import (
	"strings"
	"testing"
)

// A switch case that reads self.result on an untyped action now gets the same actionable
// message an output does, instead of a raw "schema has no properties" navigation error.
func TestGenerate_SwitchCase_UntypedSelfResult(t *testing.T) {
	untyped := `{"name":"p","tasks":[
		{"id":"call","action":{"type":"fetch","url":"http://x"},
		 "switch":[{"case":"self.result.done == true","goto":"end"},{"goto":"end"}]}]}`
	if err := runGenerateErr(t, untyped); err == nil || !strings.Contains(err.Error(), "responses") {
		t.Errorf("untyped self.result in case = %v; want the message naming the slot that would type it", err)
	}

	// With a declared status, self.result is typed and readable in the case.
	typed := `{"name":"p2","tasks":[
		{"id":"call","action":{"type":"fetch","url":"http://x","responses": { "200": {"type":"object","properties":{"done":{"type":"boolean"}},"required":["done"]} }},
		 "switch":[{"case":"self.result.done == true","goto":"end"},{"goto":"end"}]}]}`
	if err := runGenerateErr(t, typed); err != nil {
		t.Errorf("typed self.result in case should pass: %v", err)
	}
}

// Headers must be an object whose values are all strings (HTTP header values); a
// non-string value is rejected at registration rather than silently coerced at runtime.
func TestGenerate_Headers_MustBeStringValued(t *testing.T) {
	input := `{"type":"object","properties":{"n":{"type":"integer"},"tok":{"type":"string"}},"required":["n","tok"]}`

	ok := `{"name":"h1","input_schema":` + input + `,"tasks":[
		{"id":"call","action":{"type":"fetch","url":"http://x","headers":{"Authorization":"Bearer ${ input.tok }"}},"switch":"end"}]}`
	if err := runGenerateErr(t, ok); err != nil {
		t.Errorf("string-valued headers should pass: %v", err)
	}

	bad := `{"name":"h2","input_schema":` + input + `,"tasks":[
		{"id":"call","action":{"type":"fetch","url":"http://x","headers":{"X-Count":"$: input.n"}},"switch":"end"}]}`
	if err := runGenerateErr(t, bad); err == nil || !strings.Contains(err.Error(), "values must all be strings") {
		t.Errorf("integer header value = %v; want 'values must all be strings'", err)
	}
}

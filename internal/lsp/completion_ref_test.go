package lsp

import (
	"slices"
	"strings"
	"testing"

	"genroc/internal/schema"
)

// A task whose `output` is a bare nullable expression — `$: self.result[0]` — publishes an
// output def that is itself `null | object`. Downstream, `outputs.<task>` is a `$ref` AT that
// nullable, not a `$ref` beside a null. A plain StripNull leaves such a ref untouched, so the
// resolve below lands on the nullable and the object offers no members at all.
func TestMembersThroughARefToANullableObject(t *testing.T) {
	raw, err := schema.Parse([]byte(`{
		"$defs": {"a_output": {"oneOf": [
			{"type": "null"},
			{"type": "object", "properties": {"email": {"type": "string"}, "activated": {"type": "boolean"}}, "required": ["email", "activated"]}
		]}},
		"properties": {"outputs": {"type": "object", "properties": {"a": {"$ref": "#/$defs/a_output"}}, "required": ["a"]}},
		"required": ["outputs"]
	}`))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	at, err := raw.AssumeNormalized().At("outputs.a")
	if err != nil {
		t.Fatalf("At(outputs.a): %v", err)
	}
	got := labels(membersOf(at))
	slices.Sort(got)
	if !slices.Equal(got, []string{"activated", "email"}) {
		t.Errorf("completing into a nullable task output offered %v, want its members", got)
	}
}

// The hover an author reads must agree with the checker that just accepted the definition.
// Reaching case 1 means case 0's null check was false, so `self.output.activated` is a plain
// boolean there — showing `boolean|null` is the editor contradicting registration.
const guardedSwitchDoc = `name: demo
tasks:
  - id: load_user
    action:
      type: fetch
      method: get
      url: "http://x"
      responses:
        200:
          type: array
          items:
            type: object
            properties: { activated: { type: boolean } }
            required: [activated]
    output: "$: self.result[0]"
    switch:
      - case: "self.output == null"
        panic: { code: no_results, message: "none" }
      - case: "self.output.activated"
        goto: end
      - goto: end
`

func TestHoverInAGuardedSwitchCaseAgreesWithTheChecker(t *testing.T) {
	// line 19 is `      - case: "self.output.activated"`; column 32 is inside `activated`.
	got := hoverOf(t, guardedSwitchDoc, 19, 32)
	if strings.Contains(got, "null") {
		t.Errorf("hover said %q; case 0 proved the output is there, so this is a boolean", got)
	}
	if !strings.Contains(got, "boolean") {
		t.Errorf("hover said %q, want it to name the boolean", got)
	}
}

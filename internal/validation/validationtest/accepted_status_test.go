package validationtest

import (
	"strings"
	"testing"
)

// accepted_status is a shape that must evaluate to an array of strings. The structural
// array<string> check is enforced at registration (checkAcceptedStatusShape); the per-pattern
// format ("2xx"/"404") is intentionally NOT checked — an expression's elements aren't known
// statically, and an unrecognized pattern simply never matches at runtime.

func acceptedStatusFetchDef(acceptedStatus string) string {
	return `{
		"name": "p",
		"tasks": [
			{
				"id": "call",
				"action": {"type": "fetch", "method": "post", "url": "http://x", "accepted_status": ` + acceptedStatus + `},
				"switch": "end"
			}
		]
	}`
}

func wantErrContains(t *testing.T, err error, want string) {
	t.Helper()
	if err == nil {
		t.Fatalf("expected an error containing %q, got nil", want)
	}
	if !strings.Contains(err.Error(), want) {
		t.Fatalf("error %q does not contain %q", err.Error(), want)
	}
}

func TestAcceptedStatus_ValidStaticPatterns_OK(t *testing.T) {
	runGenerate(t, acceptedStatusFetchDef(`["2xx", "3xx", "4xx", "5xx", "200", "404"]`))
}

// A static literal is format-checked: an unrecognized pattern is rejected at registration
// (this is what stays possible because the value is known from the YAML, not an expression).
func TestAcceptedStatus_InvalidStaticPattern_Rejected(t *testing.T) {
	err := runGenerateErr(t, acceptedStatusFetchDef(`["2xx", "banana"]`))
	wantErrContains(t, err, `task "call" accepted_status "banana" must be`)
}

// A too-short code and an out-of-range hundred-band are caught too.
func TestAcceptedStatus_MalformedStaticCode_Rejected(t *testing.T) {
	wantErrContains(t, runGenerateErr(t, acceptedStatusFetchDef(`["2x"]`)), `accepted_status "2x" must be`)
	wantErrContains(t, runGenerateErr(t, acceptedStatusFetchDef(`["6xx"]`)), `accepted_status "6xx" must be`)
}

// A dynamic element (a ${ } interpolation) is NOT format-checked — its value is unknown
// until runtime — even though "1" is not a valid literal pattern. Only static leaves are.
func TestAcceptedStatus_DynamicElement_NotFormatChecked(t *testing.T) {
	runGenerate(t, acceptedStatusFetchDef(`["2xx", "${ 1 }"]`))
}

func TestAcceptedStatus_ExpressionYieldingStringArray_OK(t *testing.T) {
	runGenerate(t, `{
		"name": "p",
		"input_schema": {"type": "object", "properties": {"codes": {"type": "array", "items": {"type": "string"}}}, "required": ["codes"]},
		"tasks": [
			{"id": "call", "action": {"type": "fetch", "method": "post", "url": "http://x", "accepted_status": "$: input.codes"}, "switch": "end"}
		]
	}`)
}

// A bare value that is not an array (here a template yielding a string) is rejected — it must
// be an array of strings.
func TestAcceptedStatus_NonArray_Rejected(t *testing.T) {
	err := runGenerateErr(t, acceptedStatusFetchDef(`"2xx"`))
	wantErrContains(t, err, `task "call" accepted_status must evaluate to an array of strings`)
}

// A literal array whose elements are not all strings is rejected on element type.
func TestAcceptedStatus_ArrayOfNumbers_Rejected(t *testing.T) {
	err := runGenerateErr(t, acceptedStatusFetchDef(`[404, 500]`))
	wantErrContains(t, err, `task "call" accepted_status values must all be strings`)
}

// An expression whose static type is an array of the wrong element type is rejected too —
// the structural check follows the inferred type, not just literals.
func TestAcceptedStatus_ExpressionYieldingNumberArray_Rejected(t *testing.T) {
	err := runGenerateErr(t, `{
		"name": "p",
		"input_schema": {"type": "object", "properties": {"codes": {"type": "array", "items": {"type": "integer"}}}, "required": ["codes"]},
		"tasks": [
			{"id": "call", "action": {"type": "fetch", "method": "post", "url": "http://x", "accepted_status": "$: input.codes"}, "switch": "end"}
		]
	}`)
	wantErrContains(t, err, `task "call" accepted_status`)
}

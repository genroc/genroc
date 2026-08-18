package validationtest

import (
	"encoding/json"
	"reflect"
	"testing"

	"genroc/internal/model"
	"genroc/internal/validation"
)

// The process: takes input {order_id, api_key(secret)}, one task "charge" whose
// output {receipt, cvv(secret)} is projected. This gives us input.*, outputs.charge.*
// to query through the composed context schema.
const chargeProcess = `{
	"name": "order",
	"input_schema": {
		"type":"object",
		"properties":{
			"order_id":{"type":"integer"},
			"api_key":{"type":"string","secret":true}
		},
		"required":["order_id","api_key"]
	},
	"tasks": [{
		"id":"charge",
		"action":{
			"type":"fetch",
			"url":"http://x",
			"responses": { "200": {
				"type":"object",
				"properties":{
					"receipt":{"type":"string"},
					"cvv":{"type":"string","secret":true}
				},
				"required":["receipt","cvv"]
			} }
		},
		"output":{
			"receipt":"'${ self.result.receipt }'",
			"cvv":"'${ self.result.cvv }'"
		}
	}]
}`

func TestContextSchema_ValidateAndSecretAtSubpaths(t *testing.T) {
	var def model.ProcessDefinition
	if err := json.Unmarshal([]byte(chargeProcess), &def); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	ctx, err := validation.ContextSchema(&def)
	if err != nil {
		t.Fatalf("ContextSchema: %v", err)
	}

	// Validate + normalize the process input at the "input" subpath. Undeclared
	// keys are stripped; the $ref into root $defs resolves through the sub-schema.
	got, err := ctx.ValidateAt("input", map[string]any{"order_id": 7, "api_key": "sk", "junk": true})
	if err != nil {
		t.Fatalf("ValidateAt(input): %v", err)
	}
	want := map[string]any{"order_id": 7, "api_key": "sk"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("ValidateAt(input) = %v, want %v", got, want)
	}

	// Missing required order_id at the subpath is rejected.
	if _, err := ctx.ValidateAt("input", map[string]any{"api_key": "sk"}); err == nil {
		t.Error("expected error for missing required order_id")
	}

	// Secrecy is answerable at any path in the composed context.
	secretCases := map[string]bool{
		"input.api_key":          true,
		"input.order_id":         false,
		"outputs.charge.cvv":     true,
		"outputs.charge.receipt": false,
	}
	for path, want := range secretCases {
		if got := ctx.SecretAt(path); got != want {
			t.Errorf("SecretAt(%q) = %v, want %v", path, got, want)
		}
	}
}

func TestContextSchema_RedactsWholeContext(t *testing.T) {
	var def model.ProcessDefinition
	if err := json.Unmarshal([]byte(chargeProcess), &def); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	ctx, err := validation.ContextSchema(&def)
	if err != nil {
		t.Fatalf("ContextSchema: %v", err)
	}

	contextData := map[string]any{
		"input":   map[string]any{"order_id": 7, "api_key": "sk-secret"},
		"outputs": map[string]any{"charge": map[string]any{"receipt": "R-1", "cvv": "123"}},
	}
	red := ctx.Redact(contextData).(map[string]any)

	in := red["input"].(map[string]any)
	if in["api_key"] != "***" || in["order_id"] != 7 {
		t.Errorf("input redaction wrong: %v", in)
	}
	charge := red["outputs"].(map[string]any)["charge"].(map[string]any)
	if charge["cvv"] != "***" || charge["receipt"] != "R-1" {
		t.Errorf("charge redaction wrong: %v", charge)
	}
}

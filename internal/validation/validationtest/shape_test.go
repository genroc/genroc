package validationtest

import "testing"

// A single-expression output passes the action result through unchanged: the
// inferred task output type equals the result (result_schema) type, with no
// object wrapper.
func TestGenerate_OutputSingleExpressionPassthrough(t *testing.T) {
	out := runGenerate(t, `{
		"name": "p",
		"tasks": [
			{
				"id": "charge",
				"action": {"type":"fetch","url":"http://x","responses": { "200": {
					"type":"object",
					"properties":{"charged":{"type":"boolean"}},
					"required":["charged"]
				} }},
				"output": "$: self.result",
				"switch": "end"
			}
		]
	}`)
	assertJSON(t, out.Tasks["charge"].Output, `{"$ref": "#/$defs/charge_output"}`)
	assertJSON(t, defOf(out, "charge_output"), `{
		"type":"object",
		"properties":{"charged":{"type":"boolean"}},
		"required":["charged"]
	}`)
}

// A nested-object output infers a nested object schema (all keys required).
func TestGenerate_OutputNestedObject(t *testing.T) {
	out := runGenerate(t, `{
		"name": "p",
		"tasks": [
			{
				"id": "charge",
				"action": {"type":"fetch","url":"http://x","responses": { "200": {
					"type":"object",
					"properties":{"charged":{"type":"boolean"}},
					"required":["charged"]
				} }},
				"output": {"data": {"flag": "$: self.result.charged"}},
				"switch": "end"
			}
		]
	}`)
	assertJSON(t, defOf(out, "charge_output"), `{
		"type":"object",
		"properties":{
			"data":{
				"type":"object",
				"properties":{"flag":{"type":"boolean"}},
				"required":["flag"]
			}
		},
		"required":["data"]
	}`)
}

// A single-expression process output may infer to a non-object type.
func TestGenerate_ProcessOutputSingleExpressionScalar(t *testing.T) {
	out := runGenerate(t, `{
  "name": "p",
  "tasks": [
    {
      "id": "charge",
      "action": {
        "type": "fetch",
        "url": "http://x",
        "responses": { "200": {
          "type": "object",
          "properties": {
            "charged": {
              "type": "boolean"
            }
          },
          "required": [
            "charged"
          ]
        } }
      },
      "switch": "end",
      "output": "$: self.result"
    }
  ],
  "output": "$: outputs.charge.charged"
}`)
	assertJSON(t, out.ProcessOutput, `{"$ref": "#/$defs/output"}`)
	assertJSON(t, defOf(out, "output"), `{"type":"boolean"}`)
}

// A recursive single-expression output is driven by the same fixpoint as the
// map form and converges to a non-object accumulator type.
func TestGenerate_OutputRecursiveSingleExpression(t *testing.T) {
	out := runGenerate(t, `{
		"name": "p",
		"input_schema": {"type":"object","properties":{"seed":{"type":"integer"}},"required":["seed"]},
		"tasks": [
			{
				"id": "loop",
				"output": "$: (self.previous ?? input.seed) + 1",
				"switch": [{"case":"self.output < 10","goto":"$loop"},{"goto":"end"}]
			}
		]
	}`)
	assertJSON(t, defOf(out, "loop_output"), `{"type":"integer"}`)
}

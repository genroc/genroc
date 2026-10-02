package shape

import (
	"encoding/json"

	"genroc/internal/schema"
)

const exprLeafDesc = "A literal string, or a $: expression / ${ } template evaluated against the context."

// The name ModelShape is coupled to the spec builder's InterceptDefName and to the OpenAPI
// builder's #/$defs → #/components/schemas rewrite.
func modelShapeRef() map[string]any { return map[string]any{"$ref": "#/$defs/ModelShape"} }

// GenericValueSchema generates the ModelShape def every free Shape slot $refs. anyOf, not oneOf:
// the branches overlap. Keep array items {} — a $ref to ModelShape there becomes an eager cycle in
// openapi-typescript's output that tsc rejects (TS2502).
func GenericValueSchema() ([]byte, error) {
	return json.Marshal(map[string]any{
		"anyOf": []any{
			map[string]any{"type": "string", "description": "A $: typed expression, a ${ } interpolation template, or a literal string."},
			map[string]any{"type": "number"},
			map[string]any{"type": "boolean"},
			map[string]any{"type": "null"},
			map[string]any{"type": "array", "description": "Literal array; each element is recursively a Shape.", "items": map[string]any{}},
			map[string]any{"type": "object", "description": "Literal object; each value is recursively a Shape.", "additionalProperties": modelShapeRef()},
		},
	})
}

// RelaxedSchema is the editor schema for a Shape that must conform to target: every node also
// accepts a string, the expression escape hatch.
func RelaxedSchema(target schema.Schema) ([]byte, error) {
	return json.Marshal(target.Relaxed(exprLeafDesc))
}

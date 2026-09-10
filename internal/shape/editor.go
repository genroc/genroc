package shape

import (
	"encoding/json"

	"genroc/internal/schema"
)

// exprLeafDesc annotates every string position in a relaxed schema: a string leaf accepts a
// literal, or — because a Shape leaf is an expression — a $: expression / ${ } template.
const exprLeafDesc = "A literal string, or a $: expression / ${ } template evaluated against the context."

// modelShapeRef is the recursive self-reference every free (unconstrained) Shape position
// resolves to: the generic Value def. The spec builder's InterceptDefName keeps this type's
// generated def name as ModelShape, and the OpenAPI builder rewrites #/$defs/ModelShape to
// #/components/schemas/ModelShape.
func modelShapeRef() map[string]any { return map[string]any{"$ref": "#/$defs/ModelShape"} }

// GenericValueSchema generates the ModelShape def — the recursive Value grammar, with the string
// branch doubling as the $:/${ } escape hatch at every level. Every free Shape slot resolves here
// via $ref; RelaxedSchema handles the bounded ones. anyOf, not oneOf: the branches overlap, which
// oneOf's exactly-one rule would spuriously reject.
//
// Do not change array items from permissive ({}) to $ref ModelShape: openapi-typescript emits a
// $ref as an indexed access, and an array of that self-reference is an eager cycle tsc rejects
// (TS2502), breaking the client typecheck.
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

// RelaxedSchema generates the editor JSON Schema for a Shape whose value must conform to target.
// schema.Relaxed does the work: every node becomes "the literal value, or a string" — the
// expression escape hatch — recursively, with each string position labelled exprLeafDesc.
func RelaxedSchema(target schema.Schema) ([]byte, error) {
	return json.Marshal(target.Relaxed(exprLeafDesc))
}

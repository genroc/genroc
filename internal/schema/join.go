package schema

import "sort"

// nodesEqual reports whether a and b denote the same type, compared in canonical form.
func nodesEqual(a, b *node) bool {
	return nodeCanonJSON(canonicalizeNode(a)) == nodeCanonJSON(canonicalizeNode(b))
}

// joinNodes is the least upper bound: objects merge per property (a one-sided key nullable),
// anything else unions. The canonical result gives the fixpoint a monotone, terminating step.
func joinNodes(a, b *node) *node {
	if a == nil {
		return canonicalizeNode(b)
	}
	if b == nil {
		return canonicalizeNode(a)
	}
	if nodesEqual(a, b) {
		return canonicalizeNode(a)
	}
	// A pure-null operand only contributes nullability; stripping it would leave an
	// empty schema, so handle it directly: join(x, null) = x made nullable.
	if isNullType(a) {
		return canonicalizeNode(withNull(b))
	}
	if isNullType(b) {
		return canonicalizeNode(withNull(a))
	}

	nullable := hasNullType(a) || hasNullType(b)
	na, nb := stripNull(a), stripNull(b)

	var res *node
	switch {
	case nodesEqual(na, nb):
		// The inputs differed only in nullability (e.g. anyOf[$ref x, null] vs
		// $ref x) — no union needed, just restore the null below.
		res = na
	case isObjectType(na) && isObjectType(nb):
		res = joinObjects(na, nb)
	default:
		res = &node{OneOf: []*node{na, nb}}
	}
	if nullable {
		res = withNull(res)
	}
	return canonicalizeNode(res)
}

func isObjectType(s *node) bool {
	return s != nil && (s.Type.Contains("object") || s.Properties != nil || s.AdditionalProperties != nil)
}

// joinObjects merges two object schemas property-wise: a key on both is joined; a key
// on only one side is kept but made nullable; a property is required only when both
// sides require it.
func joinObjects(a, b *node) *node {
	keys := make(map[string]struct{}, len(a.Properties)+len(b.Properties))
	for k := range a.Properties {
		keys[k] = struct{}{}
	}
	for k := range b.Properties {
		keys[k] = struct{}{}
	}

	props := make(map[string]*node, len(keys))
	for k := range keys {
		av, bv := a.Properties[k], b.Properties[k]
		switch {
		case av == nil:
			props[k] = withNull(canonicalizeNode(bv))
		case bv == nil:
			props[k] = withNull(canonicalizeNode(av))
		default:
			props[k] = joinNodes(av, bv)
		}
	}

	var required []string
	for k := range keys {
		if a.Properties[k] != nil && b.Properties[k] != nil && isRequired(a, k) && isRequired(b, k) {
			required = append(required, k)
		}
	}
	sort.Strings(required)

	out := &node{Type: SchemaType{"object"}, Properties: props}
	if len(required) > 0 {
		out.Required = required
	}
	// Open-map keys: join both sides' additionalProperties, or carry the one side
	// that has it (the join permits those extras, so it widens rather than closes).
	switch {
	case a.AdditionalProperties != nil && b.AdditionalProperties != nil:
		out.AdditionalProperties = joinNodes(a.AdditionalProperties, b.AdditionalProperties)
	case a.AdditionalProperties != nil:
		out.AdditionalProperties = canonicalizeNode(a.AdditionalProperties)
	case b.AdditionalProperties != nil:
		out.AdditionalProperties = canonicalizeNode(b.AdditionalProperties)
	}
	return out
}

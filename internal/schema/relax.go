package schema

// mapChildren applies fn to every direct sub-schema — the single definition of "where
// sub-schemas live" for transforms (properties, items, additionalProperties, oneOf/anyOf/
// allOf, $defs); fn decides recursion. New keywords get picked up here once.
func mapChildren(n *node, fn func(*node) *node) *node {
	m := *n
	if n.Properties != nil {
		props := make(map[string]*node, len(n.Properties))
		for k, v := range n.Properties {
			props[k] = fn(v)
		}
		m.Properties = props
	}
	if n.Items != nil {
		m.Items = fn(n.Items)
	}
	if n.AdditionalProperties != nil {
		m.AdditionalProperties = fn(n.AdditionalProperties)
	}
	m.OneOf = mapEach(n.OneOf, fn)
	m.AnyOf = mapEach(n.AnyOf, fn)
	m.AllOf = mapEach(n.AllOf, fn)
	if n.Defs != nil {
		defs := make(map[string]*node, len(n.Defs))
		for k, v := range n.Defs {
			defs[k] = fn(v)
		}
		m.Defs = defs
	}
	return &m
}

func mapEach(vs []*node, fn func(*node) *node) []*node {
	if vs == nil {
		return nil
	}
	out := make([]*node, len(vs))
	for i, v := range vs {
		out[i] = fn(v)
	}
	return out
}

// Relaxed returns a copy of s in which every node also admits a plain string, applied
// recursively via mapChildren so it is exhaustive over nested objects, arrays, unions and
// $defs — not a special-cased handful of keywords. This is the editor's "any value may be
// written as its literal, or at any level as an expression (a string)" transform: a Shape
// leaf is an expression, and an expression is authored as a string. A pure string leaf is
// left as a string (literal and expression coincide); every other node becomes `node | string`.
//
// stringNote, when non-empty, is attached as the description of every string position (a
// string leaf, or the string alternative added to a non-string node) that does not already
// carry one — so the caller can label the expression escape hatch without a second pass. The
// node type carries descriptions natively, so no post-processing of the JSON is needed.
func (s Schema) Relaxed(stringNote string) Schema {
	if s.n == nil {
		return s
	}
	return Schema{relaxToString(s.n, stringNote)}
}

// relaxToString relaxes one node's children, then makes the node itself `node | string`
// unless it already admits a string.
func relaxToString(s *node, note string) *node {
	if s == nil {
		return nil
	}
	relaxed := mapChildren(s, func(c *node) *node { return relaxToString(c, note) })
	if nodeAdmitsString(relaxed) {
		annotateString(relaxed, note)
		return relaxed
	}
	strAlt := &node{Type: SchemaType{"string"}}
	annotateString(strAlt, note)
	// Root $defs must live on the outermost node so refs still resolve; hoist them onto the
	// wrapper.
	defs := relaxed.Defs
	relaxed.Defs = nil
	return &node{AnyOf: []*node{relaxed, strAlt}, Defs: defs}
}

// annotateString sets note as n's description when note is given and n has none — used to
// label a string (expression) position without clobbering an author's own wording.
func annotateString(n *node, note string) {
	if note != "" && n.Description == "" {
		n.Description = note
	}
}

// nodeAdmitsString reports whether s already accepts a plain string value, so relax needn't
// add a redundant alternative. An enum narrows to specific values, so it does not count — an
// expression must stay allowed alongside the enum members.
func nodeAdmitsString(s *node) bool {
	if len(s.Enum) > 0 {
		return false
	}
	for _, t := range s.Type {
		if t == "string" {
			return true
		}
	}
	return false
}

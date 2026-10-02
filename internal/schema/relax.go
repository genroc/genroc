package schema

// Relaxed makes every node also admit a plain string (an expression at any level), via
// mapChildren so it is exhaustive. stringNote labels string positions with no description.
func (s Schema) Relaxed(stringNote string) Schema {
	if s.n == nil {
		return s
	}
	return Schema{n: relaxToString(s.n, stringNote)}
}

// relaxToString relaxes one node's children, then makes the node itself `node | string`
// unless it already admits a string.
func relaxToString(s *node, note string) *node {
	if s == nil {
		return nil
	}
	relaxed := mapChildren(s, func(_ childSlot, c *node) *node { return relaxToString(c, note) })
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

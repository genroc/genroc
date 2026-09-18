package schema

import "sort"

// The third member of a family: ConformsExactlyTo is the RELATION (may this value be conformed
// to that declaration), ConformToSchemaExactly is the FILL (do it), and Conformed is the TYPE of
// what comes out. It exists so that "what is this slot" has one answer: the language server, the
// CLI and the resolver manifest all read the type validation stored, and none carries a rule of
// its own. specs/declared-slot-schemas.md §1, §6.

// Conformed is the type of a value of s once Validate(v, ConformToSchemaExactly) against
// declared has run — what actually LEAVES a declared slot.
//
// It is derived from s, never from declared: the static check proved s fits, so s is already
// the more precise description, and the declaration changes it in exactly the ways the fill
// does. An optional property declared non-nullable loses its null and becomes may-be-absent
// (the fill removes the key); a required nullable property s never sets appears as null (the
// fill writes it in); the declaration's `description` is carried, because it is the prose an
// imported schema exists to bring. Where the declaration says nothing — the top type — s is
// returned as it is, which is what makes a generic child's `input: {}` type as what the caller
// sent rather than as `unknown`.
//
// A `$ref` in s is left symbolic wherever the declaration does not reach inside it, so a
// recursive type stays finite; where it does, the reference is followed under a depth bound.
// Sound only for a pair the relation accepted — anything else is the assertion in the engine.
func (s Schema) Conformed(declared Schema) Schema {
	if s.n == nil {
		return s
	}
	out := conformedNode(s.n, s.rootDefs(), declared.n, declared.rootDefs(), conformedDepth)
	return wrap(out, s.rootDefs())
}

// conformedDepth bounds the walk. A declaration deep enough to reach it is not one anybody
// wrote by hand; past it s is returned as is, which is always sound.
const conformedDepth = 32

func conformedNode(inf *node, infDefs map[string]*node, dec *node, decDefs map[string]*node, depth int) *node {
	if inf == nil || depth == 0 {
		return inf
	}
	decR, err := deref(dec, decDefs)
	if err != nil || decR == nil || isEmptyNode(decR) {
		// Nothing to apply. The prose is still the declaration's to give, and a top type
		// written with a description is exactly how a generic child documents its payload.
		return withProse(inf, dec)
	}
	infR, err := deref(inf, infDefs)
	if err != nil || infR == nil {
		return withProse(inf, dec)
	}

	switch {
	case decR.Properties != nil && (infR.Properties != nil || hasObjectArm(infR)):
		return conformedObject(infR, infDefs, decR, decDefs, depth)
	case decR.Items != nil && infR.Items != nil:
		out := *infR
		out.Items = conformedNode(infR.Items, infDefs, decR.Items, decDefs, depth-1)
		return withProse(&out, decR)
	}
	// A leaf, or a shape the fill does not look inside. s fits and is the more precise side;
	// whether a null here survives is the PARENT's decision, made where the key lives.
	return withProse(inf, decR)
}

// conformedObject applies the two repairs the fill makes at an object, property by property.
func conformedObject(inf *node, infDefs map[string]*node, dec *node, decDefs map[string]*node, depth int) *node {
	nullable := hasNullResolved(inf, infDefs)
	base := stripNullIn(inf, infDefs, map[*node]bool{})
	baseR, err := deref(base, infDefs)
	if err != nil || baseR == nil {
		return withProse(inf, dec)
	}

	out := *baseR
	out.Properties = make(map[string]*node, len(baseR.Properties))
	out.Required = nil
	for name, ip := range baseR.Properties {
		dp, declared := dec.Properties[name]
		if !declared || dp == nil {
			// Undeclared: the closed check refuses this at registration, so it is only reached
			// where the declaration is open, and the fill leaves an open map's key alone.
			out.Properties[name] = ip
			if isRequired(baseR, name) {
				out.Required = append(out.Required, name)
			}
			continue
		}
		prop := conformedNode(ip, infDefs, dp, decDefs, depth-1)
		// The REMOVE half: a null in an optional property the declaration will not hold has
		// its key removed, so what stays is non-null and the key may be absent.
		removed := hasNullResolved(ip, infDefs) && !hasNullResolved(dp, decDefs) && !isRequired(dec, name)
		if removed {
			prop = stripNullIn(prop, infDefs, map[*node]bool{})
		}
		out.Properties[name] = prop
		present := isRequired(baseR, name) && !removed
		// The INSERT half guarantees presence from the other side: a required nullable the
		// value lacks is written in as null, so it is there whatever the caller did.
		if isRequired(dec, name) && hasNullResolved(dp, decDefs) {
			present = true
		}
		if present {
			out.Required = append(out.Required, name)
		}
	}
	// Declared, never set by the value: absent — unless the fill writes it in.
	for name, dp := range dec.Properties {
		if _, set := baseR.Properties[name]; set || dp == nil {
			continue
		}
		if isRequired(dec, name) && hasNullResolved(dp, decDefs) {
			out.Properties[name] = withProse(&node{Type: SchemaType{"null"}}, dp)
			out.Required = append(out.Required, name)
		}
	}
	sort.Strings(out.Required)
	result := withProse(&out, dec)
	if nullable {
		return withNull(result)
	}
	return result
}

// hasObjectArm reports whether a nullable wrapper holds an object — `anyOf[{object}, null]` is
// how inference spells an optional object, and its properties sit on the arm.
func hasObjectArm(n *node) bool {
	for _, arms := range [][]*node{n.AnyOf, n.OneOf} {
		for _, a := range arms {
			if a != nil && a.Properties != nil {
				return true
			}
		}
	}
	return false
}

// withProse is n carrying dec's description, copied only when there is one to carry. Never
// dec's `default`: that keyword describes how the CONTAINING object is conformed, and a type
// carrying it beside `required` is not a valid document.
func withProse(n, dec *node) *node {
	if n == nil || dec == nil || dec.Description == "" || n.Description == dec.Description {
		return n
	}
	m := *n
	m.Description = dec.Description
	return &m
}

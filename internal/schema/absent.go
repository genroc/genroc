package schema

// FillAbsentAsNull returns data with an explicit null inserted wherever s requires a
// property whose type admits null and the value does not carry one.
//
// It is the migration half of IsSubsetAbsentAsNull, and the two are a PAIR: that relation
// decides a gap between two versions is closable, and this is what closes it. They must
// accept exactly the same gaps. A relation that tolerates more than the fill can close
// promises an upgrade that then fails to conform; a fill that closes more is dead code
// nobody asked for. Change one and you must change the other — schematest/absent_test.go
// is what makes that loud.
//
// The distinction it erases is real everywhere else: conformObject rejects an absent
// required key whatever its type, so `required` governs documents at a boundary while
// nullability governs reads. This turns the first into the second for one value, which is
// why the result is safe to hand to a strict conform afterwards.
//
// It only ever ADDS keys. Nothing is stripped, rewritten, or reordered — including
// undeclared properties, which a conform would prune. That is what keeps a migration
// built on it non-destructive, and reversible in the only sense that matters.
func (s Schema) FillAbsentAsNull(data any) any {
	return fillAbsentAsNull(s.n, s.rootDefs(), data, nil)
}

// fillAbsentAsNull walks schema and value together. visiting guards a $ref that returns to
// a node already expanded at this value position, matching conformGuard: descending into a
// property or element consumes value depth, so revisiting a node there is productive.
func fillAbsentAsNull(nd *node, defs map[string]*node, data any, visiting map[*node]bool) any {
	if nd == nil || data == nil {
		return data
	}
	resolved, err := deref(nd, defs)
	if err != nil || resolved == nil {
		return data
	}
	if resolved != nd {
		if visiting[resolved] {
			return data
		}
		if visiting == nil {
			visiting = map[*node]bool{}
		}
		visiting[resolved] = true
		defer delete(visiting, resolved)
	}

	// A union describes one value several ways, and the variants can disagree about which
	// properties exist. Filling through all of them would inject keys from a variant the
	// value does not match, so the choice is made by CONFORMING the filled candidate: the
	// first variant it satisfies is the one it belongs to. This has to come before the
	// type switch below — a union node carries no properties of its own, so reaching the
	// switch first fills nothing and the variants are never consulted.
	if variants := unionVariants(resolved); len(variants) > 0 {
		for _, variant := range variants {
			if variant == nil {
				continue
			}
			filled := fillAbsentAsNull(variant, defs, data, visiting)
			if _, err := conform(variant, defs, filled, ""); err == nil {
				return filled
			}
		}
		return data
	}

	switch v := data.(type) {
	case map[string]any:
		out := make(map[string]any, len(v))
		for k, val := range v {
			// An undeclared key has no schema to fill through, and is kept as it stands:
			// this is a migration, not a conform, so nothing is pruned.
			out[k] = fillAbsentAsNull(propertySchema(resolved, k), defs, val, nil)
		}
		for _, name := range resolved.Required {
			if _, present := out[name]; present {
				continue
			}
			// A required name with no declared property has no type to call nullable, so
			// there is nothing to invent — and the relation refuses that pair for the same
			// reason, which is what keeps the two in step.
			if prop, ok := resolved.Properties[name]; ok && hasNullResolved(prop, defs) {
				out[name] = nil
			}
		}
		return out
	case []any:
		if resolved.Items == nil {
			return v
		}
		out := make([]any, len(v))
		for i, elem := range v {
			out[i] = fillAbsentAsNull(resolved.Items, defs, elem, nil)
		}
		return out
	}
	return data
}

// unionVariants returns a node's union members — anyOf and oneOf both, since either can
// carry the object variants a fill has to choose between.
func unionVariants(nd *node) []*node {
	if len(nd.AnyOf) > 0 {
		return nd.AnyOf
	}
	return nd.OneOf
}

// propertySchema picks the schema governing one key: its declared property, or the open-map
// value type when it is undeclared.
func propertySchema(nd *node, key string) *node {
	if prop, ok := nd.Properties[key]; ok {
		return prop
	}
	return nd.AdditionalProperties
}

package schema

// ─── Builders (immutable) ───────────────────────────────────────────────────────

func Object() Schema {
	return Schema{n: &node{Type: SchemaType{"object"}}}
}

func Type(types ...string) Schema {
	return Schema{n: &node{Type: SchemaType(types)}}
}

// Array returns an array Schema whose elements conform to item; a zero item yields
// an itemless array (any element). item is embedded structurally — any root $defs it
// carries are dropped, so it must reference the shared pool.
func Array(item Schema) Schema {
	n := &node{Type: SchemaType{"array"}}
	if item.n != nil {
		n.Items = item.n
	}
	return Schema{n: n}
}

func Ref(name string) Schema {
	return Schema{n: &node{Ref: "#/$defs/" + name}}
}

// Map returns an open-object Schema whose undeclared keys must each conform to sub.
// Like Array, sub is embedded structurally — any root $defs it carries are dropped,
// so it should reference the shared pool.
func Map(sub Schema) Schema {
	n := &node{Type: SchemaType{"object"}}
	if sub.n != nil {
		n.AdditionalProperties = sub.n
	}
	return Schema{n: n}
}

// ArrayLiteral builds the schema of an array literal from its already-inferred element
// schemas. An empty slice is the provably-empty array (maxItems 0) — which is what lets
// a literal `[]` (and the `?? []` idiom) be a subset of any array<T>; a non-empty slice
// is array<join of elements>, with an empty-array element absorbed so [xs, []] keeps xs's
// element type. Element root $defs are dropped (WithoutDefs); the caller owns the pool.
func ArrayLiteral(elems []Schema) Schema {
	if len(elems) == 0 {
		return emptyArray()
	}
	var joined Schema
	for i, it := range elems {
		it = it.WithoutDefs()
		switch merged, ok := absorbEmptyArray(joined, it); {
		case i == 0:
			joined = it
		case ok:
			joined = merged
		default:
			joined = joined.Join(it)
		}
	}
	return Array(joined.Canonicalize())
}

func OneOf(variants ...Schema) Schema {
	return Schema{n: &node{OneOf: nodesOf(variants), Defs: defsOf(variants)}}
}

func AnyOf(variants ...Schema) Schema {
	return Schema{n: &node{AnyOf: nodesOf(variants), Defs: defsOf(variants)}}
}

func nodesOf(vs []Schema) []*node {
	out := make([]*node, len(vs))
	for i, v := range vs {
		out[i] = v.n
	}
	return out
}

// defsOf carries the pool up from the arms. Resolution reads the handle on the ROOT node
// (`rootDefs`), so a union built without one cannot deref a `$ref` ARM: `HasNull` answers false
// about a nullable value and `Summary` renders it `unknown`. Every arm came from one Generate,
// so the first pool found is the pool.
func defsOf(vs []Schema) map[string]*node {
	for _, v := range vs {
		if d := v.rootDefs(); d != nil {
			return d
		}
	}
	return nil
}

// WithProperty returns a copy of s (treated as an object schema) with property name
// set to sub; required adds it to the required list if not already there. Root $defs
// are preserved.
func (s Schema) WithProperty(name string, sub Schema, required bool) Schema {
	base := s.n
	if base == nil {
		base = &node{}
	}
	n := *base
	n.Properties = make(map[string]*node, len(base.Properties)+1)
	for k, v := range base.Properties {
		n.Properties[k] = v
	}
	n.Properties[name] = sub.n
	if required && !isRequired(base, name) {
		n.Required = append(append([]string{}, base.Required...), name)
	}
	// Guards carry: this returns the SAME value with one more member, which is how a context
	// gains `self` between the base scope and the slot that reads it. Navigation is the case
	// that must not carry them — it returns a different value, and they are keyed from the
	// root. specs/guard-narrowing.md.
	return s.keepingGuards(Schema{n: &n})
}

// WithDescription returns a copy of s carrying desc as its documentation annotation (root
// $defs preserved). The annotation has no bearing on type-checking.
func (s Schema) WithDescription(desc string) Schema {
	base := s.n
	if base == nil {
		base = &node{}
	}
	n := *base
	n.Description = desc
	return Schema{n: &n}
}

func (s Schema) WithDef(name string, def Schema) Schema {
	cloned, _ := deepClone(s.n)
	if cloned == nil {
		cloned = &node{}
	}
	if cloned.Defs == nil {
		cloned.Defs = make(map[string]*node)
	} else {
		newDefs := make(map[string]*node, len(cloned.Defs)+1)
		for k, v := range cloned.Defs {
			newDefs[k] = v
		}
		cloned.Defs = newDefs
	}
	cloned.Defs[name] = def.n
	return Schema{n: cloned}
}

package schema

// ─── Accessors (read-only views) ────────────────────────────────────────────────

func (s Schema) Type() SchemaType {
	if s.n == nil {
		return nil
	}
	return s.n.Type
}

func (s Schema) Required() []string {
	if s.n == nil {
		return nil
	}
	return s.n.Required
}

// Properties is a raw structural view — unlike Property (single-step navigation)
// it does no $ref resolution and no nullable-wrapping of optionals.
func (s Schema) Properties() map[string]Schema {
	if s.n == nil || s.n.Properties == nil {
		return nil
	}
	out := make(map[string]Schema, len(s.n.Properties))
	for name, p := range s.n.Properties {
		out[name] = wrap(p, s.rootDefs())
	}
	return out
}

func (s Schema) Default() any {
	if s.n == nil {
		return nil
	}
	return s.n.Default
}

// Description returns the node's free-text documentation annotation ("" if none). It has no
// type meaning — see the node.Description doc.
func (s Schema) Description() string {
	if s.n == nil {
		return ""
	}
	return s.n.Description
}

func (s Schema) AdditionalProperties() (Schema, bool) {
	if s.n == nil || s.n.AdditionalProperties == nil {
		return Schema{}, false
	}
	return wrap(s.n.AdditionalProperties, s.rootDefs()), true
}

func (s Schema) HasRef() bool {
	return s.n != nil && s.n.Ref != ""
}

func (s Schema) HasDefs() bool {
	return s.n != nil && len(s.n.Defs) > 0
}

func (s Schema) HasItems() bool {
	return s.n != nil && s.n.Items != nil
}

func (s Schema) HasProperties() bool {
	return s.n != nil && len(s.n.Properties) > 0
}

func (s Schema) HasCombinators() bool {
	return s.n != nil && len(s.n.OneOf)+len(s.n.AnyOf)+len(s.n.AllOf) > 0
}

// Variants returns the union members — anyOf when present, else oneOf — with each
// nil member as a zero Schema, or nil for a non-union schema.
func (s Schema) Variants() []Schema {
	if s.n == nil {
		return nil
	}
	variants := s.n.AnyOf
	if variants == nil {
		variants = s.n.OneOf
	}
	if variants == nil {
		return nil
	}
	out := make([]Schema, len(variants))
	for i, v := range variants {
		if v != nil {
			out[i] = wrap(v, s.rootDefs())
		}
	}
	return out
}

func (s Schema) Items() Schema {
	if s.n == nil || s.n.Items == nil {
		return Schema{}
	}
	return wrap(s.n.Items, s.rootDefs())
}

// Enum's returned slice must not be modified by the caller.
func (s Schema) Enum() []any {
	if s.n == nil {
		return nil
	}
	return s.n.Enum
}

func (s Schema) Minimum() (float64, bool) {
	if s.n == nil || s.n.Minimum == nil {
		return 0, false
	}
	return *s.n.Minimum, true
}

func (s Schema) Maximum() (float64, bool) {
	if s.n == nil || s.n.Maximum == nil {
		return 0, false
	}
	return *s.n.Maximum, true
}

func (s Schema) MinLength() (int, bool) {
	if s.n == nil || s.n.MinLength == nil {
		return 0, false
	}
	return *s.n.MinLength, true
}

func (s Schema) MaxLength() (int, bool) {
	if s.n == nil || s.n.MaxLength == nil {
		return 0, false
	}
	return *s.n.MaxLength, true
}

func (s Schema) MinItems() (int, bool) {
	if s.n == nil || s.n.MinItems == nil {
		return 0, false
	}
	return *s.n.MinItems, true
}

func (s Schema) MaxItems() (int, bool) {
	if s.n == nil || s.n.MaxItems == nil {
		return 0, false
	}
	return *s.n.MaxItems, true
}

// Resolve follows a $ref to its target in the root $defs. A non-ref schema is
// returned unchanged; an unresolvable ref is an error.
func (s Schema) Resolve() (Schema, error) {
	if s.n == nil || s.n.Ref == "" {
		return s, nil
	}
	target, err := deref(s.n, s.rootDefs())
	if err != nil {
		return Schema{}, err
	}
	return wrap(target, s.rootDefs()), nil
}

// ─── Node algebra (immutable transforms and predicates) ─────────────────────────

func (s Schema) WithNull() Schema {
	return wrap(withNull(s.n), s.rootDefs())
}

// StripNull removes every null the value may take, following a `$ref` only where the null is
// behind it — so it agrees with HasNull and the result stays finite. specs/guard-narrowing.md.
func (s Schema) StripNull() Schema {
	return wrap(stripNullIn(s.n, s.rootDefs(), map[*node]bool{}), s.rootDefs())
}

// IsNull reports whether s is exactly {type:"null"} (cf. HasNull).
func (s Schema) IsNull() bool {
	return isNullType(s.n)
}

// HasNull follows $refs: nullability may be declared inside a referenced
// definition, not just on the use-site wrapper.
func (s Schema) HasNull() bool {
	return hasNullResolved(s.n, s.rootDefs())
}

// Join returns the least upper bound of s and o — grows estimates in the
// recursive-output fixpoint.
func (s Schema) Join(o Schema) Schema {
	return wrap(joinNodes(s.n, o.n), s.rootDefs())
}

// Canonicalize returns s in canonical form (stable order, merged variants) so
// equal types compare equal.
func (s Schema) Canonicalize() Schema {
	return wrap(canonicalizeNode(s.n), s.rootDefs())
}

// Size is the marshaled byte size — the growth bound the recursive fixpoint enforces.
func (s Schema) Size() int {
	return nodeSize(s.n)
}

func (s Schema) Equal(o Schema) bool {
	return nodesEqual(s.n, o.n)
}

// IsSubset requires both schemas to be normalized.
func (s Schema) IsSubset(super Schema) bool {
	return isSubset(s.n, super.n)
}

// IsSubsetAbsentAsNull is IsSubset where super may require a nullable property sub leaves
// optional. Sound only where nothing conforms the value against super; both must be normalized.
func (s Schema) IsSubsetAbsentAsNull(super Schema) bool {
	return absentAsNullSubset(s.n, super.n)
}

// IsSubsetAsStored reads both as already-conformed data: IsSubsetAbsentAsNull, plus a property s
// defaults is present. Sound only where nothing conforms against super afterwards; both must be
// normalized. specs/compat-command.md §2e.
func (s Schema) IsSubsetAsStored(super Schema) bool {
	return storedSubset(s.n, super.n)
}

// NarrowsTo is IsSubset with every {} in s accepted by whatever super declares there. Sound ONLY
// where the value is conformed against super at runtime. Both must be normalized.
func (s Schema) NarrowsTo(super Schema) bool {
	return narrowsTo(s.n, super.n)
}

// ExplainSubset returns every break of s against super in walk order (nil when it fits), from the
// same walk as IsSubset. Facts, not sentences; costs a traversal, so call it only after a no.
func (s Schema) ExplainSubset(super Schema) []*SubsetBreak {
	return subsetBreaks(s.n, super.n, subsetMode{})
}

// ExplainSubsetAsStored is ExplainSubset for the IsSubsetAsStored relation.
func (s Schema) ExplainSubsetAsStored(super Schema) []*SubsetBreak {
	return subsetBreaks(s.n, super.n, storedMode())
}

// ConformsExactlyTo: every value of s survives Validate(v, ConformToSchemaExactly) against super
// with its key set unchanged. Sound only where the value IS so conformed; both must be
// normalized. specs/declared-slot-schemas.md §4.
func (s Schema) ConformsExactlyTo(super Schema) bool {
	return conformsExactlyTo(s.n, super.n)
}

// ExplainConformsExactlyTo is ExplainSubset for the ConformsExactlyTo relation.
func (s Schema) ExplainConformsExactlyTo(super Schema) []*SubsetBreak {
	return subsetBreaks(s.n, super.n, conformsExactlyMode())
}

// ExplainNarrowsTo is ExplainSubset for the NarrowsTo relation, and carries its soundness
// condition with it: only a slot whose value is conformed against super at runtime.
func (s Schema) ExplainNarrowsTo(super Schema) []*SubsetBreak {
	return subsetBreaks(s.n, super.n, subsetMode{narrow: true})
}

// ─── Secrets ────────────────────────────────────────────────────────────────────

// IsSecret looks through nullable / single-variant union wrappers.
func (s Schema) IsSecret() bool {
	return isSecret(s.n)
}

// ContainsSecret reports `secret: true` anywhere, so registration can refuse it outside
// config_schema — there it promises a scrub nothing delivers. specs/object-store.md §secret.
func (s Schema) ContainsSecret() bool { return containsSecret(s.n, map[*node]bool{}) }

func containsSecret(n *node, seen map[*node]bool) bool {
	if n == nil || seen[n] {
		return false
	}
	seen[n] = true
	if n.Secret {
		return true
	}
	for _, c := range n.Properties {
		if containsSecret(c, seen) {
			return true
		}
	}
	for _, c := range n.Defs {
		if containsSecret(c, seen) {
			return true
		}
	}
	for _, group := range [][]*node{n.OneOf, n.AnyOf, n.AllOf} {
		for _, c := range group {
			if containsSecret(c, seen) {
				return true
			}
		}
	}
	return containsSecret(n.Items, seen) || containsSecret(n.AdditionalProperties, seen)
}

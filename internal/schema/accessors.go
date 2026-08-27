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

func (s Schema) StripNull() Schema {
	return wrap(stripNull(s.n), s.rootDefs())
}

// Taint marks the whole value secret, conservatively.
func (s Schema) Taint() Schema {
	return wrap(taintNode(s.n), s.rootDefs())
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

// IsSubsetAbsentAsNull is IsSubset with one rule relaxed: super may require a property whose
// type admits null without sub requiring it, since a missing key and a null one navigate
// identically. Sound only where nothing validates the value against super — a runtime conform
// DOES reject a missing required key. Both schemas must be normalized.
func (s Schema) IsSubsetAbsentAsNull(super Schema) bool {
	return absentAsNullSubset(s.n, super.n)
}

// IsSubsetAsStored compares two schemas as descriptions of data already conformed — an
// instance's stored state. It is IsSubsetAbsentAsNull plus one rule: a property s declares
// with a default is guaranteed present. Sound only where nothing conforms the value against
// super afterwards. Both schemas must be normalized. Design: specs/compat-command.md §2e.
func (s Schema) IsSubsetAsStored(super Schema) bool {
	return storedSubset(s.n, super.n)
}

// NarrowsTo is IsSubset with unknowns admitted: every empty schema in s is accepted by
// whatever super declares at that position, at any depth. It answers "could this be narrowed
// to super?", so it is sound ONLY where the value is conformed against super at runtime.
// Both schemas must be normalized.
func (s Schema) NarrowsTo(super Schema) bool {
	return narrowsTo(s.n, super.n)
}

// ExplainSubset names every place s fails to fit super, in walk order, or nothing when it
// fits. It runs the SAME walk as IsSubset with reporting switched on, so the two can never
// disagree. Callers word the breaks themselves (see SubsetBreakKind); this returns facts,
// not sentences. Costs a second traversal, so call it only after IsSubset has said no.
func (s Schema) ExplainSubset(super Schema) []*SubsetBreak {
	return subsetBreaks(s.n, super.n, subsetMode{})
}

// ExplainSubsetAsStored is ExplainSubset for the IsSubsetAsStored relation.
func (s Schema) ExplainSubsetAsStored(super Schema) []*SubsetBreak {
	return subsetBreaks(s.n, super.n, subsetMode{absentAsNull: true, afterConform: true})
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

// SecretAt reports whether the value at path is secret — the path either passes
// through or ends at a secret node (reading from inside a secret object is itself
// secret). False when path cannot be resolved.
func (s Schema) SecretAt(path string) bool {
	return pathHitsSecret(s.n, s.rootDefs(), path)
}

// Redact replaces every secret-marked field in data with "***", descending via the
// same navigation type inference uses.
func (s Schema) Redact(data any) any {
	return redact(data, s.n, s.rootDefs())
}

// CollectSecrets returns the string form of every secret-marked value in data —
// the gather half of log redaction.
func (s Schema) CollectSecrets(data any) []string {
	var out []string
	collectSecrets(data, s.n, s.rootDefs(), &out)
	return out
}

// ContainsSecret reports whether `secret: true` appears anywhere in the document, at any depth.
//
// It exists so registration can refuse the marker outside config_schema. `secret: true` keeps a
// value out of the server's stdout, and the only values the log scrubber can find are the config
// values it knows verbatim -- so the marker elsewhere would promise a protection nothing
// delivers, which is worse than not offering it. specs/object-store.md §secret: true is
// CONFIG-ONLY.
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

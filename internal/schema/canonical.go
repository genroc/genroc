package schema

import (
	"encoding/json"
	"math"
	"sort"
)

// canonicalizeNode makes two schemas denoting the same type byte-identical — the equality
// the recursive-inference fixpoint relies on. Flattens same-kind compositions, dedupes and
// sorts variants, collapses singletons, merges simple-primitive unions (allOf never). Idempotent.
func canonicalizeNode(s *node) *node {
	if s == nil {
		return nil
	}
	n := mapChildren(s, func(sl childSlot, c *node) *node {
		if sl.kind == slotDefs {
			return c // a namespace, not part of the type being canonicalized
		}
		return canonicalizeNode(c)
	})
	// No type meaning, and the fixpoint keys off canonical JSON.
	n.Description = ""
	n.Type = SchemaType(sortDedupStrings([]string(s.Type)))
	n.Required = sortDedupStrings(s.Required)
	n.OneOf = canonVariants(n.OneOf, kindOneOf)
	n.AnyOf = canonVariants(n.AnyOf, kindAnyOf)
	n.AllOf = canonVariants(n.AllOf, kindAllOf)

	return collapse(n)
}

type compositionKind int

const (
	kindOneOf compositionKind = iota
	kindAnyOf
	kindAllOf
)

// canonVariants flattens same-kind nested compositions, then dedups and sorts by canonical
// JSON. Its input is already canonical and nil-free (mapChildren).
func canonVariants(vs []*node, kind compositionKind) []*node {
	if len(vs) == 0 {
		return nil
	}
	flat := make([]*node, 0, len(vs))
	for _, v := range vs {
		if inner, ok := pureComposition(v, kind); ok {
			flat = append(flat, inner...)
		} else {
			flat = append(flat, v)
		}
	}
	seen := make(map[string]struct{}, len(flat))
	out := make([]*node, 0, len(flat))
	for _, v := range flat {
		key := nodeCanonJSON(v)
		if _, dup := seen[key]; dup {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, v)
	}
	sort.Slice(out, func(i, j int) bool { return nodeCanonJSON(out[i]) < nodeCanonJSON(out[j]) })
	return out
}

// collapse unwraps a single variant and merges a union of simple primitives into one type
// array. allOf is an intersection: it only unwraps a singleton.
func collapse(n *node) *node {
	// Unions (oneOf/anyOf) collapse via collapseUnion; otherwise n already carries
	// its canonical variants.
	if vs, ok := pureComposition(n, kindOneOf); ok {
		return collapseUnion(n, vs)
	}
	if vs, ok := pureComposition(n, kindAnyOf); ok {
		return collapseUnion(n, vs)
	}
	// allOf is an intersection: only a singleton unwraps; never merge simple variants.
	if vs, ok := pureComposition(n, kindAllOf); ok {
		if len(vs) == 1 {
			return vs[0]
		}
		return n
	}
	return n
}

func collapseUnion(n *node, variants []*node) *node {
	if len(variants) == 1 {
		return variants[0]
	}
	if merged, ok := mergeSimpleVariants(variants); ok {
		return merged
	}
	return n
}

// pureComposition returns the variants of s if s carries exactly the given
// composition keyword and no other type-constraining field, else (nil, false).
func pureComposition(s *node, kind compositionKind) ([]*node, bool) {
	if s == nil {
		return nil, false
	}
	if len(s.Type) > 0 || s.Properties != nil || s.AdditionalProperties != nil || s.Items != nil ||
		len(s.Required) > 0 || len(s.Enum) > 0 || s.Ref != "" {
		return nil, false
	}
	one, any, all := len(s.OneOf) > 0, len(s.AnyOf) > 0, len(s.AllOf) > 0
	switch kind {
	case kindOneOf:
		if one && !any && !all {
			return s.OneOf, true
		}
	case kindAnyOf:
		if any && !one && !all {
			return s.AnyOf, true
		}
	case kindAllOf:
		if all && !one && !any {
			return s.AllOf, true
		}
	}
	return nil, false
}

// mergeSimpleVariants merges a union of simple-primitive variants into one
// {type:[...]} node (sorted, deduped), or (nil, false) if any variant is not simple.
func mergeSimpleVariants(variants []*node) (*node, bool) {
	types := make([]string, 0, len(variants))
	for _, v := range variants {
		if !isSimpleType(v) {
			return nil, false
		}
		types = append(types, v.Type...)
	}
	return &node{Type: SchemaType(sortDedupStrings(types))}, true
}

// isSimpleType reports whether s is one or more primitive types with no other
// type-constraining fields — the shape mergeSimpleVariants can fold into a type
// array, including an already-merged multi-entry {type:[...]}.
func isSimpleType(s *node) bool {
	if s == nil || len(s.Type) == 0 {
		return false
	}
	return s.Properties == nil && s.AdditionalProperties == nil && s.Items == nil && len(s.Required) == 0 &&
		len(s.OneOf) == 0 && len(s.AnyOf) == 0 && len(s.AllOf) == 0 &&
		len(s.Enum) == 0 && s.Ref == ""
}

func sortDedupStrings(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	cp := append([]string(nil), in...)
	sort.Strings(cp)
	out := cp[:0]
	var last string
	for i, s := range cp {
		if i == 0 || s != last {
			out = append(out, s)
			last = s
		}
	}
	return out
}

func nodeCanonJSON(s *node) string {
	b, _ := json.Marshal(s)
	return string(b)
}

// nodeSize bounds the inference fixpoint by canonical JSON length. An unmarshalable schema (a
// ref cycle) counts as infinite, so the bound fails loudly rather than masking it.
func nodeSize(s *node) int {
	b, err := json.Marshal(canonicalizeNode(s))
	if err != nil {
		return math.MaxInt
	}
	return len(b)
}

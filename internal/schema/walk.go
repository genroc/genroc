package schema

import (
	"iter"
	"maps"
	"slices"
)

// slotKind is what a child position means to a walk; every structural walk steers on it.
type slotKind uint8

const (
	slotNested slotKind = iota // properties/items/additionalProperties: consumes a value level
	slotBare                   // union arms: same depth, so a $ref here makes no progress
	slotDefs                   // $defs: a namespace, not a position in the value
)

// childSlot names one position where a sub-schema lives.
type childSlot struct {
	kw   string // the JSON Schema keyword
	key  string // property or $defs name; "" elsewhere
	idx  int    // index within a list slot; -1 elsewhere
	kind slotKind
}

// mapChildren is the one definition of where sub-schemas live: add a keyword HERE. fn recurses
// itself and sees nil children; a nil result drops a list entry, clears a single slot, keeps a
// map key. Map slots visit in sorted order, so a first-error walk is deterministic.
func mapChildren(n *node, fn func(childSlot, *node) *node) *node {
	m := *n
	if n.Properties != nil {
		m.Properties = mapKeyed(n.Properties, "properties", slotNested, fn)
	}
	if n.Items != nil {
		m.Items = fn(childSlot{kw: "items", idx: -1, kind: slotNested}, n.Items)
	}
	if n.AdditionalProperties != nil {
		m.AdditionalProperties = fn(
			childSlot{kw: "additionalProperties", idx: -1, kind: slotNested},
			n.AdditionalProperties,
		)
	}
	m.OneOf = mapList(n.OneOf, "oneOf", fn)
	m.AnyOf = mapList(n.AnyOf, "anyOf", fn)
	m.AllOf = mapList(n.AllOf, "allOf", fn)
	if n.Defs != nil {
		m.Defs = mapKeyed(n.Defs, "$defs", slotDefs, fn)
	}
	return &m
}

func mapKeyed(in map[string]*node, kw string, kind slotKind, fn func(childSlot, *node) *node) map[string]*node {
	out := make(map[string]*node, len(in))
	for _, k := range slices.Sorted(maps.Keys(in)) {
		out[k] = fn(childSlot{kw: kw, key: k, idx: -1, kind: kind}, in[k])
	}
	return out
}

func mapList(in []*node, kw string, fn func(childSlot, *node) *node) []*node {
	if in == nil {
		return nil
	}
	out := make([]*node, 0, len(in))
	for i, v := range in {
		if r := fn(childSlot{kw: kw, idx: i, kind: slotBare}, v); r != nil {
			out = append(out, r)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// children yields every direct sub-schema with its slot, for walks that read rather than
// rewrite. Derived from mapChildren rather than written beside it, so the two cannot
// drift; the copy it builds is discarded.
func children(n *node) iter.Seq2[childSlot, *node] {
	return func(yield func(childSlot, *node) bool) {
		if n == nil {
			return
		}
		stopped := false
		mapChildren(n, func(sl childSlot, c *node) *node {
			if !stopped && !yield(sl, c) {
				stopped = true
			}
			return c
		})
	}
}

package schema

import (
	"iter"
	"maps"
	"slices"
)

// slotKind classifies a child position by what it means to a walk — the axis every
// structural walk in this package steers on:
//
//	slotNested — properties/items/additionalProperties: consumes a level of the value.
//	slotBare   — oneOf/anyOf/allOf arms: the value stays at this depth, so a $ref here
//	             is recursion with no structural progress (see checkProductivity).
//	slotDefs   — $defs: a definitions namespace, not a position in the value at all.
type slotKind uint8

const (
	slotNested slotKind = iota
	slotBare
	slotDefs
)

// childSlot names one position where a sub-schema lives.
type childSlot struct {
	kw   string // the JSON Schema keyword
	key  string // property or $defs name; "" elsewhere
	idx  int    // index within a list slot; -1 elsewhere
	kind slotKind
}

// mapChildren applies fn to every direct sub-schema of n and returns a copy carrying the
// results; n is not modified. It is the single definition of where sub-schemas live — a
// new sub-schema keyword is added HERE and every structural walk picks it up, which is
// the point: five walks used to enumerate the keywords independently and a missed one
// failed silently (an unstripped $defs cycles the marshaler, an uncanonicalized subtree
// stops the inference fixpoint converging).
//
// fn drives its own recursion and steers by slot — `if sl.kind != slotBare { return c }`
// leaves a subtree alone. It sees nil children, which only malformed input has (checkDoc
// rejects them). A nil result drops the entry from a list slot and clears a single-valued
// one, but keeps a map key with a nil value; that is what each walk did by hand.
//
// Map slots are visited in sorted key order, so a walk that accumulates or reports the
// first error is deterministic without having to say so.
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

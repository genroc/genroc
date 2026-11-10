package schema

import (
	"maps"
	"slices"
	"sort"
	"strings"
)

// nodesEqual reports whether a and b denote the same type, compared in canonical form.
func nodesEqual(a, b *node) bool {
	return nodeCanonJSON(canonicalizeNode(a)) == nodeCanonJSON(canonicalizeNode(b))
}

// joinNodes is the least upper bound: objects merge per property (a one-sided key nullable),
// anything else unions. The canonical result gives the fixpoint a monotone, terminating step.
func joinNodes(a, b *node) *node {
	if a == nil {
		return canonicalizeNode(b)
	}
	if b == nil {
		return canonicalizeNode(a)
	}
	if nodesEqual(a, b) {
		return canonicalizeNode(a)
	}
	// A pure-null operand only contributes nullability; stripping it would leave an
	// empty schema, so handle it directly: join(x, null) = x made nullable.
	if isNullType(a) {
		return canonicalizeNode(withNull(b))
	}
	if isNullType(b) {
		return canonicalizeNode(withNull(a))
	}

	nullable := hasNullType(a) || hasNullType(b)
	na, nb := stripNull(a), stripNull(b)

	var res *node
	switch {
	case nodesEqual(na, nb):
		// The inputs differed only in nullability (e.g. anyOf[$ref x, null] vs
		// $ref x) — no union needed, just restore the null below.
		res = na
	case isObjectType(na) && isObjectType(nb):
		res = joinObjects(na, nb)
	default:
		res = &node{OneOf: []*node{na, nb}}
	}
	if nullable {
		res = withNull(res)
	}
	return canonicalizeNode(res)
}

func isObjectType(s *node) bool {
	return s != nil && (s.Type.Contains("object") || s.Properties != nil || s.AdditionalProperties != nil)
}

// joinObjects merges two object schemas property-wise: a key on both is joined; a key
// on only one side is kept but made nullable; a property is required only when both
// sides require it.
func joinObjects(a, b *node) *node {
	keys := make(map[string]struct{}, len(a.Properties)+len(b.Properties))
	for k := range a.Properties {
		keys[k] = struct{}{}
	}
	for k := range b.Properties {
		keys[k] = struct{}{}
	}

	props := make(map[string]*node, len(keys))
	for k := range keys {
		av, bv := a.Properties[k], b.Properties[k]
		switch {
		case av == nil:
			props[k] = withNull(canonicalizeNode(bv))
		case bv == nil:
			props[k] = withNull(canonicalizeNode(av))
		default:
			props[k] = joinNodes(av, bv)
		}
	}

	var required []string
	for k := range keys {
		if a.Properties[k] != nil && b.Properties[k] != nil && isRequired(a, k) && isRequired(b, k) {
			required = append(required, k)
		}
	}
	sort.Strings(required)

	out := &node{Type: SchemaType{"object"}, Properties: props}
	if len(required) > 0 {
		out.Required = required
	}
	// Open-map keys: join both sides' additionalProperties, or carry the one side
	// that has it (the join permits those extras, so it widens rather than closes).
	switch {
	case a.AdditionalProperties != nil && b.AdditionalProperties != nil:
		out.AdditionalProperties = joinNodes(a.AdditionalProperties, b.AdditionalProperties)
	case a.AdditionalProperties != nil:
		out.AdditionalProperties = canonicalizeNode(a.AdditionalProperties)
	case b.AdditionalProperties != nil:
		out.AdditionalProperties = canonicalizeNode(b.AdditionalProperties)
	}
	return out
}

// WithoutSubsumedArms drops every oneOf arm another arm covers, a `$ref` arm judged by what it
// names through pool: a value two arms take is one oneOf refuses. Only after Solve — a solver
// sentinel reads as the top type and would cover every arm.
func (s Schema) WithoutSubsumedArms(pool Defs) Schema {
	p := pruner{defs: pool.m, cyclic: reachesCycle(pool.m)}
	return s.keepingContext(wrap(canonicalizeNode(p.prune(s.n)), s.rootDefs()))
}

type pruner struct {
	defs map[string]*node
	// cyclic names every definition that reaches a reference cycle. An arm reaching one is never
	// compared: two members of a cycle each cover the other's base case, so dropping both
	// leaves a cycle with none (TestWithoutSubsumedArms_LeavesAReferenceCycleAlone).
	cyclic map[string]bool
}

func (p pruner) prune(n *node) *node {
	if n == nil {
		return nil
	}
	m := mapChildren(n, func(sl childSlot, c *node) *node {
		if sl.kind == slotDefs {
			return c
		}
		return p.prune(c)
	})
	if len(m.OneOf) < 2 {
		return m
	}
	var kept []*node
	// Inline arms first: of two naming one type, the one that reads as itself survives.
	for _, refs := range []bool{false, true} {
		for _, a := range m.OneOf {
			if (a.Ref != "") != refs || slices.ContainsFunc(kept, func(k *node) bool { return p.covers(k, a) }) {
				continue
			}
			kept = slices.DeleteFunc(kept, func(k *node) bool { return p.covers(a, k) })
			kept = append(kept, a)
		}
	}
	m.OneOf = kept
	return m
}

// covers: every value of sub is one of super's, and conforming it there strips nothing.
func (p pruner) covers(super, sub *node) bool {
	if p.reaches(super) || p.reaches(sub) {
		return false
	}
	ctx := &subsetCtx{subDefs: p.defs, superDefs: p.defs, visiting: map[string]bool{}, subsetMode: subsetMode{closed: true}}
	return ctx.check(sub, super, nil)
}

func (p pruner) reaches(n *node) bool {
	refs := map[string]bool{}
	collectRefs(n, refs)
	for name := range refs {
		if p.cyclic[name] {
			return true
		}
	}
	return false
}

// collectRefs gathers the $defs names nd references in any slot, without following them.
func collectRefs(nd *node, out map[string]bool) {
	if nd == nil {
		return
	}
	if name, ok := strings.CutPrefix(nd.Ref, "#/$defs/"); ok {
		out[name] = true
	}
	for sl, c := range children(nd) {
		if sl.kind != slotDefs {
			collectRefs(c, out)
		}
	}
}

// reachesCycle names each definition from which a reference cycle is reachable, its own
// included. tarjanSCC yields dependencies first, so one pass settles every name.
func reachesCycle(defs map[string]*node) map[string]bool {
	graph := make(map[string][]string, len(defs))
	for name, d := range defs {
		refs := map[string]bool{}
		collectRefs(d, refs)
		for r := range refs {
			if _, ok := defs[r]; ok {
				graph[name] = append(graph[name], r)
			}
		}
		slices.Sort(graph[name])
	}
	out := map[string]bool{}
	for _, scc := range tarjanSCC(graph, slices.Sorted(maps.Keys(defs))) {
		hit := len(scc) > 1 || slices.Contains(graph[scc[0]], scc[0])
		for _, n := range scc {
			for _, e := range graph[n] {
				hit = hit || out[e]
			}
		}
		for _, n := range scc {
			out[n] = hit
		}
	}
	return out
}

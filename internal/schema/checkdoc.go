package schema

import (
	"fmt"
	"sort"
	"strings"
)

// checkDocRoot: well-formedness in the supported subset — refs resolve, combinator entries
// non-nil, paired bounds ordered, defaults validate, cycles productive. Keyword validity is
// strict-decode's job; this catches structure that survives parsing.
func checkDocRoot(nd *node) error {
	if nd == nil {
		return nil
	}
	if err := checkDoc(nd, nd.Defs, map[*node]bool{}); err != nil {
		return err
	}
	return checkProductivity(nd.Defs)
}

// checkProductivity rejects definition cycles with no structural progress: a $ref outside
// any properties/items subtree is a bare edge, and an all-bare cycle is recursion with no
// base case. Legal recursion consumes one level of the finite value per unrolling.
func checkProductivity(defs map[string]*node) error {
	if len(defs) == 0 {
		return nil
	}
	bare := make(map[string][]string, len(defs))
	names := make([]string, 0, len(defs))
	for name, d := range defs {
		names = append(names, name)
		set := map[string]struct{}{}
		collectBareRefs(d, set)
		edges := make([]string, 0, len(set))
		for e := range set {
			if _, ok := defs[e]; ok {
				edges = append(edges, e)
			}
		}
		sort.Strings(edges)
		bare[name] = edges
	}
	sort.Strings(names)

	const unvisited, onStack, done = 0, 1, 2
	state := make(map[string]int, len(defs))
	var stack []string
	var visit func(n string) error
	visit = func(n string) error {
		switch state[n] {
		case onStack:
			i := len(stack) - 1
			for i >= 0 && stack[i] != n {
				i--
			}
			cycle := append(append([]string{}, stack[i:]...), n)
			return fmt.Errorf("$defs cycle without structural progress: %s (recursion must pass through properties or items)",
				strings.Join(cycle, " -> "))
		case done:
			return nil
		}
		state[n] = onStack
		stack = append(stack, n)
		for _, e := range bare[n] {
			if err := visit(e); err != nil {
				return err
			}
		}
		stack = stack[:len(stack)-1]
		state[n] = done
		return nil
	}
	for _, n := range names {
		if err := visit(n); err != nil {
			return err
		}
	}
	return nil
}

// collectBareRefs gathers the $defs names referenced from nd without passing through
// properties or items — exactly the slotBare positions, since a ref below a level-
// consuming slot is productive.
func collectBareRefs(nd *node, out map[string]struct{}) {
	if nd == nil {
		return
	}
	const prefix = "#/$defs/"
	if strings.HasPrefix(nd.Ref, prefix) {
		out[strings.TrimPrefix(nd.Ref, prefix)] = struct{}{}
	}
	for sl, c := range children(nd) {
		if sl.kind == slotBare {
			collectBareRefs(c, out)
		}
	}
}

func checkDoc(nd *node, defs map[string]*node, seen map[*node]bool) error {
	if nd == nil || seen[nd] {
		return nil
	}
	seen[nd] = true

	if nd.Ref != "" {
		if _, err := deref(nd, defs); err != nil {
			return err
		}
	}
	// A type name outside the simpleTypes enum matches nothing — it used to parse cleanly and
	// reject every value at runtime. A validity rule, NOT a decode rule: the decoder runs over
	// stored schemas, and a legacy bad name must fail its registration, not become undecodable.
	for _, t := range nd.Type {
		if !validTypes[t] {
			return fmt.Errorf("unsupported schema type %q", t)
		}
	}
	if nd.Minimum != nil && nd.Maximum != nil && *nd.Minimum > *nd.Maximum {
		return fmt.Errorf("minimum %v exceeds maximum %v", *nd.Minimum, *nd.Maximum)
	}
	if nd.MinLength != nil && nd.MaxLength != nil && *nd.MinLength > *nd.MaxLength {
		return fmt.Errorf("minLength %d exceeds maxLength %d", *nd.MinLength, *nd.MaxLength)
	}
	if nd.MinItems != nil && nd.MaxItems != nil && *nd.MinItems > *nd.MaxItems {
		return fmt.Errorf("minItems %d exceeds maxItems %d", *nd.MinItems, *nd.MaxItems)
	}
	// An invalid default would surface only when Validate fills it into an
	// absent property; reject it here, where the error points at the schema.
	if nd.Default != nil {
		if _, err := conform(nd, defs, nd.Default, ""); err != nil {
			return fmt.Errorf("default does not validate against its schema: %w", err)
		}
	}

	for sl, c := range children(nd) {
		if c == nil {
			if err := nullChildErr(sl); err != nil {
				return err
			}
			continue
		}
		if err := checkDoc(c, defs, seen); err != nil {
			return fmt.Errorf("%s: %w", errLabel(sl), err)
		}
	}
	return nil
}

// nullChildErr rejects a null sub-schema where one is meaningless. items and
// additionalProperties cannot reach here null (children yields them only when set), and a
// null $defs entry has always been tolerated.
func nullChildErr(sl childSlot) error {
	switch sl.kw {
	case "properties":
		return fmt.Errorf("property %q is null", sl.key)
	case "oneOf", "anyOf", "allOf":
		return fmt.Errorf("%s is null", errLabel(sl))
	}
	return nil
}

// errLabel names a slot when wrapping a child's error. Chained by the recursion, these
// spell the failing node's location in the document ("$defs.Foo: rows: items: oneOf[2]"),
// which is why a union arm carries its index: every variant must be well-formed
// independently, so the index IS the location, and without it a bad variant reports as
// though the parent were at fault.
func errLabel(sl childSlot) string {
	switch sl.kw {
	case "properties":
		return sl.key
	case "$defs":
		return "$defs." + sl.key
	case "oneOf", "anyOf", "allOf":
		return fmt.Sprintf("%s[%d]", sl.kw, sl.idx)
	}
	return sl.kw
}

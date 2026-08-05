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
// properties or items. Union variants keep the value at the same depth, so they are
// walked; a ref below properties/items is productive and skipped.
func collectBareRefs(nd *node, out map[string]struct{}) {
	if nd == nil {
		return
	}
	const prefix = "#/$defs/"
	if strings.HasPrefix(nd.Ref, prefix) {
		out[strings.TrimPrefix(nd.Ref, prefix)] = struct{}{}
	}
	for _, v := range nd.OneOf {
		collectBareRefs(v, out)
	}
	for _, v := range nd.AnyOf {
		collectBareRefs(v, out)
	}
	for _, v := range nd.AllOf {
		collectBareRefs(v, out)
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

	for name, p := range nd.Properties {
		if p == nil {
			return fmt.Errorf("property %q is null", name)
		}
		if err := checkDoc(p, defs, seen); err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
	}
	if err := checkDoc(nd.Items, defs, seen); err != nil {
		return fmt.Errorf("items: %w", err)
	}
	if err := checkDoc(nd.AdditionalProperties, defs, seen); err != nil {
		return fmt.Errorf("additionalProperties: %w", err)
	}
	for i, v := range nd.OneOf {
		if v == nil {
			return fmt.Errorf("oneOf[%d] is null", i)
		}
		if err := checkDoc(v, defs, seen); err != nil {
			return err
		}
	}
	for i, v := range nd.AnyOf {
		if v == nil {
			return fmt.Errorf("anyOf[%d] is null", i)
		}
		if err := checkDoc(v, defs, seen); err != nil {
			return err
		}
	}
	for name, d := range nd.Defs {
		if err := checkDoc(d, defs, seen); err != nil {
			return fmt.Errorf("$defs.%s: %w", name, err)
		}
	}
	return nil
}

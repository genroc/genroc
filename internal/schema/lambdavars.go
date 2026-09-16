package schema

import "genroc/internal/expression/syntax"

// LambdaVars types every lambda parameter an expression binds — the scope a reader inside that
// body writes in, which the slot's own context does not carry. Pair it with WithVars to type a
// fragment of the body on its own; whole-expression inference binds these for itself.
//
// Two names are dropped rather than guessed at: one two lambdas bind, and one that also names a
// context root. Nodes carry no offsets (specs/language-server.md §6), so a caller holding a
// cursor cannot say which binder it is under, and a wrong type is worse than none.
func (s Schema) LambdaVars(expression string) map[string]Schema {
	node, err := syntax.Parse(expression)
	if err != nil {
		return nil
	}
	bound := map[string]int{}
	countParams(node, bound)

	arms := s.contextStates()
	if len(arms) == 0 {
		arms = []Schema{s}
	}
	out := map[string]Schema{}
	for _, arm := range arms {
		collectParams(node, inferCtx{s: arm, guards: s.guards, vars: s.vars}, bound, out)
	}
	for name := range out {
		for _, arm := range arms {
			if _, err := arm.Property(name); err == nil {
				delete(out, name)
				break
			}
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func countParams(node syntax.Node, bound map[string]int) {
	if lam, ok := node.(*syntax.LambdaNode); ok {
		bound[lam.Param]++
		if lam.IndexParam != "" {
			bound[lam.IndexParam]++
		}
	}
	for _, c := range operands(node) {
		countParams(c, bound)
	}
}

// collectParams walks under the same bindings inference builds, so the two cannot disagree about
// what a parameter is: a map's source types in the scope around it, its body in that scope plus
// the parameters. A source that does not type binds nothing — an element guessed from a broken
// source is fiction, and the body's own scope is unknowable anyway.
func collectParams(node syntax.Node, ictx inferCtx, bound map[string]int, out map[string]Schema) {
	call, ok := node.(*syntax.CallNode)
	if !ok || call.Name != "map" || len(call.Args) != 2 {
		for _, c := range operands(node) {
			collectParams(c, ictx, bound, out)
		}
		return
	}
	lam, ok := call.Args[1].(*syntax.LambdaNode)
	if !ok {
		return
	}
	collectParams(call.Args[0], ictx, bound, out)
	elem, err := mapElement(call, ictx)
	if err != nil {
		return
	}
	record(out, bound, lam.Param, elem)
	if lam.IndexParam != "" {
		record(out, bound, lam.IndexParam, Type("integer"))
	}
	collectParams(lam.Body, ictx.withParams(lam, elem), bound, out)
}

// record joins what another context arm already said about the same parameter: a union context
// is alternative states, and the parameter has a type in each.
func record(out map[string]Schema, bound map[string]int, name string, t Schema) {
	if bound[name] > 1 {
		return
	}
	if prev, ok := out[name]; ok {
		t = prev.Join(t)
	}
	out[name] = t
}

// operands returns a node's sub-expressions. A lambda's body is one of them, so a caller that
// binds parameters handles CallNode itself before falling back here.
func operands(node syntax.Node) []syntax.Node {
	switch n := node.(type) {
	case *syntax.MemberNode:
		return []syntax.Node{n.Base}
	case *syntax.IndexNode:
		return []syntax.Node{n.Base}
	case *syntax.KeyNode:
		return []syntax.Node{n.Base, n.Key}
	case *syntax.ArrayNode:
		return n.Items
	case *syntax.ObjectNode:
		return n.Values
	case *syntax.LambdaNode:
		return []syntax.Node{n.Body}
	case *syntax.CallNode:
		return n.Args
	case *syntax.UnaryNode:
		return []syntax.Node{n.Operand}
	case *syntax.BinaryNode:
		return []syntax.Node{n.Left, n.Right}
	case *syntax.CondNode:
		return []syntax.Node{n.Cond, n.Then, n.Else}
	}
	return nil
}

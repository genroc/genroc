package expression

import (
	"sort"

	"genroc/internal/expression/syntax"
)

// OutputRefsNode returns the distinct task ids referenced via outputs.<id> in an
// already-parsed expression — the edges of the output-dependency graph used for ordering
// and recursion detection.
func OutputRefsNode(node syntax.Node) []string {
	set := map[string]struct{}{}
	collectOutputRefs(node, nil, set)
	ids := make([]string, 0, len(set))
	for id := range set {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

// Roots describes which top-level context roots an expression reads. Used by the
// engine to resolve only the externalized value-slots an expression actually needs
// (slot-level lazy loading) instead of materializing every big value every tick.
type Roots struct {
	Input        bool     // reads the process input
	Error        bool     // reads the error namespace
	Outputs      []string // reads outputs.<id> for these specific task ids
	AllOutputs   bool     // reads the outputs map in a way we can't pin to static ids
	SelfPrevious bool     // reads self.previous (this task's own prior output — an alias
	//                      for outputs[<this task>], so it can be an externalized ref too)
	SelfResult bool // reads self.result (the task's raw action result)
	// ErrorData pins `error.data` specifically. The rest of `error` is small and always
	// inline, but the body can be as large as any response, so it is externalized like a task
	// output — and a handler reading only error.code must not pay to load one.
	ErrorData bool
}

func RootRefs(expr string) (Roots, error) {
	node, err := syntax.Parse(expr)
	if err != nil {
		return Roots{}, err
	}
	return RootRefsNode(node), nil
}

// RootRefsNode is RootRefs over an already-parsed expression.
func RootRefsNode(node syntax.Node) Roots {
	var r Roots
	collectRoots(node, nil, &r)
	return r
}

// bind extends bound with a lambda's parameters, which shadow context roots. Over-report
// is waste; UNDER-report makes the engine serve nil for an externalized slot — so the
// shadowing must be exact in both directions.
func bindParams(bound map[string]bool, lam *syntax.LambdaNode) map[string]bool {
	next := make(map[string]bool, len(bound)+2)
	for k := range bound {
		next[k] = true
	}
	next[lam.Param] = true
	if lam.IndexParam != "" {
		next[lam.IndexParam] = true
	}
	return next
}

func collectOutputRefs(node syntax.Node, bound map[string]bool, set map[string]struct{}) {
	switch n := node.(type) {
	case *syntax.MemberNode:
		if id := outputRefID(n, bound); id != "" {
			set[id] = struct{}{}
		}
		collectOutputRefs(n.Base, bound, set)
	case *syntax.IndexNode:
		collectOutputRefs(n.Base, bound, set)
	case *syntax.KeyNode:
		collectOutputRefs(n.Base, bound, set)
		collectOutputRefs(n.Key, bound, set)
	case *syntax.ArrayNode:
		for _, item := range n.Items {
			collectOutputRefs(item, bound, set)
		}
	case *syntax.ObjectNode:
		for _, v := range n.Values {
			collectOutputRefs(v, bound, set)
		}
	case *syntax.CallNode:
		for _, a := range n.Args {
			collectOutputRefs(a, bound, set)
		}
	case *syntax.LambdaNode:
		collectOutputRefs(n.Body, bindParams(bound, n), set)
	case *syntax.BinaryNode:
		collectOutputRefs(n.Left, bound, set)
		collectOutputRefs(n.Right, bound, set)
	case *syntax.UnaryNode:
		collectOutputRefs(n.Operand, bound, set)
	case *syntax.CondNode:
		collectOutputRefs(n.Cond, bound, set)
		collectOutputRefs(n.Then, bound, set)
		collectOutputRefs(n.Else, bound, set)
	}
}

func collectRoots(node syntax.Node, bound map[string]bool, r *Roots) {
	switch n := node.(type) {
	case *syntax.MemberNode:
		if id := outputRefID(n, bound); id != "" {
			r.Outputs = append(r.Outputs, id)
			return // consumed outputs.<id>; don't descend into the "outputs" identifier
		}
		if isSelfField(n, bound, "previous") {
			r.SelfPrevious = true
			return // consumed self.previous; don't descend into the "self" identifier
		}
		if isSelfField(n, bound, "result") {
			r.SelfResult = true
			return // consumed self.result; don't descend into the "self" identifier
		}
		// A field access on `error` is consumed here so the bare-identifier case below does
		// not also run: only `error.data` wants the body loaded, while `error.code` and its
		// siblings are inline. A BARE `error` falls through to the identifier case, which
		// takes the whole namespace — under-reporting there would export a marker.
		if base, ok := n.Base.(*syntax.IdentNode); ok && base.Name == "error" && !bound["error"] {
			r.Error = true
			r.ErrorData = r.ErrorData || n.Name == "data"
			return
		}
		collectRoots(n.Base, bound, r)
	case *syntax.IndexNode:
		collectRoots(n.Base, bound, r)
	case *syntax.KeyNode:
		// The base of a computed key is never the `outputs.<id>` shape, so it falls
		// through to the bare-identifier case and marks AllOutputs — which is what
		// `outputs[k]` needs: the id is not known until the expression runs.
		collectRoots(n.Base, bound, r)
		collectRoots(n.Key, bound, r)
	case *syntax.IdentNode:
		if bound[n.Name] {
			return // a lambda parameter, not a context root
		}
		switch n.Name {
		case "input":
			r.Input = true
		case "error":
			// The whole namespace, body included: this is reached only when `error` is used
			// without naming a field, so nothing narrows what is wanted.
			r.Error, r.ErrorData = true, true
		case "outputs":
			r.AllOutputs = true // bare/dynamic outputs reference
		}
	case *syntax.ArrayNode:
		for _, item := range n.Items {
			collectRoots(item, bound, r)
		}
	case *syntax.ObjectNode:
		for _, v := range n.Values {
			collectRoots(v, bound, r)
		}
	case *syntax.CallNode:
		for _, a := range n.Args {
			collectRoots(a, bound, r)
		}
	case *syntax.LambdaNode:
		collectRoots(n.Body, bindParams(bound, n), r)
	case *syntax.BinaryNode:
		collectRoots(n.Left, bound, r)
		collectRoots(n.Right, bound, r)
	case *syntax.UnaryNode:
		collectRoots(n.Operand, bound, r)
	case *syntax.CondNode:
		collectRoots(n.Cond, bound, r)
		collectRoots(n.Then, bound, r)
		collectRoots(n.Else, bound, r)
	}
}

// isSelfField reports whether n is exactly self.<field>, with self unshadowed. A
// deeper self.previous.x is a MemberNode whose Base is this node, so the walkers
// still reach it.
func isSelfField(n *syntax.MemberNode, bound map[string]bool, field string) bool {
	base, ok := n.Base.(*syntax.IdentNode)
	return ok && base.Name == "self" && !bound["self"] && n.Name == field
}

// outputRefID returns <id> when n is exactly outputs.<id>, else "".
func outputRefID(n *syntax.MemberNode, bound map[string]bool) string {
	base, ok := n.Base.(*syntax.IdentNode)
	if !ok || base.Name != "outputs" || bound["outputs"] {
		return ""
	}
	return n.Name
}

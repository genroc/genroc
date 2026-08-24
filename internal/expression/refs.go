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

	// Through marks the roots the expression reads INTO or operates on, as against the ones it
	// merely COPIES. A copied root can stay an *ObjectRef: the value flows into the result and
	// on into the next write as the reference it already was, never loaded. One read through
	// must be materialized, or the read finds a marker where the data should be.
	//
	// Same conservatism as Roots itself and for a sharper reason: over-reporting costs a load,
	// under-reporting hands a marker to an operation. specs/lazy-context.md.
	Through Through
}

// Through is the subset of Roots that must be materialized rather than left as references.
type Through struct {
	Input, Error, ErrorData, AllOutputs, SelfPrevious, SelfResult bool
	Outputs                                                       []string
}

// Union merges o into r. One place, so a new field cannot be forgotten at an aggregation site
// (shape.Roots and Template.RootRefs both union, and both used to do it field by field).
func (r *Roots) Union(o Roots) {
	r.Input = r.Input || o.Input
	r.Error = r.Error || o.Error
	r.AllOutputs = r.AllOutputs || o.AllOutputs
	r.Outputs = append(r.Outputs, o.Outputs...)
	r.SelfPrevious = r.SelfPrevious || o.SelfPrevious
	r.SelfResult = r.SelfResult || o.SelfResult
	r.ErrorData = r.ErrorData || o.ErrorData
	r.Through.Input = r.Through.Input || o.Through.Input
	r.Through.Error = r.Through.Error || o.Through.Error
	r.Through.AllOutputs = r.Through.AllOutputs || o.Through.AllOutputs
	r.Through.Outputs = append(r.Through.Outputs, o.Through.Outputs...)
	r.Through.SelfPrevious = r.Through.SelfPrevious || o.Through.SelfPrevious
	r.Through.SelfResult = r.Through.SelfResult || o.Through.SelfResult
	r.Through.ErrorData = r.Through.ErrorData || o.Through.ErrorData
}

func RootRefs(expr string) (Roots, error) {
	node, err := syntax.Parse(expr)
	if err != nil {
		return Roots{}, err
	}
	return RootRefsNode(node, false), nil
}

// RootRefsNode is RootRefs over an already-parsed expression. through says whether the
// expression's RESULT is operated on rather than taken as a value -- true for a ${ }
// interpolation, which stringifies it; false for `$:`, which hands the value through.
func RootRefsNode(node syntax.Node, through bool) Roots {
	var r Roots
	collectRoots(node, nil, &r, through)
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

// collectRoots walks the expression recording which roots it names and, in Through, which of
// them it reads INTO. A root is read through when the walk descends from a navigation (a field,
// an index, a computed key), an operator, or a call argument; it is merely copied when it sits
// in an array item, an object value, or a conditional branch -- positions whose value is placed
// somewhere, not inspected.
func collectRoots(node syntax.Node, bound map[string]bool, r *Roots, through bool) {
	switch n := node.(type) {
	case *syntax.MemberNode:
		if id := outputRefID(n, bound); id != "" {
			r.Outputs = append(r.Outputs, id)
			if through {
				r.Through.Outputs = append(r.Through.Outputs, id)
			}
			return // consumed outputs.<id>; don't descend into the "outputs" identifier
		}
		if isSelfField(n, bound, "previous") {
			r.SelfPrevious = true
			r.Through.SelfPrevious = r.Through.SelfPrevious || through
			return // consumed self.previous; don't descend into the "self" identifier
		}
		if isSelfField(n, bound, "result") {
			r.SelfResult = true
			r.Through.SelfResult = r.Through.SelfResult || through
			return // consumed self.result; don't descend into the "self" identifier
		}
		// A field access on `error` is consumed here so the bare-identifier case below does
		// not also run: only `error.data` wants the body loaded, while `error.code` and its
		// siblings are inline. A BARE `error` falls through to the identifier case, which
		// takes the whole namespace — under-reporting there would export a marker.
		if base, ok := n.Base.(*syntax.IdentNode); ok && base.Name == "error" && !bound["error"] {
			r.Error = true
			r.Through.Error = true // the namespace itself is navigated: `error.<field>`
			if n.Name == "data" {
				r.ErrorData = true
				r.Through.ErrorData = r.Through.ErrorData || through
			}
			return
		}
		// Navigating into the base reads THROUGH it, whatever position this node is in.
		collectRoots(n.Base, bound, r, true)
	case *syntax.IndexNode:
		collectRoots(n.Base, bound, r, true)
	case *syntax.KeyNode:
		// The base of a computed key is never the `outputs.<id>` shape, so it falls
		// through to the bare-identifier case and marks AllOutputs — which is what
		// `outputs[k]` needs: the id is not known until the expression runs.
		collectRoots(n.Base, bound, r, true)
		collectRoots(n.Key, bound, r, true)
	case *syntax.IdentNode:
		if bound[n.Name] {
			return // a lambda parameter, not a context root
		}
		switch n.Name {
		case "input":
			r.Input = true
			r.Through.Input = r.Through.Input || through
		case "error":
			// The whole namespace, body included: this is reached only when `error` is used
			// without naming a field, so nothing narrows what is wanted.
			r.Error, r.ErrorData = true, true
			r.Through.Error = r.Through.Error || through
			r.Through.ErrorData = r.Through.ErrorData || through
		case "outputs":
			r.AllOutputs = true // bare/dynamic outputs reference
			r.Through.AllOutputs = r.Through.AllOutputs || through
		}
	case *syntax.ArrayNode:
		// An item's value is placed in the array, not inspected -- a copy position.
		for _, item := range n.Items {
			collectRoots(item, bound, r, false)
		}
	case *syntax.ObjectNode:
		for _, v := range n.Values {
			collectRoots(v, bound, r, false)
		}
	case *syntax.CallNode:
		// A function inspects its arguments. Conservative: the callee is not analysed, and
		// over-reporting costs a load where under-reporting hands it a marker.
		for _, a := range n.Args {
			collectRoots(a, bound, r, true)
		}
	case *syntax.LambdaNode:
		collectRoots(n.Body, bindParams(bound, n), r, through)
	case *syntax.BinaryNode:
		collectRoots(n.Left, bound, r, true)
		collectRoots(n.Right, bound, r, true)
	case *syntax.UnaryNode:
		collectRoots(n.Operand, bound, r, true)
	case *syntax.CondNode:
		collectRoots(n.Cond, bound, r, true)
		// The chosen branch becomes this node's value, so it inherits this node's position.
		collectRoots(n.Then, bound, r, through)
		collectRoots(n.Else, bound, r, through)
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

// Static type inference for the expression language: it must accept exactly the constructs
// internal/expression's Eval does.
package schema

import (
	"errors"
	"fmt"
	"slices"
	"sort"

	"genroc/internal/expression/syntax"
)

// ErrUnsupported is returned when an expression uses a construct outside the
// supported subset. internal/expression aliases it so inference and evaluation
// report the same error type.
type ErrUnsupported struct{ Detail string }

func (e ErrUnsupported) Error() string {
	return "unsupported expression: " + e.Detail
}

// inferCtx is immutable: withGuard/withParams copy the maps. vars (lambda params) shadow the
// context's roots.
type inferCtx struct {
	s      Schema
	guards map[string]guard
	vars   map[string]Schema
}

// guard is one narrowed path. roots holds every identifier it depends on — a computed key adds
// its own — so a lambda shadowing any drops the entry: `m[k]` is void once `k` rebinds.
type guard struct {
	roots []string
	s     Schema
}

func (c inferCtx) withGuard(steps []pathStep, narrowed Schema) inferCtx {
	guards := make(map[string]guard, len(c.guards)+1)
	for k, v := range c.guards {
		guards[k] = v
	}
	guards[guardKey(steps)] = guard{roots: stepRoots(steps), s: narrowed}
	return inferCtx{s: c.s, guards: guards, vars: c.vars}
}

// stepRoots collects the identifiers a path is rooted at: its own leading name,
// plus the leading name of every computed key nested inside it.
func stepRoots(steps []pathStep) []string {
	var roots []string
	for i, st := range steps {
		if i == 0 && st.kind == stepProp {
			roots = append(roots, st.prop)
		}
		if st.kind == stepKey {
			roots = append(roots, stepRoots(st.key)...)
		}
	}
	return roots
}

// guardKey uses the shared path rendering, injective because a bracket-needing key never
// renders dotted: x["a.b"] and x.a.b are different paths, and collapsing them would
// narrow whichever the author did not write.
func guardKey(steps []pathStep) string { return renderPath(steps) }

// identKey is guardKey for a bare identifier — the root of every guarded path.
func identKey(name string) string { return JoinPath("", name) }

// withParams drops guards rooted at a name the lambda shadows: they say nothing about the
// parameter that now owns it.
func (c inferCtx) withParams(lam *syntax.LambdaNode, elem Schema) inferCtx {
	vars := make(map[string]Schema, len(c.vars)+2)
	for k, v := range c.vars {
		vars[k] = v
	}
	vars[lam.Param] = elem
	if lam.IndexParam != "" {
		vars[lam.IndexParam] = Type("integer")
	}
	guards := make(map[string]guard, len(c.guards))
	for k, v := range c.guards {
		if slices.Contains(v.roots, lam.Param) || (lam.IndexParam != "" && slices.Contains(v.roots, lam.IndexParam)) {
			continue
		}
		guards[k] = v
	}
	return inferCtx{s: c.s, guards: guards, vars: vars}
}

// Infer statically determines the JSON Schema type of an expression against s
// (e.g. "user.issues[0].value ?? 0"). The result carries s's root $defs, so it stays
// navigable/validatable. For plain sub-path lookup without expression semantics, see At.
func (s Schema) Infer(expression string) (Schema, error) {
	node, err := syntax.Parse(expression)
	if err != nil {
		return Schema{}, fmt.Errorf("parse %q: %w", expression, err)
	}
	return s.inferNodeWithGuards(node, s.guards)
}

// InferWithGuards is Infer with refinements already proved, keyed by rendered access path
// (`outputs.a.v`, as JoinPath emits). A key this context lacks is ignored. specs/guard-narrowing.md.
func (s Schema) InferWithGuards(expression string, narrowed map[string]Schema) (Schema, error) {
	return s.WithGuards(narrowed).Infer(expression)
}

// seedGuards turns proved paths into the guard map the inferrer already consults. The root
// is the leading segment: it is what a lambda parameter shadowing that name invalidates.
func seedGuards(narrowed map[string]Schema) map[string]guard {
	if len(narrowed) == 0 {
		return nil
	}
	out := make(map[string]guard, len(narrowed))
	for path, sc := range narrowed {
		segs, err := ParsePath(path)
		if err != nil || len(segs) == 0 || segs[0].IsIndex {
			continue
		}
		out[path] = guard{roots: []string{segs[0].Name}, s: sc}
	}
	return out
}

// InferNode is Infer over a parsed tree. A union context is typed per arm and joined, keeping
// the correlations flattening destroys; every arm must type. specs/path-sensitive-output.md.
func (s Schema) InferNode(node syntax.Node) (Schema, error) {
	return s.inferNodeWithGuards(node, s.guards)
}

func (s Schema) inferNodeWithGuards(node syntax.Node, guards map[string]guard) (Schema, error) {
	arms := s.contextStates()
	if len(arms) < 2 {
		return inferNode(node, inferCtx{s: s, guards: guards, vars: s.vars})
	}
	var (
		joined   Schema
		ok       int
		firstErr error
		failedIn string
	)
	for _, arm := range arms {
		// Seeded guards hold under every arm: the edge proved them before the split.
		t, err := inferNode(node, inferCtx{s: arm, guards: guards, vars: s.vars})
		if err != nil {
			if firstErr == nil {
				firstErr, failedIn = err, arm.Description()
			}
			continue
		}
		if ok == 0 {
			joined = t
		} else {
			joined = joined.Join(t)
		}
		ok++
	}
	if firstErr != nil {
		// Name the state only when another typed: one failing under every arm is just wrong.
		if ok > 0 && failedIn != "" {
			return Schema{}, fmt.Errorf("%s: %w", failedIn, firstErr)
		}
		return Schema{}, firstErr
	}
	return joined, nil
}

// contextStates returns a union context's arms, each carrying the pool so a `$ref` in an arm
// still resolves.
func (s Schema) contextStates() []Schema {
	if s.n == nil || len(s.n.AnyOf) == 0 {
		return nil
	}
	defs := s.rootDefs()
	out := make([]Schema, 0, len(s.n.AnyOf))
	for _, arm := range s.n.AnyOf {
		out = append(out, wrap(arm, defs))
	}
	return out
}

func inferNode(node syntax.Node, ictx inferCtx) (Schema, error) {
	switch n := node.(type) {
	case *syntax.IntNode:
		return Type("integer"), nil
	case *syntax.FloatNode:
		return Type("number"), nil
	case *syntax.StringNode:
		return Type("string"), nil
	case *syntax.BoolNode:
		return Type("boolean"), nil
	case *syntax.NullNode:
		return Type("null"), nil
	case *syntax.IdentNode:
		if g, ok := ictx.guards[identKey(n.Name)]; ok {
			return g.s, nil
		}
		if s, ok := ictx.vars[n.Name]; ok {
			return s, nil
		}
		return ictx.s.Property(n.Name)
	case *syntax.MemberNode:
		return inferMember(n, ictx)
	case *syntax.IndexNode:
		return inferIndexNode(n, ictx)
	case *syntax.KeyNode:
		return inferKeyNode(n, ictx)
	case *syntax.ArrayNode:
		return inferArray(n, ictx)
	case *syntax.ObjectNode:
		return inferObject(n, ictx)
	case *syntax.CallNode:
		return inferCall(n, ictx)
	case *syntax.BinaryNode:
		return inferBinary(n, ictx)
	case *syntax.UnaryNode:
		return inferUnary(n, ictx)
	case *syntax.CondNode:
		return inferConditional(n, ictx)
	case *syntax.LambdaNode:
		return Schema{}, ErrUnsupported{Detail: "a lambda is only valid as a map argument"}
	default:
		return Schema{}, ErrUnsupported{Detail: fmt.Sprintf("node type %T", node)}
	}
}

// inferBase resolves the base of a member or index access, applying the shared
// null-base rule. ok is false when the whole access collapses to null.
func inferBase(node syntax.Node, ictx inferCtx) (base Schema, ok bool, err error) {
	base, err = inferNode(node, ictx)
	if err != nil {
		return Schema{}, false, err
	}
	// A composed result (operator-built union) carries no defs: re-anchor so $refs resolve.
	base = base.WithDefs(ictx.s.DefsHandle())
	// Access on a known-null base is null, as at runtime — and it seeds recursive inference:
	// self.previous is null on iteration one, so `?? default` can fire.
	if base.IsNull() {
		return Schema{}, false, nil
	}
	if base.HasRef() {
		rb, rerr := base.Resolve()
		if rerr != nil {
			// Resolution may have demanded solving the referenced definition; its
			// failure is the real error and must not be masked.
			return Schema{}, false, rerr
		}
		if rb.IsNull() {
			return Schema{}, false, nil
		}
	}
	return base, true, nil
}

func inferMember(n *syntax.MemberNode, ictx inferCtx) (Schema, error) {
	if steps, ok := nodeSteps(n); ok {
		if g, ok := ictx.guards[guardKey(steps)]; ok {
			return g.s, nil
		}
	}
	base, ok, err := inferBase(n.Base, ictx)
	if err != nil || !ok {
		return nullOr(err)
	}
	return base.Property(n.Name)
}

func inferIndexNode(n *syntax.IndexNode, ictx inferCtx) (Schema, error) {
	if steps, ok := nodeSteps(n); ok {
		if g, ok := ictx.guards[guardKey(steps)]; ok {
			return g.s, nil
		}
	}
	base, ok, err := inferBase(n.Base, ictx)
	if err != nil || !ok {
		return nullOr(err)
	}
	return base.Index()
}

// inferKeyNode: the key type a[expr] needs follows from the base, so the error names the
// mismatch rather than the key type alone.
func inferKeyNode(n *syntax.KeyNode, ictx inferCtx) (Schema, error) {
	if steps, ok := nodeSteps(n); ok {
		if g, ok := ictx.guards[guardKey(steps)]; ok {
			return g.s, nil
		}
	}
	key, err := inferNode(n.Key, ictx)
	if err != nil {
		return Schema{}, err
	}
	base, ok, err := inferBase(n.Base, ictx)
	if err != nil || !ok {
		return nullOr(err)
	}
	value, wantKey, err := base.AnyKey()
	if err != nil {
		return Schema{}, err
	}
	// IsType is strict — a nullable key does not pass — so `m[k]` where k may be
	// null has to be narrowed or defaulted first, exactly as a null base would.
	if !key.IsType(wantKey) {
		return Schema{}, fmt.Errorf("a computed key must be %s, got %s", wantKey, key.TypeName())
	}
	return value, nil
}

func nullOr(err error) (Schema, error) {
	if err != nil {
		return Schema{}, err
	}
	return Type("null"), nil
}

// inferArray: an empty literal is itemless, which makes `?? []` a default without asserting an
// element type.
func inferArray(n *syntax.ArrayNode, ictx inferCtx) (Schema, error) {
	if len(n.Items) == 0 {
		return emptyArray(), nil
	}
	elems := make([]Schema, len(n.Items))
	for i, item := range n.Items {
		it, err := inferNode(item, ictx)
		if err != nil {
			return Schema{}, err
		}
		elems[i] = it
	}
	return ArrayLiteral(elems).WithDefs(ictx.s.DefsHandle()), nil
}

// inferObject: a closed object with every key required, as a Shape's object node infers;
// sorted keys keep the schema deterministic.
func inferObject(n *syntax.ObjectNode, ictx inferCtx) (Schema, error) {
	type entry struct {
		key string
		sc  Schema
	}
	entries := make([]entry, 0, len(n.Keys))
	for i, k := range n.Keys {
		v, err := inferNode(n.Values[i], ictx)
		if err != nil {
			return Schema{}, fmt.Errorf("key %q: %w", k, err)
		}
		entries = append(entries, entry{key: k, sc: v.WithoutDefs()})
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].key < entries[j].key })
	out := Object()
	for _, e := range entries {
		out = out.WithProperty(e.key, e.sc, true)
	}
	return out.WithDefs(ictx.s.DefsHandle()), nil
}

// inferCall: a $ref the lambda body leaves symbolic sits under `items`, which the productivity
// rule counts as productive.
func inferCall(n *syntax.CallNode, ictx inferCtx) (Schema, error) {
	if n.Name != "map" {
		return Schema{}, ErrUnsupported{Detail: fmt.Sprintf("function %q", n.Name)}
	}
	elem, err := mapElement(n, ictx)
	if err != nil {
		return Schema{}, err
	}
	lam, ok := n.Args[1].(*syntax.LambdaNode)
	if !ok {
		return Schema{}, ErrUnsupported{Detail: "map expects a lambda"}
	}
	body, err := inferNode(lam.Body, ictx.withParams(lam, elem))
	if err != nil {
		return Schema{}, err
	}
	return Array(body).WithDefs(ictx.s.DefsHandle()), nil
}

// mapElement infers a map's source and returns its element type.
func mapElement(n *syntax.CallNode, ictx inferCtx) (Schema, error) {
	if len(n.Args) != 2 {
		return Schema{}, ErrUnsupported{Detail: "map takes 2 arguments"}
	}
	src, err := inferNode(n.Args[0], ictx)
	if err != nil {
		return Schema{}, err
	}
	src = src.WithDefs(ictx.s.DefsHandle())
	if src.HasNull() {
		return Schema{}, errors.New("map source may be null; use ?? to provide a default array")
	}
	if !src.IsType("array") {
		return Schema{}, fmt.Errorf("map source must be an array, got %q", src.TypeName())
	}
	return elementOf(resolveTolerant(src))
}

// errNoElement: binding an unconstrained element would turn a typo in the lambda body into a
// runtime null instead of a registration error.
var errNoElement = errors.New("map source array has no element type")

// emptyArray: maxItems 0 marks `[]` provably empty, so elementOf can discard that arm and
// `xs ?? []` keeps xs's element type.
func emptyArray() Schema {
	zero := 0
	return Schema{n: &node{Type: SchemaType{"array"}, MaxItems: &zero}}
}

// elementOf joins a union source's variants, skipping provably-empty arms. Items, not Index:
// Index is nullable for out-of-bounds, and map only visits real elements.
func elementOf(src Schema) (Schema, error) {
	variants := src.Variants()
	if variants == nil {
		if isProvablyEmpty(src) || !src.HasItems() {
			return Schema{}, errNoElement
		}
		return src.Items(), nil
	}
	var joined Schema
	found := false
	for _, v := range variants {
		v = resolveTolerant(v)
		if v.IsNull() || isProvablyEmpty(v) {
			continue
		}
		if !v.HasItems() {
			return Schema{}, errNoElement
		}
		if !found {
			joined, found = v.Items(), true
			continue
		}
		joined = joined.Join(v.Items())
	}
	if !found {
		return Schema{}, errNoElement
	}
	return joined.Canonicalize(), nil
}

func isProvablyEmpty(s Schema) bool {
	max, ok := s.MaxItems()
	return ok && max == 0
}

func inferBinary(n *syntax.BinaryNode, ictx inferCtx) (Schema, error) {
	op, ok := inferBinaryOps[n.Op]
	if !ok {
		return Schema{}, ErrUnsupported{Detail: fmt.Sprintf("operator %q", n.Op)}
	}
	left, err := inferNode(n.Left, ictx)
	if err != nil {
		return Schema{}, err
	}
	// The right operand runs on exactly one outcome of the left (evalLogical short-circuits),
	// so it is typed under what that outcome proves.
	rightCtx := ictx
	switch n.Op {
	case "&&":
		rightCtx, _ = narrowCondition(n.Left, ictx)
	case "||":
		_, rightCtx = narrowCondition(n.Left, ictx)
	}
	right, err := inferNode(n.Right, rightCtx)
	if err != nil {
		return Schema{}, err
	}
	// Operands may be composed results (or preserved $refs) with no resolution
	// context of their own; re-anchor so operator-level analysis can resolve.
	left = left.WithDefs(ictx.s.DefsHandle())
	right = right.WithDefs(ictx.s.DefsHandle())
	return op(unwrapSingleVariant(left), unwrapSingleVariant(right))
}

func inferUnary(n *syntax.UnaryNode, ictx inferCtx) (Schema, error) {
	op, ok := inferUnaryOps[n.Op]
	if !ok {
		return Schema{}, ErrUnsupported{Detail: fmt.Sprintf("unary operator %q", n.Op)}
	}
	operand, err := inferNode(n.Operand, ictx)
	if err != nil {
		return Schema{}, err
	}
	operand = operand.WithDefs(ictx.s.DefsHandle())
	return op(unwrapSingleVariant(operand))
}

func inferConditional(n *syntax.CondNode, ictx inferCtx) (Schema, error) {
	if _, err := inferNode(n.Cond, ictx); err != nil {
		return Schema{}, err
	}
	thenCtx, elseCtx := narrowCondition(n.Cond, ictx)
	t, err := inferNode(n.Then, thenCtx)
	if err != nil {
		return Schema{}, err
	}
	f, err := inferNode(n.Else, elseCtx)
	if err != nil {
		return Schema{}, err
	}
	if schemasEqual(t, f) {
		return t, nil
	}
	if s, ok := nullableSchema(t, f); ok {
		return s, nil
	}
	if merged, ok := absorbEmptyArray(t, f); ok {
		return merged, nil
	}
	return OneOf(t, f), nil
}

// guardFact is one comparison as it holds on one branch (equal: the equal branch). What it
// PROVES is the consumer's: the walk is shared, the semantics are not. specs/guard-narrowing.md.
type guardFact struct {
	subject syntax.Node
	steps   []pathStep
	lit     syntax.Node
	equal   bool
}

// guardFacts is the walk only. `A && B` proves both halves when true and NEITHER when false;
// `||` mirrors it and `!` swaps the pair.
func guardFacts(cond syntax.Node) (whenTrue, whenFalse []guardFact) {
	switch n := cond.(type) {
	case *syntax.UnaryNode:
		if n.Op == "!" {
			t, f := guardFacts(n.Operand)
			return f, t
		}
	case *syntax.BinaryNode:
		switch n.Op {
		case "&&":
			lt, _ := guardFacts(n.Left)
			rt, _ := guardFacts(n.Right)
			return concatFacts(lt, rt), nil
		case "||":
			_, lf := guardFacts(n.Left)
			_, rf := guardFacts(n.Right)
			return nil, concatFacts(lf, rf)
		case "==", "!=":
			var subject, lit syntax.Node
			switch {
			case isLiteralNode(n.Right):
				subject, lit = n.Left, n.Right
			case isLiteralNode(n.Left):
				subject, lit = n.Right, n.Left
			default:
				return nil, nil
			}
			steps, ok := nodeSteps(subject)
			if !ok {
				return nil, nil
			}
			eq := n.Op == "=="
			return []guardFact{{subject, steps, lit, eq}}, []guardFact{{subject, steps, lit, !eq}}
		}
	}
	return nil, nil
}

// concatFacts copies rather than appending in place: the left slice is a caller's, and a
// chain would otherwise share one backing array across branches.
func concatFacts(a, b []guardFact) []guardFact {
	out := make([]guardFact, 0, len(a)+len(b))
	return append(append(out, a...), b...)
}

// narrowCondition: equality narrows to the literal's type; only `!= null` narrows the other
// way, since not being one non-null literal says nothing about type.
func narrowCondition(cond syntax.Node, ictx inferCtx) (thenCtx, elseCtx inferCtx) {
	whenTrue, whenFalse := guardFacts(cond)
	return applyGuardFacts(ictx, whenTrue), applyGuardFacts(ictx, whenFalse)
}

func applyGuardFacts(ictx inferCtx, facts []guardFact) inferCtx {
	for _, f := range facts {
		litSchema, err := inferNode(f.lit, ictx)
		if err != nil {
			continue
		}
		if f.equal {
			ictx = ictx.withGuard(f.steps, litSchema)
			continue
		}
		if _, isNull := f.lit.(*syntax.NullNode); !isNull {
			continue
		}
		if subject, err := inferNode(f.subject, ictx); err == nil {
			ictx = ictx.withGuard(f.steps, subject.StripNull())
		}
	}
	return ictx
}

func isLiteralNode(n syntax.Node) bool {
	switch n.(type) {
	case *syntax.BoolNode, *syntax.StringNode, *syntax.IntNode, *syntax.FloatNode, *syntax.NullNode:
		return true
	}
	return false
}

// nodeSteps renders a chain rooted at a bare identifier as steps, never a dot-path: a["a.b"]
// and a.a.b would collide in the guard map and the secret walk.
func nodeSteps(node syntax.Node) ([]pathStep, bool) {
	switch n := node.(type) {
	case *syntax.IdentNode:
		return []pathStep{propStep(n.Name)}, true
	case *syntax.MemberNode:
		if base, ok := nodeSteps(n.Base); ok {
			return append(base, propStep(n.Name)), true
		}
	case *syntax.IndexNode:
		if base, ok := nodeSteps(n.Base); ok {
			return append(base, indexStep(n.Index)), true
		}
	case *syntax.KeyNode:
		// Only a static-path key makes two reads of a[k] the same access. An operator-built
		// key loses narrowing, never soundness; the secret walk covers it separately.
		base, ok := nodeSteps(n.Base)
		if !ok {
			return nil, false
		}
		key, ok := nodeSteps(n.Key)
		if !ok {
			return nil, false
		}
		return append(base, keyStep(key)), true
	}
	return nil, false
}

// GuardFact is one comparison as it holds on one branch: IsNull if against `null`, Equal on the
// equal branch. Path is keyed as a read of it is, so it goes straight back to WithGuards.
// specs/guard-narrowing.md.
type GuardFact struct {
	Path   string
	IsNull bool
	Equal  bool
}

// GuardFacts is the guard catalogue over a source expression: what it proves when it holds,
// and when it fails. An expression it cannot read proves nothing and is not an error — most
// conditions are not guards.
func GuardFacts(expr string) (whenTrue, whenFalse []GuardFact, err error) {
	node, err := syntax.Parse(expr)
	if err != nil {
		return nil, nil, fmt.Errorf("parse %q: %w", expr, err)
	}
	t, f := guardFacts(node)
	return exportFacts(t), exportFacts(f), nil
}

func exportFacts(facts []guardFact) []GuardFact {
	if len(facts) == 0 {
		return nil
	}
	out := make([]GuardFact, 0, len(facts))
	for _, f := range facts {
		_, isNull := f.lit.(*syntax.NullNode)
		out = append(out, GuardFact{Path: renderPath(f.steps), IsNull: isNull, Equal: f.equal})
	}
	return out
}

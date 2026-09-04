// Static type inference for the genroc expression language, evaluated against a
// Schema context. The grammar lives in internal/expression/syntax and the
// matching runtime evaluator in internal/expression (Eval); the two must accept
// the same constructs:
//
//   - Literals: integer, float, string, bool, null
//   - Field access via dot notation: input.x, outputs.task.y
//   - Field access by string key, for keys no identifier can spell:
//     self.headers["retry-after"] — the same access, not a distinct construct
//   - Constant indexing: input.items[0]
//   - Object and array literals: {a: x, b: y}, [x, y]
//   - map with a lambda: map(input.items, item => {id: item.id})
//   - Arithmetic: +, -, *, /, % (numbers; + also concatenates strings)
//   - Comparison: ==, !=, <, >, <=, >= → boolean
//   - Logical: &&, || → boolean (short-circuit); ! → boolean
//   - Conditional: cond ? a : b
//   - Null coalescing: a ?? b (returns a if non-nil, else b)
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

// inferCtx: the immutable inference context — s (context schema carrying root $defs),
// guards (path → schema overrides for narrowed branches, shallow-copied), vars (lambda
// params in scope, shadowing context roots).
type inferCtx struct {
	s      Schema
	guards map[string]guard
	vars   map[string]Schema
}

// guard is one narrowed path. roots holds every identifier the path depends on,
// kept so a lambda that shadows any of them can drop the entry without decoding
// the key. There is more than one because a computed key contributes its own: the
// narrowing of `m[k]` is void once `k` means something else.
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

// withParams binds a lambda's parameters to the element type. Guards rooted at a
// name the lambda shadows are dropped: a narrowing established outside says
// nothing about the parameter that now owns that name.
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
	return s.InferNode(node)
}

// InferNode is Infer over an already-parsed expression. Callers that hold a
// parsed tree — internal/template — use this to avoid re-parsing the source.
//
// A context that is a UNION is one of several possible states, so the expression is typed
// under each and the results joined. That is what keeps a CORRELATION between two properties
// — `a` is null exactly where `b` is not — which flattening the arms destroys: `a ?? b` types
// nullable against the flattened view and non-null under every arm. The process output's
// context is such a union, one arm per way the process can end
// (specs/path-sensitive-output.md). Every arm must type: the expression runs in one of the
// states and nothing says which.
func (s Schema) InferNode(node syntax.Node) (Schema, error) {
	arms := s.contextStates()
	if len(arms) < 2 {
		return inferNode(node, inferCtx{s: s})
	}
	var (
		joined   Schema
		ok       int
		firstErr error
		failedIn string
	)
	for _, arm := range arms {
		t, err := inferNode(node, inferCtx{s: arm})
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
		// Name the state only when another one typed: an expression that is simply wrong fails
		// under every arm and deserves its plain message, unprefixed. The name comes off the
		// arm's own description, so nothing outside the schema has to carry it.
		if ok > 0 && failedIn != "" {
			return Schema{}, fmt.Errorf("%s: %w", failedIn, firstErr)
		}
		return Schema{}, firstErr
	}
	return joined, nil
}

// contextStates returns the alternative states a union context describes, each carrying the
// pool so a `$ref` inside an arm still resolves. A context that is not a union has one state,
// itself, and the caller takes the fast path.
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

// ReferencesSecret reports whether expression reads any value whose schema — or an
// enclosing object's along the access path — is secret. Conservative: any path through
// a secret node taints the whole expression, whatever it then does with the value.
func (s Schema) ReferencesSecret(expression string) (bool, error) {
	node, err := syntax.Parse(expression)
	if err != nil {
		return false, fmt.Errorf("parse %q: %w", expression, err)
	}
	return s.ReferencesSecretNode(node), nil
}

// ReferencesSecretNode is ReferencesSecret over an already-parsed expression.
func (s Schema) ReferencesSecretNode(node syntax.Node) bool {
	return walkSecretRefs(node, inferCtx{s: s})
}

// walkSecretRefs looks for a read of a secret value. A path rooted at a lambda
// parameter is resolved against that parameter's element type rather than the
// root context, so a secret that lives on the element — reachable only as
// item.token, never as a path from the root — still taints.
func walkSecretRefs(n syntax.Node, ictx inferCtx) bool {
	if n == nil {
		return false
	}
	if steps, ok := nodeSteps(n); ok {
		if elem, bound := ictx.vars[steps[0].prop]; bound {
			if secretAtSub(elem, steps[1:]) {
				return true
			}
		} else if stepsHitSecret(ictx.s.n, ictx.s.rootDefs(), steps) {
			return true
		}
	}
	switch x := n.(type) {
	case *syntax.MemberNode:
		return walkSecretRefs(x.Base, ictx)
	case *syntax.IndexNode:
		return walkSecretRefs(x.Base, ictx)
	case *syntax.KeyNode:
		return keySecretRefs(x, ictx)
	case *syntax.ArrayNode:
		for _, item := range x.Items {
			if walkSecretRefs(item, ictx) {
				return true
			}
		}
	case *syntax.ObjectNode:
		for _, v := range x.Values {
			if walkSecretRefs(v, ictx) {
				return true
			}
		}
	case *syntax.CallNode:
		return callSecretRefs(x, ictx)
	case *syntax.BinaryNode:
		return walkSecretRefs(x.Left, ictx) || walkSecretRefs(x.Right, ictx)
	case *syntax.UnaryNode:
		return walkSecretRefs(x.Operand, ictx)
	case *syntax.CondNode:
		return walkSecretRefs(x.Cond, ictx) || walkSecretRefs(x.Then, ictx) || walkSecretRefs(x.Else, ictx)
	}
	return false
}

// keySecretRefs covers computed keys, which the path walk cannot: a[expr] names no static
// path, so the taint reads off the resolved type. The mark lives on the VALUE schema
// (items/additionalProperties), never the container — a map of secrets is not a secret map.
func keySecretRefs(x *syntax.KeyNode, ictx inferCtx) bool {
	// Building the key can read a secret in its own right: vault[input.which].
	if walkSecretRefs(x.Key, ictx) {
		return true
	}
	value, err := inferNode(x, ictx)
	if err != nil {
		// The expression will not type-check anyway; taint rather than risk a leak,
		// matching the unresolvable-lambda case.
		return true
	}
	if secretAtSub(value, nil) {
		return true
	}
	return walkSecretRefs(x.Base, ictx)
}

func callSecretRefs(x *syntax.CallNode, ictx inferCtx) bool {
	for _, a := range x.Args {
		lam, isLambda := a.(*syntax.LambdaNode)
		if !isLambda {
			if walkSecretRefs(a, ictx) {
				return true
			}
			continue
		}
		elem, err := mapElement(x, ictx)
		if err != nil {
			// The expression will not type-check anyway; taint rather than risk a
			// leak, since over-tainting only costs log verbosity.
			return true
		}
		if walkSecretRefs(lam.Body, ictx.withParams(lam, elem)) {
			return true
		}
	}
	return false
}

// secretAtSub checks a path below an already-resolved schema; an empty sub-path
// means the value itself.
func secretAtSub(s Schema, sub []pathStep) bool {
	if len(sub) > 0 {
		return stepsHitSecret(s.n, s.rootDefs(), sub)
	}
	// The node's own flag is not enough: reading one field of a secret definition would taint
	// while copying the whole element did not — backwards, the copy exposes more. Follow the
	// ref, at every union position (nullable presents as oneOf[{$ref}, null]).
	if nodeOrTargetSecret(s.n, s.rootDefs()) {
		return true
	}
	return false
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
	// The base may be a composed result (an operator-built union) that carries no
	// resolution context of its own — re-anchor it to the context's root $defs so
	// any $refs inside still resolve.
	base = base.WithDefs(ictx.s.DefsHandle())
	// Access on a known-null base is null (runtime optional chaining does the same) — and it
	// seeds recursive inference: self.previous is null on iteration one, so `?? default` can
	// fire. A $ref base resolves for this check; the mid-solve null seed behaves identically.
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

// inferKeyNode types a computed key, a[expr]. The key must be a string (into a
// map) or an integer (into an array); which one is required follows from the base,
// so the error names the mismatch rather than the key type in isolation.
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

// inferArray types an array literal as an array of the joined element types. An
// empty literal is an itemless array, which is what makes `?? []` usable as a
// default without asserting an element type.
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

// inferObject types an object literal as a closed object with every key required,
// mirroring how a Shape's object node is inferred. Keys are emitted in sorted
// order so the generated schema is deterministic.
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

// inferCall types a builtin. map's source position is a look-inside construct: it
// must resolve the operand to read its element type. The lambda body is inferred
// in a child scope and may itself stay symbolic, so a $ref surviving into the
// result sits under `items`, which the productivity rule counts as productive.
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

// errNoElement is returned when a source array declares no element type. Binding
// an unconstrained element would turn a typo in the lambda body into a runtime
// null instead of a registration error.
var errNoElement = errors.New("map source array has no element type")

// emptyArray types the `[]` literal. maxItems 0 records that it can never hold an
// element, which is what lets `xs ?? []` keep xs's element type: the union the
// coalesce builds has a provably-empty variant that elementOf can discard.
func emptyArray() Schema {
	zero := 0
	return Schema{&node{Type: SchemaType{"array"}, MaxItems: &zero}}
}

// elementOf reads an array source's element type; a union source (`xs ?? []`, ternaries)
// joins its variants', skipping provably-empty arms. Items, not Index: Index is nullable
// because a constant index may be out of bounds — map only visits real elements.
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
	right, err := inferNode(n.Right, ictx)
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

// narrowCondition returns then/else contexts narrowed by an equality condition.
func narrowCondition(cond syntax.Node, ictx inferCtx) (thenCtx, elseCtx inferCtx) {
	thenCtx, elseCtx = ictx, ictx
	bin, ok := cond.(*syntax.BinaryNode)
	if !ok || (bin.Op != "==" && bin.Op != "!=") {
		return
	}

	var subject, litNode syntax.Node
	switch {
	case isLiteralNode(bin.Right):
		subject, litNode = bin.Left, bin.Right
	case isLiteralNode(bin.Left):
		subject, litNode = bin.Right, bin.Left
	default:
		return
	}

	steps, ok := nodeSteps(subject)
	if !ok {
		return
	}

	litSchema, err := inferNode(litNode, ictx)
	if err != nil {
		return
	}

	_, litIsNull := litNode.(*syntax.NullNode)

	if bin.Op == "==" {
		thenCtx = ictx.withGuard(steps, litSchema)
		if litIsNull {
			if subjectSchema, err := inferNode(subject, ictx); err == nil {
				elseCtx = ictx.withGuard(steps, subjectSchema.StripNull())
			}
		}
	} else {
		elseCtx = ictx.withGuard(steps, litSchema)
		if litIsNull {
			if subjectSchema, err := inferNode(subject, ictx); err == nil {
				thenCtx = ictx.withGuard(steps, subjectSchema.StripNull())
			}
		}
	}
	return
}

func isLiteralNode(n syntax.Node) bool {
	switch n.(type) {
	case *syntax.BoolNode, *syntax.StringNode, *syntax.IntNode, *syntax.FloatNode, *syntax.NullNode:
		return true
	}
	return false
}

// nodeSteps renders a member/index chain as steps rooted at a bare identifier (ok=false
// otherwise). Steps, never a rendered dot-path: a["a.b"] and a.a.b render identically as
// dots, and the guard map and the secret walk must not confuse them.
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
		// Only when the key is itself a static path, which is what makes two reads
		// of a[k] the same access and so safely narrowable. A computed key built by
		// an operator (a[x + "s"]) yields no path — it loses narrowing, never
		// soundness, and the secret walk covers it separately.
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

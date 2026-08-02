// Package expression provides runtime evaluation and reference analysis for the
// genroc expression language. The grammar lives in internal/expression/syntax;
// the matching static type inference lives on schema.Schema.Infer, which must
// accept exactly the same constructs:
//
//   - Literals: integer, float, string, bool, null
//   - Field access via dot notation: input.x, outputs.task.y
//   - Field access by string key: self.headers["retry-after"]
//   - Indexing: input.items[0], and a computed key input.items[i] / headers[k]
//     where every key shares one type (an array, or a map of additionalProperties)
//   - Object and array literals: {a: x, b: y}, [x, y]
//   - map with a lambda: map(input.items, item => {id: item.id})
//   - Arithmetic: +, -, *, /, % (numbers; + also concatenates strings)
//   - Comparison: ==, !=, <, >, <=, >= → boolean
//   - Logical: &&, || → boolean (short-circuit); ! → boolean
//   - Conditional: cond ? a : b
//   - Null coalescing: a ?? b (returns a if non-nil, else b)
package expression

import (
	"encoding/json"
	"fmt"

	"genroc/internal/expression/syntax"
	"genroc/internal/numeric"
)

func Eval(expression string, context map[string]any) (any, error) {
	node, err := syntax.Parse(expression)
	if err != nil {
		return nil, fmt.Errorf("parse %q: %w", expression, err)
	}
	return evalNode(node, env{ctx: context})
}

// EvalNode evaluates an already-parsed expression against context. Callers that
// hold a parsed tree — internal/template, which parses each ${ } block once —
// use this to avoid re-parsing the source on every evaluation.
func EvalNode(node syntax.Node, context map[string]any) (any, error) {
	return evalNode(node, env{ctx: context})
}

// env is the evaluation environment: the context roots, plus any lambda
// parameters currently in scope. A parameter shadows a context root of the same
// name, so map(xs, input => input.n) reads the element, not the process input.
type env struct {
	ctx  map[string]any
	vars map[string]any
}

// bind returns e extended with pairs. It copies rather than mutates so sibling
// elements of a map never observe each other's binding; vars holds at most a
// couple of entries per nesting level, so the copy is cheap.
func (e env) bind(pairs map[string]any) env {
	vars := make(map[string]any, len(e.vars)+len(pairs))
	for k, v := range e.vars {
		vars[k] = v
	}
	for k, v := range pairs {
		vars[k] = v
	}
	return env{ctx: e.ctx, vars: vars}
}

func (e env) lookup(name string) (any, bool) {
	if v, ok := e.vars[name]; ok {
		return v, true
	}
	if e.ctx == nil {
		return nil, false
	}
	v, ok := e.ctx[name]
	return v, ok
}

func evalNode(node syntax.Node, e env) (any, error) {
	switch n := node.(type) {
	case *syntax.IntNode:
		// Literals are carried as their exact text, the same representation
		// decoded data uses, so a literal past int64 is an ordinary value.
		return json.Number(n.Text), nil
	case *syntax.FloatNode:
		return json.Number(n.Text), nil
	case *syntax.StringNode:
		return n.Value, nil
	case *syntax.BoolNode:
		return n.Value, nil
	case *syntax.NullNode:
		return nil, nil
	case *syntax.IdentNode:
		v, ok := e.lookup(n.Name)
		if !ok {
			return nil, fmt.Errorf("field %q not found in context", n.Name)
		}
		return v, nil
	case *syntax.MemberNode:
		return evalMember(n, e)
	case *syntax.IndexNode:
		return evalIndex(n, e)
	case *syntax.KeyNode:
		return evalKey(n, e)
	case *syntax.ArrayNode:
		return evalArray(n, e)
	case *syntax.ObjectNode:
		return evalObject(n, e)
	case *syntax.CallNode:
		return evalCall(n, e)
	case *syntax.UnaryNode:
		return evalUnary(n, e)
	case *syntax.BinaryNode:
		return evalBinary(n, e)
	case *syntax.CondNode:
		return evalConditional(n, e)
	case *syntax.LambdaNode:
		// The parser only accepts a lambda in a builtin's lambda argument, so this
		// is unreachable from parsed source.
		return nil, ErrUnsupported{Detail: "a lambda is only valid as a map argument"}
	default:
		return nil, ErrUnsupported{Detail: fmt.Sprintf("node type %T", node)}
	}
}

// evalMember reads a property. A null, missing, or non-object base yields null,
// mirroring the optional-chaining semantics of type inference.
func evalMember(n *syntax.MemberNode, e env) (any, error) {
	base, err := evalNode(n.Base, e)
	if err != nil || base == nil {
		return nil, err
	}
	m, ok := base.(map[string]any)
	if !ok {
		return nil, nil
	}
	v, ok := m[n.Name]
	if !ok {
		return nil, nil
	}
	return v, nil
}

// evalIndex reads a constant index. A null, non-array, or out-of-bounds base
// yields null, matching evalMember.
func evalIndex(n *syntax.IndexNode, e env) (any, error) {
	base, err := evalNode(n.Base, e)
	if err != nil || base == nil {
		return nil, err
	}
	slice, ok := base.([]any)
	if !ok {
		return nil, nil
	}
	if n.Index < 0 || n.Index >= len(slice) {
		return nil, nil
	}
	return slice[n.Index], nil
}

// evalKey reads a computed key. A null or non-container base yields null, matching
// evalMember and evalIndex; an absent key or out-of-range index likewise. A key of
// the wrong type is an error rather than null: inference has already established
// that the key types, so reaching here means the data contradicted its schema, and
// silently reading nothing would hide that.
func evalKey(n *syntax.KeyNode, e env) (any, error) {
	base, err := evalNode(n.Base, e)
	if err != nil || base == nil {
		return nil, err
	}
	key, err := evalNode(n.Key, e)
	if err != nil {
		return nil, err
	}
	switch b := base.(type) {
	case map[string]any:
		name, ok := key.(string)
		if !ok {
			return nil, fmt.Errorf("a computed key into a map must be a string, got %s", jsonKindOf(key))
		}
		return b[name], nil
	case []any:
		d, ok := numeric.ToDecimal(key)
		if !ok || !numeric.IsIntegral(key) {
			return nil, fmt.Errorf("a computed index into an array must be an integer, got %s", jsonKindOf(key))
		}
		i, err := d.Int64()
		if err != nil || i < 0 || i >= int64(len(b)) {
			return nil, nil
		}
		return b[i], nil
	}
	return nil, nil
}

func jsonKindOf(v any) string {
	switch v.(type) {
	case nil:
		return "null"
	case string:
		return "string"
	case bool:
		return "boolean"
	case map[string]any:
		return "object"
	case []any:
		return "array"
	}
	if _, ok := numeric.ToDecimal(v); ok {
		return "number"
	}
	return fmt.Sprintf("%T", v)
}

func evalArray(n *syntax.ArrayNode, e env) (any, error) {
	out := make([]any, len(n.Items))
	for i, item := range n.Items {
		v, err := evalNode(item, e)
		if err != nil {
			return nil, err
		}
		out[i] = v
	}
	return out, nil
}

func evalObject(n *syntax.ObjectNode, e env) (any, error) {
	out := make(map[string]any, len(n.Keys))
	for i, key := range n.Keys {
		v, err := evalNode(n.Values[i], e)
		if err != nil {
			return nil, fmt.Errorf("key %q: %w", key, err)
		}
		out[key] = v
	}
	return out, nil
}

func evalCall(n *syntax.CallNode, e env) (any, error) {
	if n.Name != "map" {
		return nil, ErrUnsupported{Detail: fmt.Sprintf("function %q", n.Name)}
	}
	src, err := evalNode(n.Args[0], e)
	if err != nil {
		return nil, err
	}
	// Inference rejects a nullable or non-array source, so a registered definition
	// cannot reach these; they guard hand-built contexts and stale definitions.
	if src == nil {
		return nil, fmt.Errorf("map source is null; use ?? to provide a default array")
	}
	items, ok := src.([]any)
	if !ok {
		return nil, fmt.Errorf("map source must be an array, got %T", src)
	}
	lam, ok := n.Args[1].(*syntax.LambdaNode)
	if !ok {
		return nil, ErrUnsupported{Detail: "map expects a lambda"}
	}
	out := make([]any, len(items))
	for i, item := range items {
		pairs := map[string]any{lam.Param: item}
		if lam.IndexParam != "" {
			pairs[lam.IndexParam] = i
		}
		v, err := evalNode(lam.Body, e.bind(pairs))
		if err != nil {
			return nil, err
		}
		out[i] = v
	}
	return out, nil
}

func evalBinary(n *syntax.BinaryNode, e env) (any, error) {
	// Short-circuit operators are evaluated before the ops table lookup.
	switch n.Op {
	case "??":
		left, err := evalNode(n.Left, e)
		if err != nil {
			return nil, err
		}
		if left != nil {
			return left, nil
		}
		return evalNode(n.Right, e)
	case "&&", "||":
		return evalLogical(n, e)
	}

	op, ok := binaryOps[n.Op]
	if !ok {
		return nil, ErrUnsupported{Detail: fmt.Sprintf("operator %q", n.Op)}
	}
	left, err := evalNode(n.Left, e)
	if err != nil {
		return nil, err
	}
	right, err := evalNode(n.Right, e)
	if err != nil {
		return nil, err
	}
	return op(left, right)
}

func evalLogical(n *syntax.BinaryNode, e env) (any, error) {
	left, err := evalNode(n.Left, e)
	if err != nil {
		return nil, err
	}
	lb, ok := left.(bool)
	if !ok {
		return nil, fmt.Errorf("%s requires boolean operands, got %T", n.Op, left)
	}
	// Short-circuit: && on false and || on true never evaluate the right operand.
	if (n.Op == "&&") != lb {
		return lb, nil
	}
	right, err := evalNode(n.Right, e)
	if err != nil {
		return nil, err
	}
	rb, ok := right.(bool)
	if !ok {
		return nil, fmt.Errorf("%s requires boolean operands, got %T", n.Op, right)
	}
	return rb, nil
}

func evalUnary(n *syntax.UnaryNode, e env) (any, error) {
	op, ok := unaryOps[n.Op]
	if !ok {
		return nil, ErrUnsupported{Detail: fmt.Sprintf("unary operator %q", n.Op)}
	}
	operand, err := evalNode(n.Operand, e)
	if err != nil {
		return nil, err
	}
	return op(operand)
}

func evalConditional(n *syntax.CondNode, e env) (any, error) {
	cond, err := evalNode(n.Cond, e)
	if err != nil {
		return nil, err
	}
	if mustBool(cond) {
		return evalNode(n.Then, e)
	}
	return evalNode(n.Else, e)
}

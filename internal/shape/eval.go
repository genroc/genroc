package shape

import (
	"fmt"

	"genroc/internal/expression"
	"genroc/internal/template"
)

// Eval evaluates a templated value against runtime data env: a string leaf is a template
// (a $: leaf returning its raw typed value, otherwise a stringified template), an array
// and object evaluate their members recursively, and a scalar/null passes through. It
// operates on a raw value; the Shape.Eval method is the sugar over a Shape's Raw.
func Eval(node any, env map[string]any) (any, error) {
	switch n := node.(type) {
	case string:
		t, err := template.Get(n)
		if err != nil {
			return nil, err
		}
		return t.EvalAny(env)
	case []any:
		out := make([]any, len(n))
		for i, v := range n {
			ev, err := Eval(v, env)
			if err != nil {
				return nil, fmt.Errorf("[%d]: %w", i, err)
			}
			out[i] = ev
		}
		return out, nil
	case map[string]any:
		out := make(map[string]any, len(n))
		for k, v := range n {
			ev, err := Eval(v, env)
			if err != nil {
				return nil, fmt.Errorf("%q: %w", k, err)
			}
			out[k] = ev
		}
		return out, nil
	case bool, float64, nil:
		// Scalar literal or null: pass through unchanged.
		return n, nil
	default:
		return nil, fmt.Errorf("invalid shape node %T", node)
	}
}

// Eval is the runtime phase: it computes the shape's value from ctxData — the actual
// values of the roots, keyed as they are named in the check-phase context schema.
// Expressions were already type-checked by Check, so Eval just produces the concrete
// structure from the data. An Expr shape evaluates its bare expression directly.
func (s *Shape) Eval(ctxData map[string]any) (any, error) {
	if s.Expr {
		return expression.Eval(s.exprString(), ctxData)
	}
	return Eval(s.Raw, ctxData)
}

// Roots reports which context roots the shape's expressions reference — the bare
// expression for an Expr shape, aggregated across every leaf otherwise — so the engine can
// lazily resolve only the value-slots the shape reads.
func (s *Shape) Roots() (expression.Roots, error) {
	return s.refs()
}

// Roots unions the root references of every template-string leaf in a templated value, so
// the engine lazily resolves only the value-slots the value reads.
func Roots(node any) (expression.Roots, error) {
	var r expression.Roots
	var walk func(n any) error
	walk = func(n any) error {
		switch v := n.(type) {
		case string:
			t, err := template.Get(v)
			if err != nil {
				return err
			}
			r.Union(t.RootRefs())
		case []any:
			for _, vv := range v {
				if err := walk(vv); err != nil {
					return err
				}
			}
		case map[string]any:
			for _, vv := range v {
				if err := walk(vv); err != nil {
					return err
				}
			}
		}
		return nil
	}
	return r, walk(node)
}

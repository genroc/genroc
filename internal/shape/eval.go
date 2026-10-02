package shape

import (
	"encoding/json"
	"fmt"

	"genroc/internal/expression"
	"genroc/internal/template"
)

// Eval evaluates a raw templated value against env: a $: leaf yields its typed value, any other
// string leaf a string, and arrays and objects evaluate member-wise.
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
	case bool, json.Number, nil:
		return n, nil
	default:
		return nil, fmt.Errorf("invalid shape node %T", node)
	}
}

// Eval assumes Check passed, and keys ctxData as the roots are named in Check's context schema.
func (s *Shape) Eval(ctxData map[string]any) (any, error) {
	if s.Expr {
		return expression.Eval(s.exprString(), ctxData)
	}
	return Eval(s.Raw, ctxData)
}

// Roots lists the context roots the shape reads; the engine resolves only those.
func (s *Shape) Roots() (expression.Roots, error) {
	return s.refs()
}

// Roots unions the root references of every string leaf in a raw templated value.
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

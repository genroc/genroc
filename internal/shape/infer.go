package shape

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"genroc/internal/expression"
	"genroc/internal/schema"
	"genroc/internal/template"
)

// Infer types a raw templated value against ctx; an object's keys are all required, and label
// prefixes errors.
func Infer(node any, ctx schema.Schema, label string) (schema.Schema, error) {
	switch n := node.(type) {
	case string:
		t, err := template.Get(n)
		if err != nil {
			return schema.Schema{}, fmt.Errorf("%s: %w", label, err)
		}
		inferred, err := t.InferType(ctx)
		if err != nil {
			return schema.Schema{}, fmt.Errorf("%s: %w", label, err)
		}
		// The leaf carries ctx's $defs; the structure it is embedded in owns them.
		return inferred.WithoutDefs(), nil
	case []any:
		elems := make([]schema.Schema, len(n))
		for i, item := range n {
			el, err := Infer(item, ctx, schema.JoinIndex(label, i))
			if err != nil {
				return schema.Schema{}, err
			}
			elems[i] = el
		}
		return schema.ArrayLiteral(elems), nil
	case map[string]any:
		names := make([]string, 0, len(n))
		for name := range n {
			names = append(names, name)
		}
		slices.Sort(names)
		out := schema.Object()
		for _, name := range names {
			p, err := Infer(n[name], ctx, schema.JoinPath(label, name))
			if err != nil {
				return schema.Schema{}, err
			}
			out = out.WithProperty(name, p, true)
		}
		return out, nil
	case bool:
		return schema.Type("boolean"), nil
	case json.Number:
		// Spelling decides, as for an expression literal: a fraction or exponent is a number.
		if strings.ContainsAny(string(n), ".eE") {
			return schema.Type("number"), nil
		}
		return schema.Type("integer"), nil
	case nil:
		return schema.Type("null"), nil
	default:
		return schema.Schema{}, fmt.Errorf("%s: invalid shape node %T", label, node)
	}
}

// CheckHooks tailor Check's errors; a nil hook keeps the default. Roots words a ROOT problem (an
// expression touches something not usable here), Result a RESULT one (the value misfits Schema).
type CheckHooks struct {
	// Roots runs before inference with every root the shape references; an error rejects it.
	Roots func(refs expression.Roots) error
	// Result runs when the inferred type misfits Schema; nil keeps the default message.
	Result func(inferred, required schema.Schema) error
}

// A fixed target (headers, query) has no conform behind it and takes plain subset; a declared
// slot schema takes the relation paired with the conform it gets.
func (s *Shape) fits(norm schema.Schema) bool {
	if s.Conformed {
		return norm.ConformsExactlyTo(*s.Schema)
	}
	return norm.IsSubset(*s.Schema)
}

// Check is the static-validation phase with default messages; see CheckWith.
func (s *Shape) Check(ctxSchema schema.Schema) (schema.Schema, error) {
	return s.CheckWith(ctxSchema, CheckHooks{})
}

func (s *Shape) refs() (expression.Roots, error) {
	if s.Expr {
		return expression.RootRefs(s.exprString())
	}
	return Roots(s.Raw)
}

func (s *Shape) inferType(ctxSchema schema.Schema, label string) (schema.Schema, error) {
	if s.Expr {
		t, err := ctxSchema.Infer(s.exprString())
		if err != nil {
			return schema.Schema{}, fmt.Errorf("%s: %w", label, err)
		}
		return t, nil
	}
	return Infer(s.Raw, ctxSchema, label)
}

// CheckWith runs the Roots hook, inference, then (when Schema is set) the conformance check
// Result words, and returns the inferred type. ctxSchema's properties are the roots; both
// schemas must be normalized.
func (s *Shape) CheckWith(ctxSchema schema.Schema, hooks CheckHooks) (schema.Schema, error) {
	label := s.Name
	if label == "" {
		label = "shape"
	}
	if hooks.Roots != nil {
		refs, err := s.refs()
		if err != nil {
			return schema.Schema{}, fmt.Errorf("%s: %w", label, err)
		}
		if err := hooks.Roots(refs); err != nil {
			return schema.Schema{}, err
		}
	}
	inferred, err := s.inferType(ctxSchema, label)
	if err != nil {
		return schema.Schema{}, err
	}
	if s.Schema != nil {
		norm := inferred
		if h := ctxSchema.DefsHandle(); !h.IsZero() {
			if norm, err = inferred.WithDefs(h).Normalize(); err != nil {
				return schema.Schema{}, fmt.Errorf("%s: %w", label, err)
			}
		}
		if !s.fits(norm) {
			if hooks.Result != nil {
				if e := hooks.Result(norm, *s.Schema); e != nil {
					return schema.Schema{}, e
				}
			}
			return schema.Schema{}, fmt.Errorf("%s does not conform to the required schema", label)
		}
	}
	return inferred, nil
}

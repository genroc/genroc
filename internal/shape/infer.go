package shape

import (
	"fmt"
	"math"
	"slices"

	"genroc/internal/expression"
	"genroc/internal/schema"
	"genroc/internal/template"
)

// Infer returns the static type (a JSON Schema) of a templated value evaluated against
// context ctx: a string leaf yields its template's inferred type (a $: leaf preserving
// the expression's type), an array joins its element types, an object infers each value
// (all keys required), and a scalar/null types as its JSON kind. label prefixes errors.
//
// It operates on a raw value so a bare templated string (a fetch url/method) can be typed
// the same way as a full Shape; the Shape.Infer method is the sugar over a Shape's Raw.
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
		// The inferred sub-schema carries the context's root $defs for its own
		// resolvability; the leaf is embedded into a structure whose root owns the
		// defs, so re-root it bare.
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
	case float64:
		// JSON numbers decode to float64; a whole number types as integer so a literal
		// like 3 is a subset of an `integer` slot, a fractional one as number.
		if n == math.Trunc(n) {
			return schema.Type("integer"), nil
		}
		return schema.Type("number"), nil
	case nil:
		return schema.Type("null"), nil
	default:
		return schema.Schema{}, fmt.Errorf("%s: invalid shape node %T", label, node)
	}
}

// CheckHooks turn Check's raw findings into tailored errors. Both are optional; a nil hook
// leaves Check's default behavior. They separate the two kinds of problem a shape can have:
// a ROOT problem (an expression touches something that exists in general but is not usable
// here) and a RESULT problem (what the shape produces does not fit its required schema).
type CheckHooks struct {
	// Roots is called before inference with the roots the shape's expressions reference,
	// aggregated across every leaf. Return a non-nil error to reject the shape — e.g.
	// self.result is referenced but the action has no result_schema, so it is not available
	// here. This is how a caller crafts a message for touching an unavailable root.
	Roots func(refs expression.Roots) error
	// Result is called when the shape declares a required Schema and the inferred type is
	// not a subset of it, with both schemas so the caller can inspect the mismatch
	// (inferred.HasNull(), inferred.TypeName(), inferred.IsType("array"), …) and craft the
	// message. Return nil to fall back to Check's default message.
	Result func(inferred, required schema.Schema) error
}

// Check is the static-validation phase with default messages; see CheckWith.
func (s *Shape) Check(ctxSchema schema.Schema) (schema.Schema, error) {
	return s.CheckWith(ctxSchema, CheckHooks{})
}

// refs returns the roots the shape's expressions reference — from the bare expression for
// an Expr shape, aggregated across every leaf otherwise.
func (s *Shape) refs() (expression.Roots, error) {
	if s.Expr {
		return expression.RootRefs(s.exprString())
	}
	return Roots(s.Raw)
}

// inferType infers the shape's type against ctxSchema: a bare expression for an Expr shape
// (directly, preserving type), the recursive templated value otherwise.
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

// CheckWith is the static-validation phase. ctxSchema is an object schema whose properties
// are the roots (input, config, outputs, self, …) expressions may navigate; referencing an
// undeclared root or a bad path is an error. It runs in order: the Roots hook (reference
// availability), then inference (every leaf type-checked), then — if the shape declares a
// required Schema — a conformance check whose failure is handed to the Result hook. It
// returns the inferred type, which is the whole answer when Schema is nil (free projection).
//
// The required Schema and ctxSchema are assumed normalized; the inferred type is normalized
// against ctxSchema's $defs before the subset check so ref-bearing schemas compare cleanly.
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
		if !norm.IsSubset(*s.Schema) {
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

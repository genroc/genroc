// Package shape is the self-contained "templated value" unit: a Shape is a value
// authored with expressions at its leaves, which the package can type-check against a
// context schema (Infer) and evaluate against runtime data (Eval), independently of the
// process model, validation, or engine packages.
//
// A Shape is recursively:
//
//	Shape = string | number | boolean | null | Shape[] | Record<string, Shape>
//
// A string leaf is a template or a $: typed expression (see the template package); a
// scalar/array/object is a literal built recursively. The authoring structure fixes each
// node's kind, but a string leaf may evaluate to any type.
package shape

import (
	"encoding/json"
	"fmt"

	"genroc/internal/schema"
)

// Shape is a templated value together with the optional structure it must produce and a
// name locating it in errors. It has two phases: Check validates it statically against a
// ContextSchema (roots' types), and Eval computes its value against a ContextData (roots'
// data). The two contexts are provided independently — static validation at registration,
// evaluation per run.
//
// Only Raw is populated by JSON (un)marshaling; Schema, Name and Expr are attached at the
// slot the shape belongs to.
type Shape struct {
	Raw    any            // the templated value: string | float64 | bool | nil | []any | map[string]any
	Schema *schema.Schema // optional: the required structure Check verifies conformance to
	Name   string         // optional: locates the shape in error messages (e.g. "task X headers")
	// Expr marks an expression-only slot (a switch case, child_list over): Raw is
	// a single bare expression string — not a template — checked and evaluated directly,
	// with a required Schema of the expected type (e.g. boolean for a case). Structural and
	// template semantics do not apply.
	Expr bool
}

// exprString returns Raw as the bare-expression source for an Expr shape (empty if Raw is
// not a string, which then surfaces as an expression parse error).
func (s *Shape) exprString() string {
	str, _ := s.Raw.(string)
	return str
}

func (s *Shape) UnmarshalJSON(b []byte) error {
	var raw any
	if err := json.Unmarshal(b, &raw); err != nil {
		return err
	}
	if err := checkShape(raw); err != nil {
		return fmt.Errorf("shape: %w", err)
	}
	s.Raw = raw
	return nil
}

func (s Shape) MarshalJSON() ([]byte, error) {
	return json.Marshal(s.Raw)
}

// Present reports whether the shape carries a value; nil-safe so callers can skip a
// separate nil check.
func (s *Shape) Present() bool {
	return s != nil && s.Raw != nil
}

// Strings returns every string leaf in the shape (descending into arrays and objects),
// used to collect outputs.<id> references for the output-dependency graph.
func (s *Shape) Strings() []string {
	if s == nil {
		return nil
	}
	var out []string
	var walk func(any)
	walk = func(n any) {
		switch v := n.(type) {
		case string:
			out = append(out, v)
		case []any:
			for _, c := range v {
				walk(c)
			}
		case map[string]any:
			for _, c := range v {
				walk(c)
			}
		}
	}
	walk(s.Raw)
	return out
}

// checkShape recursively enforces the value grammar:
// string | number | boolean | null | Shape[] | Record<string, Shape>. A string leaf is a
// template or a $: expression; scalars and null are literals; arrays and objects are
// built recursively. JSON numbers decode to float64, so that is the only numeric kind.
func checkShape(n any) error {
	switch v := n.(type) {
	case string, float64, bool, nil:
		return nil
	case []any:
		for i, c := range v {
			if err := checkShape(c); err != nil {
				return fmt.Errorf("[%d]: %w", i, err)
			}
		}
		return nil
	case map[string]any:
		for k, c := range v {
			if err := checkShape(c); err != nil {
				return fmt.Errorf("%q: %w", k, err)
			}
		}
		return nil
	default:
		return fmt.Errorf("must be a string, number, boolean, null, array or object, got %T", n)
	}
}

// JSONSchemaBytes exposes the Shape schema for OpenAPI reflection (swaggest calls it to
// produce the ModelShape def). It is generated, not hand-written: the generic Value grammar
// from GenericValueSchema, the single source every free Shape slot resolves to. See that
// function for the anyOf/recursion/permissive-array rationale.
func (Shape) JSONSchemaBytes() ([]byte, error) {
	return GenericValueSchema()
}

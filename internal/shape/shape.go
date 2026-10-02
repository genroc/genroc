// Package shape is a templated value: expressions at its leaves, type-checked against a context
// schema (Infer) and evaluated against runtime data (Eval). It imports nothing of the process
// model, validation or engine. A string leaf may evaluate to any type.
package shape

import (
	"encoding/json"
	"fmt"

	"genroc/internal/schema"
)

// Shape is a templated value plus the structure it must produce. Only Raw survives JSON
// (un)marshaling; Schema, Name, Expr and Conformed are attached by the owning slot.
type Shape struct {
	Raw    any            // the templated value: string | float64 | bool | nil | []any | map[string]any
	Schema *schema.Schema // optional: the required structure Check verifies conformance to
	Name   string         // optional: locates the shape in error messages (e.g. "task X headers")
	// Expr marks an expression-only slot (a switch case, child_list over): Raw is one bare
	// expression, not a template, and Schema is required.
	Expr bool
	// Conformed marks Schema as one the value is conformed to at runtime, so Check uses the
	// conform's paired relation rather than plain subset. specs/declared-slot-schemas.md §4.
	Conformed bool
}

// A non-string Raw yields "", which surfaces as an expression parse error.
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

// Present is nil-safe: a nil *Shape is absent.
func (s *Shape) Present() bool {
	return s != nil && s.Raw != nil
}

// checkShape enforces string | number | boolean | null | Shape[] | Record<string, Shape>.
// JSON numbers decode to float64, so that is the only numeric kind.
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

// JSONSchemaBytes is swaggest's hook for the ModelShape def; GenericValueSchema is its one source.
func (Shape) JSONSchemaBytes() ([]byte, error) {
	return GenericValueSchema()
}

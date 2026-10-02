package schema

import (
	"encoding/json"
	"fmt"
	"genroc/internal/numeric"
	"maps"
	"slices"
	"strings"
	"unicode/utf8"
)

// Validate returns a normalized copy of data: undeclared keys dropped, absent optionals filled
// from their conformed default, a missing required one an error; "integer" takes any integral
// number. The result shares nothing with data; a nil or {} schema passes it through.
func (s Schema) Validate(data any, mode ...ConformMode) (any, error) {
	return conformGuard(s.n, s.rootDefs(), data, "", nil, firstMode(mode))
}

// firstMode defaults to Strict, the document check every boundary caller relies on.
func firstMode(mode []ConformMode) ConformMode {
	if len(mode) == 0 {
		return Strict
	}
	return mode[0]
}

// ValidateAt is At(path) followed by Validate: it checks data against the subschema at
// path (e.g. "outputs.taskA") and returns the normalized value. Optional path segments
// are treated as nullable, matching At.
func (s Schema) ValidateAt(path string, data any) (any, error) {
	sub, err := s.At(path)
	if err != nil {
		return nil, err
	}
	return sub.Validate(data)
}

// conform is the recursive validator/normalizer. path is data's dotted location within
// the root value (empty at the root), used only for error messages.
func conform(nd *node, defs map[string]*node, data any, path string) (any, error) {
	return conformGuard(nd, defs, data, path, nil, Strict)
}

// ConformMode selects what the one walk of schema-and-value is FOR. Do not add a walk beside
// it: it would have to rediscover where a value lives inside a schema and stay in step.
type ConformMode int

const (
	// Strict is the default: a document check at a boundary. An absent required property
	// is rejected whatever its type, and undeclared keys are stripped.
	Strict ConformMode = iota

	// ConformToSchemaExactly is a MIGRATION (specs/compat-command.md §2d): an absent required
	// nullable gets null written in; a null an optional non-nullable cannot hold loses its key.
	// No defaults filled; undeclared keys stripped — validation.MigrateState restores them.
	ConformToSchemaExactly
)

// conformGuard: visiting holds nodes expanded at this value position, so a $ref back to one
// fails the branch (stored schemas skip CheckDoc). Descent into a property/element starts fresh.
func conformGuard(nd *node, defs map[string]*node, data any, path string, visiting map[*node]bool, mode ConformMode) (any, error) {
	resolved, err := deref(nd, defs)
	if err != nil {
		return nil, err
	}
	if nd != nil && nd.Ref != "" {
		if visiting[resolved] {
			return nil, fmt.Errorf("%sschema reference cycle without structural progress", pathPrefix(path))
		}
		next := make(map[*node]bool, len(visiting)+1)
		for k := range visiting {
			next[k] = true
		}
		next[resolved] = true
		visiting = next
	}
	if resolved == nil || isEmptyNode(resolved) {
		return data, nil // unconstrained — pass through untouched
	}

	// Combinators take precedence: a nullable complex value is modelled as
	// oneOf:[X, {type:null}], and discriminated unions as anyOf/oneOf of objects.
	if len(resolved.AnyOf) > 0 {
		return conformUnion(resolved.AnyOf, defs, data, path, false, visiting, mode)
	}
	if len(resolved.OneOf) > 0 {
		return conformUnion(resolved.OneOf, defs, data, path, true, visiting, mode)
	}

	if err := checkType(resolved, data, path); err != nil {
		return nil, err
	}
	if len(resolved.Enum) > 0 && !enumContains(resolved.Enum, data) {
		return nil, fmt.Errorf("%svalue is not one of the permitted enum values", pathPrefix(path))
	}

	switch v := data.(type) {
	case nil:
		return nil, nil
	case map[string]any:
		if isObjectSchema(resolved) {
			return conformObject(resolved, defs, v, path, mode)
		}
		return v, nil
	case []any:
		if resolved.Items != nil || resolved.Type.Contains("array") {
			return conformArray(resolved, defs, v, path, mode)
		}
		return v, nil
	default:
		if err := checkScalar(resolved, data, path); err != nil {
			return nil, err
		}
		return data, nil
	}
}

// conformObject keeps declared properties (filling defaults for absent optionals,
// erroring on absent required) and recurses into present values. Undeclared keys are
// stripped for a closed object, or conformed against AdditionalProperties for an open map.
func conformObject(nd *node, defs map[string]*node, v map[string]any, path string, mode ConformMode) (any, error) {
	required := make(map[string]bool, len(nd.Required))
	for _, r := range nd.Required {
		required[r] = true
	}
	out := make(map[string]any, len(nd.Properties))
	// Sorted, because the first failure is the one reported: map order would name a different
	// property on each run for the same value.
	for _, name := range slices.Sorted(maps.Keys(nd.Properties)) {
		prop := nd.Properties[name]
		val, present := v[name]
		if !present {
			if required[name] {
				// The one gap a migration closes: absence and null navigate the same way,
				// so a nullable slot can be written in rather than rejected.
				if mode == ConformToSchemaExactly && hasNullResolved(prop, defs) {
					out[name] = nil
					continue
				}
				return nil, fmt.Errorf("%srequired property %q is missing", pathPrefix(path), name)
			}
			if def := propDefault(prop, defs); def != nil && mode == Strict {
				// Conformed like a supplied value, so a filled default can never violate the
				// schema and object defaults are normalized too.
				norm, err := conformGuard(prop, defs, cloneJSON(def), JoinPath(path, name), nil, mode)
				if err != nil {
					return nil, fmt.Errorf("invalid schema default: %w", err)
				}
				out[name] = norm
			}
			continue // absent optional without a default is omitted
		}
		// Where the property is OPTIONAL, removing a null the schema will not hold reconciles
		// the value; required and non-nullable falls through to the error.
		if mode == ConformToSchemaExactly && val == nil && !required[name] && !hasNullResolved(prop, defs) {
			continue
		}
		norm, err := conformGuard(prop, defs, val, JoinPath(path, name), nil, mode)
		if err != nil {
			return nil, err
		}
		out[name] = norm
	}
	// A closed object drops undeclared keys in every mode, a migration's included.
	if nd.AdditionalProperties != nil {
		for _, name := range slices.Sorted(maps.Keys(v)) {
			if _, declared := nd.Properties[name]; declared {
				continue
			}
			norm, err := conformGuard(nd.AdditionalProperties, defs, v[name], JoinPath(path, name), nil, mode)
			if err != nil {
				return nil, err
			}
			out[name] = norm
		}
	}
	return out, nil
}

func conformArray(nd *node, defs map[string]*node, arr []any, path string, mode ConformMode) (any, error) {
	if nd.MinItems != nil && len(arr) < *nd.MinItems {
		return nil, fmt.Errorf("%sarray has %d items, fewer than minItems %d", pathPrefix(path), len(arr), *nd.MinItems)
	}
	if nd.MaxItems != nil && len(arr) > *nd.MaxItems {
		return nil, fmt.Errorf("%sarray has %d items, more than maxItems %d", pathPrefix(path), len(arr), *nd.MaxItems)
	}
	out := make([]any, len(arr))
	for i, el := range arr {
		norm, err := conformGuard(nd.Items, defs, el, JoinIndex(path, i), nil, mode)
		if err != nil {
			return nil, err
		}
		out[i] = norm
	}
	return out, nil
}

// conformUnion normalizes data against a oneOf/anyOf: anyOf returns the first branch
// that validates; oneOf requires exactly one (zero or several is an error). Branches
// keep the value at the same position, so the caller's visiting set carries through.
func conformUnion(branches []*node, defs map[string]*node, data any, path string, exactlyOne bool, visiting map[*node]bool, mode ConformMode) (any, error) {
	var (
		firstErr error
		match    any
		matches  int
	)
	for _, b := range branches {
		res, err := conformGuard(b, defs, data, path, visiting, mode)
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		if !exactlyOne {
			return res, nil // anyOf: first match wins
		}
		match = res
		matches++
	}
	if matches == 0 {
		return nil, fmt.Errorf("%svalue does not match any of the permitted variants: %v", pathPrefix(path), firstErr)
	}
	if matches > 1 {
		return nil, fmt.Errorf("%svalue matches %d oneOf variants; exactly one is required", pathPrefix(path), matches)
	}
	return match, nil
}

// checkType verifies data's JSON type is permitted by nd.Type (empty = unconstrained).
// "integer" accepts an integral number; "number" accepts any number.
func checkType(nd *node, data any, path string) error {
	if len(nd.Type) == 0 {
		return nil
	}
	for _, t := range nd.Type {
		if valueHasType(data, t) {
			return nil
		}
	}
	return fmt.Errorf("%sexpected type %s, got %s", pathPrefix(path), strings.Join(nd.Type, "|"), jsonTypeName(data))
}

func valueHasType(data any, t string) bool {
	switch t {
	case "null":
		return data == nil
	case "boolean":
		_, ok := data.(bool)
		return ok
	case "string":
		_, ok := data.(string)
		return ok
	case "object":
		_, ok := data.(map[string]any)
		return ok
	case "array":
		_, ok := data.([]any)
		return ok
	case "number":
		_, ok := asFloat(data)
		return ok
	case "integer":
		return isIntegral(data)
	default:
		return false
	}
}

// checkScalar applies the scalar constraints: numeric range and string length.
func checkScalar(nd *node, data any, path string) error {
	// Bounds compare exactly: through float64 a number just outside a bound can land on it.
	if _, isNum := numeric.ToDecimal(data); isNum {
		if nd.Minimum != nil {
			if c, ok := numeric.Compare(data, *nd.Minimum); ok && c < 0 {
				return fmt.Errorf("%svalue %v is less than minimum %v", pathPrefix(path), data, *nd.Minimum)
			}
		}
		if nd.Maximum != nil {
			if c, ok := numeric.Compare(data, *nd.Maximum); ok && c > 0 {
				return fmt.Errorf("%svalue %v is greater than maximum %v", pathPrefix(path), data, *nd.Maximum)
			}
		}
	}
	if s, ok := data.(string); ok {
		n := utf8.RuneCountInString(s)
		if nd.MinLength != nil && n < *nd.MinLength {
			return fmt.Errorf("%sstring length %d is less than minLength %d", pathPrefix(path), n, *nd.MinLength)
		}
		if nd.MaxLength != nil && n > *nd.MaxLength {
			return fmt.Errorf("%sstring length %d is greater than maxLength %d", pathPrefix(path), n, *nd.MaxLength)
		}
	}
	return nil
}

// propDefault returns the default for a property, following a lone $ref to its target
// when the property node itself carries none.
func propDefault(prop *node, defs map[string]*node) any {
	if prop == nil {
		return nil
	}
	if prop.Default != nil {
		return prop.Default
	}
	if prop.Ref != "" {
		if target, err := deref(prop, defs); err == nil && target != nil {
			return target.Default
		}
	}
	return nil
}

// isObjectSchema reports whether node describes an object (so a map value should
// be pruned to declared properties rather than passed through).
func isObjectSchema(nd *node) bool {
	return nd.Type.Contains("object") || nd.Properties != nil || nd.Required != nil || nd.AdditionalProperties != nil
}

// enumContains reports whether data equals any enum member, compared by JSON encoding so
// 1 and 1.0 (and nested values) compare equal.
func enumContains(enum []any, data any) bool {
	db, err := json.Marshal(data)
	if err != nil {
		return false
	}
	for _, e := range enum {
		// By value, not literal: [1] must accept 1.0, which the marshalled bytes reject.
		if numeric.Equal(e, data) {
			return true
		}
		if eb, err := json.Marshal(e); err == nil && string(eb) == string(db) {
			return true
		}
	}
	return false
}

func asFloat(data any) (float64, bool) {
	switch n := data.(type) {
	case float64:
		return n, true
	case float32:
		return float64(n), true
	case int:
		return float64(n), true
	case int64:
		return float64(n), true
	case int32:
		return float64(n), true
	case json.Number:
		f, err := n.Float64()
		return f, err == nil
	default:
		return 0, false
	}
}

func isIntegral(data any) bool { return numeric.IsIntegral(data) }

// jsonTypeName names data's JSON type for error messages, reusing valueHasType so it
// can't drift from the type check. "integer" is tried before "number" so an integral
// value reads as the more specific kind.
func jsonTypeName(data any) string {
	for _, t := range []string{"null", "boolean", "integer", "number", "string", "array", "object"} {
		if valueHasType(data, t) {
			return t
		}
	}
	return fmt.Sprintf("%T", data)
}

// cloneJSON deep-copies so a filled default is never aliased into two documents; the decode
// keeps exact literals, which a plain Unmarshal would undo.
func cloneJSON(v any) any {
	b, err := json.Marshal(v)
	if err != nil {
		return v
	}
	var out any
	if err := numeric.Decode(b, &out); err != nil {
		return v
	}
	return out
}

func pathPrefix(path string) string {
	if path == "" {
		return ""
	}
	return path + ": "
}

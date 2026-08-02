package schema

import (
	"encoding/json"
	"fmt"
	"genroc/internal/numeric"
	"strings"
	"unicode/utf8"
)

// Validate checks data against the schema and returns a normalized copy: undeclared
// object properties are dropped; an absent declared property is filled from its
// (conformed) default or omitted, and a missing required property is an error; retained
// values are type- and constraint-checked. Types are strict, except an "integer" schema
// accepts any number with no fractional part (JSON decodes all numbers to float64). The
// result shares no maps or slices with the input. A nil or empty {} schema passes data
// through; $refs resolve against the schema's own $defs, so it should be normalized first.
func (s Schema) Validate(data any) (any, error) {
	return conform(s.n, s.rootDefs(), data, "")
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
	return conformGuard(nd, defs, data, path, nil)
}

// conformGuard is conform with a same-position cycle guard: visiting holds the resolved
// nodes already expanded at the current value position (no object/array descent since),
// so a $ref back to one is a schema cycle with no structural progress and the branch
// fails instead of recursing forever. Stored schemas decode without CheckDoc, so the
// validator guards itself. Descending into a property or element starts a fresh set —
// value depth was consumed, so revisiting a node there is productive recursion.
func conformGuard(nd *node, defs map[string]*node, data any, path string, visiting map[*node]bool) (any, error) {
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
		return conformUnion(resolved.AnyOf, defs, data, path, false, visiting)
	}
	if len(resolved.OneOf) > 0 {
		return conformUnion(resolved.OneOf, defs, data, path, true, visiting)
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
			return conformObject(resolved, defs, v, path)
		}
		return v, nil
	case []any:
		if resolved.Items != nil || resolved.Type.Contains("array") {
			return conformArray(resolved, defs, v, path)
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
func conformObject(nd *node, defs map[string]*node, v map[string]any, path string) (any, error) {
	required := make(map[string]bool, len(nd.Required))
	for _, r := range nd.Required {
		required[r] = true
	}
	out := make(map[string]any, len(nd.Properties))
	for name, prop := range nd.Properties {
		val, present := v[name]
		if !present {
			if required[name] {
				return nil, fmt.Errorf("%srequired property %q is missing", pathPrefix(path), name)
			}
			if def := propDefault(prop, defs); def != nil {
				// The default is conformed like a supplied value, so a filled
				// value can never violate the schema and object defaults are
				// normalized (pruned, nested defaults filled) consistently.
				norm, err := conform(prop, defs, cloneJSON(def), JoinPath(path, name))
				if err != nil {
					return nil, fmt.Errorf("invalid schema default: %w", err)
				}
				out[name] = norm
			}
			continue // absent optional without a default is omitted
		}
		norm, err := conform(prop, defs, val, JoinPath(path, name))
		if err != nil {
			return nil, err
		}
		out[name] = norm
	}
	// Undeclared keys: dropped for a closed object; validated against the
	// additionalProperties subschema and kept for an open map.
	if nd.AdditionalProperties != nil {
		for name, val := range v {
			if _, declared := nd.Properties[name]; declared {
				continue
			}
			norm, err := conform(nd.AdditionalProperties, defs, val, JoinPath(path, name))
			if err != nil {
				return nil, err
			}
			out[name] = norm
		}
	}
	return out, nil
}

func conformArray(nd *node, defs map[string]*node, arr []any, path string) (any, error) {
	if nd.MinItems != nil && len(arr) < *nd.MinItems {
		return nil, fmt.Errorf("%sarray has %d items, fewer than minItems %d", pathPrefix(path), len(arr), *nd.MinItems)
	}
	if nd.MaxItems != nil && len(arr) > *nd.MaxItems {
		return nil, fmt.Errorf("%sarray has %d items, more than maxItems %d", pathPrefix(path), len(arr), *nd.MaxItems)
	}
	out := make([]any, len(arr))
	for i, el := range arr {
		norm, err := conform(nd.Items, defs, el, JoinIndex(path, i))
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
func conformUnion(branches []*node, defs map[string]*node, data any, path string, exactlyOne bool, visiting map[*node]bool) (any, error) {
	var (
		firstErr error
		match    any
		matches  int
	)
	for _, b := range branches {
		res, err := conformGuard(b, defs, data, path, visiting)
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
	// Bounds compare exactly. Routing the value through float64 first would let a
	// number just outside a bound land exactly on it, which is the whole class of
	// error this representation exists to remove.
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
		// Numbers match by value, not by literal. The schema's enum entries decode
		// as float64 while runtime data decodes with UseNumber, so an enum declared
		// [1] must still accept an input that arrives as "1.0" — comparing the
		// marshalled bytes alone would silently start rejecting it.
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

// cloneJSON deep-copies a JSON value so a schema default can be handed out
// without sharing mutable state with the schema.
// cloneJSON deep-copies a value so a filled default is never aliased into two
// documents. The decode preserves exact numeric literals — a plain Unmarshal here
// would undo the exactness the schema decode just established, silently rounding
// a large default back to float64.
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

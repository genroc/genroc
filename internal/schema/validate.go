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
func (s Schema) Validate(data any, mode ...ConformMode) (any, error) {
	return conformGuard(s.n, s.rootDefs(), data, "", nil, firstMode(mode))
}

// firstMode reads the optional mode argument, defaulting to Strict so that every existing
// caller — an input, an action result, a collected child output — keeps checking documents
// exactly as before.
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

// ConformMode selects what a walk of schema-and-value is FOR. There is one traversal
// because there is one set of rules about where a value lives inside a schema —
// combinators before types, $refs with a cycle guard, open maps versus closed objects,
// unions picked by which branch the value satisfies — and a second walk beside it would
// have to rediscover all of them and then stay in step forever.
type ConformMode int

const (
	// Strict is the default: a document check at a boundary. An absent required property
	// is rejected whatever its type, and undeclared keys are stripped.
	Strict ConformMode = iota

	// ConformToSchemaExactly turns the walk into a MIGRATION: it reconciles a stored value
	// with a schema it was not written against, so that the result satisfies that schema.
	// It is the other half of IsSubsetAsStored, and the two must accept exactly the same
	// gaps — a relation tolerating more promises a migration that then fails to conform.
	//
	// The whole of the difference from Strict is the null-versus-missing distinction, in
	// BOTH directions, because a version change can open the gap either way:
	//
	//   absent, required, admits null   → the null is written in
	//   present null, optional, admits  → the key is REMOVED; the value cannot stay, and
	//   no null                           the property is optional, so absence is valid
	//
	// Undeclared keys are STRIPPED, as in every other mode: a key the target schema does not
	// name is one nothing on the new version can read -- validation rejects the reference --
	// so keeping it stores weight that only grows, and pins whatever it references in the
	// object store for as long as the instance lives. A caller whose schema is deliberately
	// PARTIAL is the one that must put the rest back; see validation.MigrateState, where the
	// layer describes a definition's own slots and the engine's bookkeeping is not its
	// business.
	//
	// One thing it deliberately does not do. **Defaults are NOT filled**: a default filled at
	// creation is filled before anything reads it, so every value derived from it agrees;
	// filling one into a half-run instance disagrees with the values already computed in its
	// absence, and nothing recomputes them (specs/compat-command.md §2d).
	ConformToSchemaExactly
)

// conformGuard: visiting holds nodes expanded at the current value position, so a $ref
// back to one is a no-progress schema cycle and the branch fails instead of recursing
// forever (stored schemas decode without CheckDoc). Descent into a property/element
// starts fresh — value depth was consumed.
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
	for name, prop := range nd.Properties {
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
				// The default is conformed like a supplied value, so a filled
				// value can never violate the schema and object defaults are
				// normalized (pruned, nested defaults filled) consistently.
				norm, err := conformGuard(prop, defs, cloneJSON(def), JoinPath(path, name), nil, mode)
				if err != nil {
					return nil, fmt.Errorf("invalid schema default: %w", err)
				}
				out[name] = norm
			}
			continue // absent optional without a default is omitted
		}
		// The other direction of the null-versus-missing gap. A stored null that the new
		// schema will not hold cannot stay — but where the property is OPTIONAL, absence is
		// valid, so removing the key reconciles the value instead of failing it. Required
		// and non-nullable is the case nothing can fix, and it falls through to the error.
		if mode == ConformToSchemaExactly && val == nil && !required[name] && !hasNullResolved(prop, defs) {
			continue
		}
		norm, err := conformGuard(prop, defs, val, JoinPath(path, name), nil, mode)
		if err != nil {
			return nil, err
		}
		out[name] = norm
	}
	// Undeclared keys: dropped for a closed object; validated against the
	// additionalProperties subschema and kept for an open map. A migration keeps them
	// either way — stripping is a conform's job, and losing a value nobody declared is
	// exactly what an upgrade must not do.
	for name, val := range v {
		if _, declared := nd.Properties[name]; declared {
			continue
		}
		if nd.AdditionalProperties != nil {
			norm, err := conformGuard(nd.AdditionalProperties, defs, val, JoinPath(path, name), nil, mode)
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

// cloneJSON deep-copies so a filled default is never aliased into two documents, decoding
// with exact numeric literals — a plain Unmarshal here would silently undo the exactness
// the schema decode just established.
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

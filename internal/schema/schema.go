// Package schema normalizes, validates and types a strict subset of JSON Schema: valid JSON
// Schema, minus allOf and anything outside allowedKeywords; {} is the top type. Parse yields a
// Raw for Normalize, whose Schema keeps $defs at the root and carries them into sub-schemas.
package schema

import (
	"encoding/json"
	"fmt"
	"slices"

	"genroc/internal/numeric"
)

// SchemaType holds one or more JSON Schema type strings.
// A single type marshals as a JSON string; multiple types marshal as a JSON array.
type SchemaType []string

func (t SchemaType) MarshalJSON() ([]byte, error) {
	if len(t) == 1 {
		return json.Marshal(t[0])
	}
	return json.Marshal([]string(t))
}

func (t *SchemaType) UnmarshalJSON(data []byte) error {
	var s string
	if err := json.Unmarshal(data, &s); err == nil {
		*t = SchemaType{s}
		return nil
	}
	var arr []string
	if err := json.Unmarshal(data, &arr); err != nil {
		return fmt.Errorf("schema type must be a string or array of strings: %w", err)
	}
	*t = arr
	return nil
}

func (t SchemaType) Contains(s string) bool {
	for _, v := range t {
		if v == s {
			return true
		}
	}
	return false
}

// allowedKeywords is both the allowlist UnmarshalJSON enforces and the editor's vocabulary.
// "secret" is a genroc extension, meaningful only in a config_schema (log redaction).
var allowedKeywords = map[string]keyword{
	"type":                 {"string|array", "The value's JSON type, or a list of types it may take."},
	"properties":           {"object", "The named members of an object, each a schema."},
	"required":             {"array", "Which properties must be present. A property not listed here may be absent, and reading it yields null."},
	"items":                {"object", "The schema every element of an array conforms to."},
	"additionalProperties": {"object", "The schema undeclared keys conform to, making the object an open map. Omit to close the object: undeclared keys are stripped."},
	"oneOf":                {"array", "The value conforms to exactly one of these schemas."},
	"anyOf":                {"array", "The value conforms to at least one of these schemas."},
	"enum":                 {"array", "The complete set of values allowed here."},
	"minimum":              {"number", "Smallest allowed number, inclusive."},
	"maximum":              {"number", "Largest allowed number, inclusive."},
	"minLength":            {"integer", "Shortest allowed string."},
	"maxLength":            {"integer", "Longest allowed string."},
	"minItems":             {"integer", "Fewest allowed array elements."},
	"maxItems":             {"integer", "Most allowed array elements."},
	"$ref":                 {"string", "A reference to another schema, as `#/$defs/<name>`."},
	"$defs":                {"object", "Named schemas this document's $refs resolve against."},
	"$anchor":              {"string", "A name this schema can be referenced by."},
	"$id":                  {"string", "An identifier for this schema."},
	"default":              {"", "The value used when this one is absent. Annotation only — it does not make a required property optional."},
	"secret":               {"boolean", "Redacts the value from logs. Only meaningful in a process config_schema."},
	"description":          {"string", "Free text. Shown in the editor, and never a constraint."},
	// No "allOf": navigation cannot resolve a member through an intersection. node.AllOf is
	// only normalization's ref-bundling vehicle.
}

// JSONSchemaBytes is hand-written because `node` is unexported. Sub-schemas stay permissive
// rather than self-$ref -- openapi-typescript turns that into a cycle tsc rejects.
func (Schema) JSONSchemaBytes() ([]byte, error) {
	props := make(map[string]any, len(allowedKeywords))
	for name, k := range allowedKeywords {
		// Description only: openapi-typescript turns a `type` here into a def the generated
		// client cannot satisfy. KeywordKind carries the kind instead.
		props[name] = map[string]any{"description": k.description}
	}
	return json.Marshal(map[string]any{
		"type":                 "object",
		"description":          "A JSON Schema, in the subset genroc supports.",
		"properties":           props,
		"additionalProperties": false,
	})
}

// validTypes is the JSON Schema "simpleTypes" enum. Unlisted names are rejected by checkDoc,
// not here, so a definition already stored with one stays decodable.
var validTypes = map[string]bool{
	"null": true, "boolean": true, "string": true, "number": true,
	"integer": true, "object": true, "array": true,
}

// TypeNames is the complete set a `type` may name, in the order a reader meets them: the
// scalars first, then the two that hold other values.
func TypeNames() []string {
	return []string{"string", "number", "integer", "boolean", "null", "object", "array"}
}

// keyword is what one JSON Schema keyword is: the kind of value it takes, shown beside it in
// an editor's completion list, and what it means.
type keyword struct {
	kind        string
	description string
}

// keywordOrder is how a schema reads; json and yaml.v3 sort map keys, so anything showing
// keywords to a person orders by this. A keyword absent here follows, sorted.
var keywordOrder = []string{
	"description", "$ref", "type", "oneOf", "anyOf", "allOf", "enum", "default",
	"properties", "required", "additionalProperties", "items",
	"minimum", "maximum", "minLength", "maxLength", "minItems", "maxItems",
	"secret", "$anchor", "$id", "$defs",
}

// KeywordOrder is the order keywords are read in, for a caller that prints or offers them.
func KeywordOrder() []string { return slices.Clone(keywordOrder) }

// KeywordRank is a keyword's place in that order, or -1 for a word the order does not name.
func KeywordRank(name string) int { return slices.Index(keywordOrder, name) }

// KeywordKind is the kind of value a keyword takes, or "" for one this package does not
// accept. The published schema cannot carry it (see JSONSchemaBytes).
func KeywordKind(name string) string { return allowedKeywords[name].kind }

// node's fields are exported only for encoding/json; callers hold a Raw or a Schema.
type node struct {
	Type SchemaType `json:"type,omitempty"`
	// Description is never a constraint: canonicalizeNode strips it so the inference
	// fixpoint, which compares canonical JSON, sees equal types as equal.
	Description string           `json:"description,omitempty"`
	Properties  map[string]*node `json:"properties,omitempty"`
	Required    []string         `json:"required,omitempty"`
	// AdditionalProperties nil is a closed object (undeclared keys stripped), non-nil an
	// open map. The boolean form is rejected at parse.
	AdditionalProperties *node            `json:"additionalProperties,omitempty"`
	Items                *node            `json:"items,omitempty"`
	OneOf                []*node          `json:"oneOf,omitempty"`
	AnyOf                []*node          `json:"anyOf,omitempty"`
	AllOf                []*node          `json:"allOf,omitempty"`
	Enum                 []any            `json:"enum,omitempty"`
	Minimum              *float64         `json:"minimum,omitempty"`
	Maximum              *float64         `json:"maximum,omitempty"`
	MinLength            *int             `json:"minLength,omitempty"`
	MaxLength            *int             `json:"maxLength,omitempty"`
	MinItems             *int             `json:"minItems,omitempty"`
	MaxItems             *int             `json:"maxItems,omitempty"`
	Ref                  string           `json:"$ref,omitempty"`
	Defs                 map[string]*node `json:"$defs,omitempty"`
	Anchor               string           `json:"$anchor,omitempty"`
	ID                   string           `json:"$id,omitempty"`
	Default              any              `json:"default,omitempty"`
	Secret               bool             `json:"secret,omitempty"`
	// pending routes a solver sentinel to its solver so deref can force it on demand. A JSON
	// round-trip drops it and Solve nils it, so an escaped sentinel fails loudly on
	// pendingAnchor rather than reading as {}.
	pending *pendingEntry
}

// UnmarshalJSON checks shape and keywords first (checkRawNode), so a mistake is reported at
// the sub-schema holding it, not as encoding/json's error against the outermost slot.
func (n *node) UnmarshalJSON(data []byte) error {
	if err := checkRawNode(data); err != nil {
		return err
	}
	// Not a plain Unmarshal: default/enum are any-typed, and float64 corrupts a big default
	// and inverts an enum.
	type alias node
	return numeric.Decode(data, (*alias)(n))
}

// ─── Raw: the unnormalized document ─────────────────────────────────────────────

// Raw is an unnormalized schema (nested $defs, $id, anchor refs): it can only be normalized
// or round-tripped, since its $refs are not yet resolved against a single root.
type Raw struct {
	n *node
}

// Parse parses a JSON-encoded schema into an unnormalized Raw (strict keyword
// allowlist). Call Normalize on the result to obtain an operable Schema.
func Parse(data []byte) (Raw, error) {
	var n node
	if err := json.Unmarshal(data, &n); err != nil {
		return Raw{}, err
	}
	return Raw{&n}, nil
}

// Normalize flattens all $defs to the root, drops unused definitions, rewrites
// $refs to their flat locations, and returns the result as a Schema. The receiver
// is not modified.
func (r Raw) Normalize() (Schema, error) {
	cloned, err := deepClone(r.n)
	if err != nil {
		return Schema{}, err
	}
	out, err := normalize(cloned)
	if err != nil {
		return Schema{}, err
	}
	if out == nil {
		out = &node{}
	}
	return Schema{n: out}, nil
}

// AssumeNormalized wraps the parsed document as a Schema without normalizing it — an
// escape hatch for input known to already be normalized (defs only at the root), e.g. a
// schema this package marshaled earlier. Prefer Normalize when in doubt; it is idempotent.
func (r Raw) AssumeNormalized() Schema {
	if r.n == nil {
		return Schema{n: &node{}}
	}
	return Schema{n: r.n}
}

// CheckDoc reports whether the document is structurally well-formed in the
// supported subset (see Schema.CheckDoc).
func (r Raw) CheckDoc() error {
	return checkDocRoot(r.n)
}

// MarshalJSON emits the raw schema (including any nested $defs) unchanged.
func (r Raw) MarshalJSON() ([]byte, error) {
	return json.Marshal(r.n)
}

// UnmarshalJSON parses a schema document with the strict keyword allowlist.
func (r *Raw) UnmarshalJSON(data []byte) error {
	var n node
	if err := json.Unmarshal(data, &n); err != nil {
		return err
	}
	r.n = &n
	return nil
}

// JSONSchemaBytes returns a permissive JSON Schema for OpenAPI reflection.
func (Raw) JSONSchemaBytes() ([]byte, error) {
	return []byte(`{"type":"object","additionalProperties":true}`), nil
}

// ─── Schema: the normalized, operable schema ────────────────────────────────────

// Schema is normalized: its root $defs resolve every $ref in the tree, and navigation returns
// sub-schemas carrying them. Builders never mutate the receiver.
type Schema struct {
	n *node
	// guards are refinements already proved here (a `switch` case's edge). Not part of the
	// schema: dropped by every constructor, never marshalled or compared. specs/guard-narrowing.md.
	guards map[string]guard
	// vars are lambda parameters in scope (LambdaVars), shadowing the context's roots; carried
	// and dropped like guards.
	vars map[string]Schema
}

// WithGuards returns s carrying refinements proved about some of its references. Keys are
// rendered access paths (`outputs.a.v`), values the narrowed type.
func (s Schema) WithGuards(narrowed map[string]Schema) Schema {
	s.guards = seedGuards(narrowed)
	return s
}

// WithVars returns s carrying lambda parameter bindings in scope, as if the expression about to
// be typed were written inside the body that binds them. LambdaVars is what produces them.
func (s Schema) WithVars(vars map[string]Schema) Schema {
	s.vars = vars
	return s
}

// wrap pairs a node with the pool it resolves against: the package-internal form of WithDefs,
// for code that already holds the raw map rather than a Defs handle. Every navigation and
// transform returns through it, so a node never leaves the package without its pool.
func wrap(n *node, defs map[string]*node) Schema {
	if n == nil {
		return Schema{n: &node{Defs: defs}}
	}
	m := *n
	m.Defs = defs
	return Schema{n: &m}
}

// Load wraps a raw schema map as a Schema, silently dropping unrecognised keywords
// via a JSON roundtrip. Intended for programmatic construction of already-flat
// schemas; use Parse for user-supplied JSON.
func Load(raw map[string]any) Schema {
	if len(raw) == 0 {
		return Schema{n: &node{}}
	}
	b, err := json.Marshal(raw)
	if err != nil {
		return Schema{n: &node{}}
	}
	type alias node // bypass strict UnmarshalJSON
	var a alias
	if err := json.Unmarshal(b, &a); err != nil {
		return Schema{n: &node{}}
	}
	n := node(a)
	return Schema{n: &n}
}

// MarshalJSON emits the schema with its root $defs.
func (s Schema) MarshalJSON() ([]byte, error) {
	return json.Marshal(s.n)
}

// UnmarshalJSON decodes with the strict allowlist but does not normalize: it is for schemas
// this package produced. Use Parse + Normalize for untrusted input.
func (s *Schema) UnmarshalJSON(data []byte) error {
	var n node
	if err := json.Unmarshal(data, &n); err != nil {
		return err
	}
	s.n = &n
	return nil
}

// AsMap returns the schema as a plain map. Intended for compatibility and testing;
// avoid in new code.
func (s Schema) AsMap() map[string]any {
	if s.n == nil {
		return nil
	}
	b, err := json.Marshal(s.n)
	if err != nil {
		return nil
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		return nil
	}
	return m
}

// IsZero reports whether s is the zero Schema (no underlying document, "no schema
// declared"), as opposed to IsNull (the schema {type:"null"}). It is also the hook
// encoding/json's `omitzero` calls to drop absent value fields.
func (s Schema) IsZero() bool {
	return s.n == nil
}

// Normalize re-normalizes (idempotent) into a fresh Schema; the receiver is not modified.
func (s Schema) Normalize() (Schema, error) {
	return Raw{s.n}.Normalize()
}

// CheckDoc reports whether the schema is structurally well-formed in the supported
// subset (see checkDocRoot).
func (s Schema) CheckDoc() error {
	return checkDocRoot(s.n)
}

// rootDefs returns the resolution context, nil-safe against a zero Schema.
func (s Schema) rootDefs() map[string]*node {
	if s.n == nil {
		return nil
	}
	return s.n.Defs
}

// ─── Navigation ─────────────────────────────────────────────────────────────────

// At navigates a dot-path (e.g. "user.issues[0].value") and returns the subschema at
// that path, carrying the same root $defs so it stays navigable/validatable. For the
// type of a full expression rather than a plain sub-path, see Infer.
func (s Schema) At(path string) (Schema, error) {
	return s.subSchema(navigate(s.n, s.rootDefs(), path))
}

// Property is one step of At: an optional property comes back nullable.
func (s Schema) Property(name string) (Schema, error) {
	return s.subSchema(lookupProperty(s.n, name, s.rootDefs()))
}

// Index returns the (nullable) element subschema for array index access, carrying
// the same root $defs. Always nullable because the index may be out of bounds.
func (s Schema) Index() (Schema, error) {
	return s.subSchema(inferIndex(s.n, s.rootDefs()))
}

// AnyKey returns the nullable type a computed key a[expr] reads and the type the key must have:
// an array's element (integer) or an additionalProperties-only map's value (string).
func (s Schema) AnyKey() (Schema, string, error) {
	value, keyType, err := anyKey(s.n, s.rootDefs())
	if err != nil {
		return Schema{}, "", err
	}
	sub, err := s.subSchema(value, nil)
	return sub, keyType, err
}

// subSchema wraps a one-step navigation result as a Schema carrying the parent's $defs,
// so it resolves $refs against the same root, threading the navigation error through.
func (s Schema) subSchema(n *node, err error) (Schema, error) {
	if err != nil {
		return Schema{}, err
	}
	return wrap(n, s.rootDefs()), nil
}

// ─── internal ───────────────────────────────────────────────────────────────────

// deepClone returns a fully independent copy via JSON roundtrip.
func deepClone(n *node) (*node, error) {
	if n == nil {
		return nil, nil
	}
	b, err := json.Marshal(n)
	if err != nil {
		return nil, err
	}
	// alias skips the strict UnmarshalJSON on already-valid data; the decode must still keep
	// exact literals, or cloning rounds defaults and enums through float64.
	type alias node
	var a alias
	if err := numeric.Decode(b, &a); err != nil {
		return nil, err
	}
	result := node(a)
	return &result, nil
}

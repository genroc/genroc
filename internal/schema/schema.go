// Package schema provides a normalizer, validator, and type helpers for a strict subset of
// JSON Schema: everything it accepts is valid JSON Schema, but it accepts less. Keywords
// outside allowedKeywords fail to unmarshal; an unrecognised type name is caught a step later
// by CheckDoc. The empty node {} is the top type as authored — specs/unknown-type.md.
//
// Two value types: Parse yields a Raw (unnormalized, possibly nested $defs and unresolved
// anchors) whose only use is Normalize, which yields a Schema — the operating type, where
// $defs live only at the root and everything is a method. Sub-schemas from navigation carry
// the root $defs, so they stay resolvable at any depth.
//
// allOf is NOT accepted: navigation cannot resolve a member through an intersection, so it
// would be a half-supported keyword. The AllOf field is only normalization's internal vehicle
// for bundling refs (FlattenNamed) and is never populated from user JSON.
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

// allowedKeywords is BOTH the allowlist UnmarshalJSON enforces and the schema an editor
// completes from (JSONSchemaBytes), so a keyword cannot be accepted without being offered.
// "secret" is a genroc extension, meaningful only inside a config_schema (it drives log
// redaction) and ignored elsewhere.
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
	// "allOf" is intentionally omitted — see the package doc.
}

// JSONSchemaBytes describes the JSON Schema subset a user may write. Hand-written because
// `node` is unexported, so reflection over Schema sees no fields. Sub-schemas stay permissive
// rather than recursing into this def -- openapi-typescript turns a self-$ref into an eager
// cycle tsc rejects -- so completion is top-level only.
func (Schema) JSONSchemaBytes() ([]byte, error) {
	props := make(map[string]any, len(allowedKeywords))
	for name, k := range allowedKeywords {
		// Description only. A `type` here would be right, and openapi-typescript turns the
		// enriched def into one the generated client cannot satisfy — the same trap an `enum`
		// on `type` sprang. KeywordKind carries the kind to a consumer that wants it instead.
		props[name] = map[string]any{"description": k.description}
	}
	return json.Marshal(map[string]any{
		"type":                 "object",
		"description":          "A JSON Schema, in the subset genroc supports.",
		"properties":           props,
		"additionalProperties": false,
	})
}

// validTypes is the JSON Schema "simpleTypes" enum — the only names a type may take.
// Unlisted names are rejected by checkDoc, not here, so that a definition already
// stored with one stays decodable; see the note at that check.
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

// keywordOrder is how a schema READS — what it is, then what it holds, then the pool it
// resolves against. Both json and yaml.v3 sort map keys instead, so every consumer that shows
// keywords to a person orders by this. A keyword absent here follows, sorted, so a new one
// shows up rather than disappearing.
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

// KeywordKind names the kind of value a JSON Schema keyword takes, or "" for a word this
// package does not accept. It is what an editor shows beside the keyword in a completion list;
// the published schema cannot carry it (see JSONSchemaBytes).
func KeywordKind(name string) string { return allowedKeywords[name].kind }

// node is the structural representation of the supported JSON Schema subset. It is
// unexported: callers hold a Raw or a Schema and use their methods. The fields stay
// exported so encoding/json (and in-package code) can reach them.
// Any JSON key absent from allowedKeywords causes an UnmarshalJSON error.
type node struct {
	Type SchemaType `json:"type,omitempty"`
	// Description is a free-text annotation with no bearing on type-checking: it is preserved
	// through parse/normalize/store and shown in the editor, but stripped by canonicalizeNode
	// so two schemas denoting the same type stay equal (the recursive-inference fixpoint
	// compares canonical JSON). Like a JSON Schema "description", never a constraint.
	Description string           `json:"description,omitempty"`
	Properties  map[string]*node `json:"properties,omitempty"`
	Required    []string         `json:"required,omitempty"`
	// AdditionalProperties, when non-nil, types the object's undeclared keys as an
	// open map (each extra value must conform to this subschema, and survives
	// normalization). nil = closed object (undeclared keys stripped). Only the schema
	// form is supported; a boolean additionalProperties is rejected at parse time.
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
	// pending routes a solver sentinel back to the solver that owns it, so deref can
	// force the definition on demand (see Solver.Declare). Struct copies may carry it —
	// a copy denotes the same definition and resolving it is correct. Unexported, so a
	// JSON round-trip drops it; that plus Solve nilling it is what leaves an escaped
	// sentinel bare, to fail loudly on pendingAnchor rather than read as permissive {}.
	pending *pendingEntry
}

// UnmarshalJSON implements strict decoding: the document's shape and keywords are checked
// first (checkRawNode), so a mistake is reported against the sub-schema holding it rather
// than as encoding/json's type error against the outermost slot.
func (n *node) UnmarshalJSON(data []byte) error {
	if err := checkRawNode(data); err != nil {
		return err
	}
	// Decode preserving exact numeric literals: default/enum are any-typed, and a plain
	// Unmarshal collapsed them to float64 — corrupting a big default and INVERTING an enum
	// (the whitelist for 9007199254740993 rejected it and admitted its neighbour).
	type alias node
	return numeric.Decode(data, (*alias)(n))
}

// ─── Raw: the unnormalized document ─────────────────────────────────────────────

// Raw is an unnormalized parsed schema: it may carry nested $defs, $id resources,
// and anchor-style $refs. The only operations are Normalize (yielding the operating
// Schema type) and JSON round-tripping — a Raw cannot be validated or navigated,
// because its $refs are not yet resolved against a single root.
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
	return Schema{out}, nil
}

// AssumeNormalized wraps the parsed document as a Schema without normalizing it — an
// escape hatch for input known to already be normalized (defs only at the root), e.g. a
// schema this package marshaled earlier. Prefer Normalize when in doubt; it is idempotent.
func (r Raw) AssumeNormalized() Schema {
	if r.n == nil {
		return Schema{&node{}}
	}
	return Schema{r.n}
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

// Schema is a normalized JSON Schema you operate on: a root node whose $defs are the sole
// resolution context for every $ref in the tree. Navigation (Infer/At/Property/Index) returns
// a Schema carrying those same $defs, so a sub-schema stays fully resolvable; builders never
// mutate their receiver.
type Schema struct {
	n *node
}

// wrap builds a Schema whose node is n but whose resolution context is the given
// defs map. TEMPORARY migration shim — use Schema.WithDefs / Defs instead.
func wrap(n *node, defs map[string]*node) Schema {
	if n == nil {
		return Schema{&node{Defs: defs}}
	}
	m := *n
	m.Defs = defs
	return Schema{&m}
}

// Load wraps a raw schema map as a Schema, silently dropping unrecognised keywords
// via a JSON roundtrip. Intended for programmatic construction of already-flat
// schemas; use Parse for user-supplied JSON.
func Load(raw map[string]any) Schema {
	if len(raw) == 0 {
		return Schema{&node{}}
	}
	b, err := json.Marshal(raw)
	if err != nil {
		return Schema{&node{}}
	}
	type alias node // bypass strict UnmarshalJSON
	var a alias
	if err := json.Unmarshal(b, &a); err != nil {
		return Schema{&node{}}
	}
	n := node(a)
	return Schema{&n}
}

// MarshalJSON emits the schema with its root $defs.
func (s Schema) MarshalJSON() ([]byte, error) {
	return json.Marshal(s.n)
}

// UnmarshalJSON parses a schema document with the strict keyword allowlist. The
// caller is responsible for the content being normalized (this is the decode path
// for schemas this package produced — e.g. stored definitions); parse untrusted
// input via Parse + Normalize instead.
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

// Normalize re-normalizes the schema (idempotent when already normalized) and
// returns a fresh Schema. Handy when a schema was assembled from parts that may
// still carry nested defs. The receiver is not modified.
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

// Property returns the subschema for a single named property, carrying the same
// root $defs. An optional property comes back nullable, matching At's per-step
// semantics. It is the single-step form of At used by the type inferrer.
func (s Schema) Property(name string) (Schema, error) {
	return s.subSchema(lookupProperty(s.n, name, s.rootDefs()))
}

// Index returns the (nullable) element subschema for array index access, carrying
// the same root $defs. Always nullable because the index may be out of bounds.
func (s Schema) Index() (Schema, error) {
	return s.subSchema(inferIndex(s.n, s.rootDefs()))
}

// AnyKey returns the (nullable) subschema a computed key a[expr] reads and the type
// that key must have: the element type of an array (integer key), or the value type
// of a map declaring only additionalProperties (string key). An object with declared
// properties is an error — its type varies per key.
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
	// Use alias to avoid the strict UnmarshalJSON on a round-trip of already-valid
	// data. The decode still has to preserve exact literals, or cloning a schema
	// would quietly round its defaults and enum entries back through float64.
	type alias node
	var a alias
	if err := numeric.Decode(b, &a); err != nil {
		return nil, err
	}
	result := node(a)
	return &result, nil
}

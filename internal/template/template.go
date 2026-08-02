// Package template parses and evaluates template strings.
//
// Modes:
//   - Typed expression "$: expr": the leaf is one expression whose result type is
//     preserved (leading whitespace before the marker is tolerated). Bypasses block
//     splitting; this is the type-preserving form for a data leaf.
//   - Plain string (no ${ }): returned as a string literal.
//   - Interpolation "text${expr}text": each ${expr} is evaluated, must be
//     string/number/bool, and is stringified and concatenated with the literal text.
//     Interpolation always yields a string — to preserve a value's type use $:.
//
// Escaping uses $-doubling (never a backslash, so it does not collide with JSON/YAML
// string escaping): in literal text "$$" is a literal "$", so "$${" is a literal "${" and
// a leaf-leading "$$:" is a literal "$:". Escaping is a template-layer concern — inside a
// ${ } or $: body the raw source is handed to the expression lexer, which does its own.
//
// A template is parsed once into a Template — literal chunks interleaved with
// parsed expression ASTs — and then evaluated or type-inferred against a context
// any number of times. Which mode applies is fixed at parse time rather than
// re-derived per call. Get memoises parsing, since templates are static strings
// carried on process definitions.
//
// Where an interpolation ends is decided by the expression parser, not by scanning
// for the next "}": at each "${" the candidate terminators are tried in order and
// the first body that parses wins. A "}" nested inside an object literal
// (${ {a: {b: 1}} }) or inside a string literal (${ "x}y" }) therefore does not end
// the block, because a candidate that cuts through either fails to parse. That keeps
// the lexical rules in one place instead of duplicating a string-and-bracket scanner
// here, where it could silently drift.
package template

import (
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"

	"genroc/internal/expression"
	"genroc/internal/expression/syntax"
	"genroc/internal/schema"
)

// Template is a parsed template. The zero value is not usable; obtain one from Parse
// or Get. A Template is immutable and safe for concurrent use.
type Template struct {
	src    string
	chunks []chunk
	// expr marks a $: leaf — one typed expression — so evaluation returns the raw
	// value (preserving its type) instead of stringifying it. Interpolation (${ })
	// always stringifies, so it never sets this.
	expr bool
}

// chunk is either literal text (node == nil) or a parsed expression, in which
// case text holds its source for error messages.
type chunk struct {
	text string
	node syntax.Node
}

// exprMarker prefixes a leaf that is a single typed expression: everything after it
// (trimmed) is one expression whose result type is preserved, bypassing block
// splitting and stringification. A leaf is a $: expression when its first
// non-whitespace content is this marker (unescaped).
const exprMarker = "$:"

// leadingWS returns the byte length of s's leading ASCII whitespace.
func leadingWS(s string) int {
	i := 0
	for i < len(s) && (s[i] == ' ' || s[i] == '\t' || s[i] == '\r' || s[i] == '\n') {
		i++
	}
	return i
}

// Parse splits s into literal and expression chunks. A leaf whose first non-whitespace
// content is an unescaped "$:" is one typed expression (type-preserving); everything else
// is a template whose ${ } interpolations are stringified into the surrounding text.
//
// Escaping uses $-doubling, not a backslash, so it never collides with JSON or YAML string
// escaping ('$' is not an escape character in either, in any quoting style). In literal
// text "$$" renders one literal "$": thus "$${" is a literal "${", and a leaf-leading
// "$$:" is a literal "$:". A single "$" not forming a marker is already literal, so no
// escape is needed there. Escaping applies only to literal text — inside a ${ } or $: body
// the raw source is handed to the expression lexer, which does its own escapes.
func Parse(s string) (*Template, error) {
	ws := leadingWS(s)
	if body, ok := strings.CutPrefix(s[ws:], exprMarker); ok {
		body = strings.TrimSpace(body)
		node, err := syntax.Parse(body)
		if err != nil {
			return nil, fmt.Errorf("expression %q: %w", body, err)
		}
		return &Template{src: s, chunks: []chunk{{text: body, node: node}}, expr: true}, nil
	}
	return scanTemplate(s)
}

// scanTemplate splits a template into literal and ${ } expression chunks, collapsing a
// doubled "$$" in literal text to a single literal "$" (see Parse).
func scanTemplate(s string) (*Template, error) {
	t := &Template{src: s}
	var lit []byte
	flush := func() {
		if len(lit) > 0 {
			t.chunks = append(t.chunks, chunk{text: string(lit)})
			lit = lit[:0]
		}
	}
	for i := 0; i < len(s); {
		if s[i] == '$' && i+1 < len(s) {
			switch s[i+1] {
			case '$':
				lit = append(lit, '$') // $$ -> literal $ (so $${ is a literal ${)
				i += 2
				continue
			case '{':
				flush()
				body, node, after, err := parseBlock(s[i+2:])
				if err != nil {
					return nil, fmt.Errorf("template %q: %w", s, err)
				}
				t.chunks = append(t.chunks, chunk{text: body, node: node})
				i = len(s) - len(after) // after is a suffix of s; advance past the block
				continue
			}
		}
		lit = append(lit, s[i])
		i++
	}
	flush()
	return t, nil
}

// parseBlock splits one ${ } interpolation off the front of s, which starts just past
// the opening "${". Each "}" is tried as the terminator in order and the first body
// that parses wins, so a "}" inside a nested literal or a string does not end the
// block: a candidate that cuts mid-string leaves the string unterminated, which is
// itself a parse error, and the scan moves on.
//
// Shortest-match is sound. For a longer body to have been intended it would have to
// parse too, which requires the intervening "}" to sit inside brackets or a string
// — and in both cases the shorter candidate fails to parse.
func parseBlock(s string) (expr string, node syntax.Node, rest string, err error) {
	var longest error
	for at := 0; ; {
		end := strings.Index(s[at:], "}")
		if end == -1 {
			if longest == nil {
				return "", nil, "", errors.New("unclosed ${")
			}
			// Every candidate failed. Report the longest one's error: a truncated
			// candidate dies with an uninformative "unexpected EOF", while the full
			// body dies with the syntax error the author actually needs to see.
			return "", nil, "", longest
		}
		end += at
		body := s[:end]
		node, perr := syntax.Parse(body)
		if perr == nil {
			return body, node, s[end+1:], nil
		}
		longest = fmt.Errorf("expression %q: %w", body, perr)
		at = end + 1
	}
}

func (t *Template) Source() string { return t.src }

// Static returns the template's constant value and true when it is a pure literal — not a
// $: expression and free of any ${ } interpolation — so its value is fixed at authoring
// time without a context. Callers use it to statically validate hand-written literals
// (e.g. a fetch accepted_status pattern) while leaving dynamic leaves for runtime.
func (t *Template) Static() (string, bool) {
	if t.expr {
		return "", false
	}
	var b strings.Builder
	for _, c := range t.chunks {
		if c.node != nil {
			return "", false
		}
		b.WriteString(c.text)
	}
	return b.String(), true
}

// IsExpr reports whether the leaf is a single $: typed expression rather than a template.
// Together with Static this gives the three-way split a slot needs when a literal and an
// expression mean different things in it — pure literal, $: expression, ${ } interpolation
// — decidable syntactically, before any inference runs. The delay `for` / `until` slots
// are the case that motivates exposing it: a literal is parsed at registration, a $: leaf
// must infer to a number, and an interpolation is rejected outright.
func (t *Template) IsExpr() bool { return t.expr }

// EvalAny evaluates the template against ctx. A $: expression returns the raw value,
// preserving its type; a template stringifies each ${ } interpolation and concatenates
// it with the literal text.
func (t *Template) EvalAny(ctx map[string]any) (any, error) {
	if t.expr {
		val, err := expression.EvalNode(t.chunks[0].node, ctx)
		if err != nil {
			return nil, fmt.Errorf("template %q: %w", t.src, err)
		}
		return val, nil
	}
	var out strings.Builder
	for _, c := range t.chunks {
		if c.node == nil {
			out.WriteString(c.text)
			continue
		}
		val, err := expression.EvalNode(c.node, ctx)
		if err != nil {
			return nil, fmt.Errorf("template expression %q: %w", c.text, err)
		}
		str, err := stringify(val)
		if err != nil {
			return nil, fmt.Errorf("template expression %q: %w", c.text, err)
		}
		out.WriteString(str)
	}
	return out.String(), nil
}

// InferType returns the static type of the template's result against sc.
func (t *Template) InferType(sc schema.Schema) (schema.Schema, error) {
	if t.expr {
		return sc.InferNode(t.chunks[0].node)
	}
	// A template always yields a string, but a null interpolation would silently
	// stringify to "null" — reject it so the author adds a ?? default.
	for _, c := range t.chunks {
		if c.node == nil {
			continue
		}
		inferred, err := sc.InferNode(c.node)
		if err != nil {
			return schema.Schema{}, fmt.Errorf("template expression %q: %w", c.text, err)
		}
		if inferred.HasNull() {
			return schema.Schema{}, fmt.Errorf("template expression %q may be null; use ?? to provide a default value", c.text)
		}
		// stringify accepts only string/number/bool, so an array or object here is a
		// guaranteed runtime failure. Catch it at registration instead: IsType is
		// "resolves uniformly to", so this fires only when the value provably cannot
		// be interpolated, never on a type we merely cannot pin down.
		if inferred.IsType("array") || inferred.IsType("object") {
			return schema.Schema{}, fmt.Errorf("template expression %q is %s; only string, number and boolean values can be interpolated into surrounding text", c.text, inferred.TypeName())
		}
	}
	return schema.Type("string"), nil
}

// ReferencesSecret reports whether any embedded expression reads a secret value
// (see schema.Schema.ReferencesSecret); a plain string is never secret.
func (t *Template) ReferencesSecret(sc schema.Schema) bool {
	for _, c := range t.chunks {
		if c.node != nil && sc.ReferencesSecretNode(c.node) {
			return true
		}
	}
	return false
}

// RootRefs reports which context roots the embedded expressions read, merged across
// every block, so the engine lazily resolves only the value-slots a template needs.
func (t *Template) RootRefs() expression.Roots {
	var out expression.Roots
	for _, c := range t.chunks {
		if c.node == nil {
			continue
		}
		r := expression.RootRefsNode(c.node)
		out.Input = out.Input || r.Input
		out.Error = out.Error || r.Error
		out.AllOutputs = out.AllOutputs || r.AllOutputs
		out.Outputs = append(out.Outputs, r.Outputs...)
		out.SelfPrevious = out.SelfPrevious || r.SelfPrevious
		out.SelfResult = out.SelfResult || r.SelfResult
	}
	return out
}

// OutputRefs returns the distinct outputs.<id> task ids across every block, for the
// output-dependency graph.
func (t *Template) OutputRefs() []string {
	set := map[string]struct{}{}
	for _, c := range t.chunks {
		if c.node == nil {
			continue
		}
		for _, id := range expression.OutputRefsNode(c.node) {
			set[id] = struct{}{}
		}
	}
	ids := make([]string, 0, len(set))
	for id := range set {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

// cache memoises Parse. Template strings are static content carried on process
// definitions, so the key set is bounded by the registered definitions — the same
// bound the DB's definition cache already accepts. Failures are cached too, so a
// definition that somehow reaches the engine with a bad template does not re-parse
// on every tick.
var cache sync.Map // string -> parsed

type parsed struct {
	t   *Template
	err error
}

// Get returns the parsed form of s, parsing on first use. The returned Template is
// shared across callers and must not be mutated.
func Get(s string) (*Template, error) {
	if v, ok := cache.Load(s); ok {
		p := v.(parsed)
		return p.t, p.err
	}
	t, err := Parse(s)
	cache.Store(s, parsed{t: t, err: err})
	return t, err
}

func stringify(v any) (string, error) {
	switch val := v.(type) {
	case string:
		return val, nil
	case json.Number:
		// Already the exact literal; rendering it any other way would reintroduce
		// the float64 rounding this representation exists to avoid.
		return val.String(), nil
	case int:
		return fmt.Sprintf("%d", val), nil
	case int64:
		return fmt.Sprintf("%d", val), nil
	case float32:
		return fmt.Sprintf("%g", float64(val)), nil
	case float64:
		return fmt.Sprintf("%g", val), nil
	case bool:
		if val {
			return "true", nil
		}
		return "false", nil
	default:
		return "", fmt.Errorf("cannot stringify %T in interpolation (only string, number, bool allowed)", v)
	}
}

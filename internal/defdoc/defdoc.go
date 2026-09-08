// Package defdoc parses a definition document into the JSON-native values the model decodes
// from, and records where in the source each part of it was written.
//
// One walk produces both. A second walk would have to repeat the merge-key precedence rule to
// stay aligned with the first, and a location that disagrees with the value it locates is
// worse than no location. specs/language-server.md §3.
package defdoc

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"gopkg.in/yaml.v3"
)

// Range is a span of source text in yaml.v3's own coordinates: 1-based line, 1-based column.
// The LSP converts to its own 0-based UTF-16 positions; nothing else needs to.
//
// The end is approximate for a quoted or block scalar — it is an underline, not a parse.
type Range struct {
	Line, Col       int
	EndLine, EndCol int
}

func (r Range) Empty() bool { return r.Line == 0 }

// Span is where one value was written. Key is the mapping key that introduced it, and is
// empty when nothing introduced it by name: a sequence element, or the document root.
//
// Both are kept because a diagnostic chooses: an unknown key underlines the key, a value that
// failed its rule underlines the value.
type Span struct {
	Key   Range
	Value Range
}

// Doc is one parsed document.
type Doc struct {
	Value any
	spans map[string]Span
}

// Span returns where path was written. Two spellings address the same node:
//
//	physical   tasks[0].on_error[1].case
//	logical    tasks.fetch.on_error.1.case
//
// The physical one is what a validator namespace produces (model.FieldError.Field); the
// logical one is the slot-address grammar of specs/schema-command.md, which names a task by
// its id and so survives a task being inserted above it. Both are registered, because a
// conversion between them is a rule that can be wrong, and a second map entry cannot.
func (d *Doc) Span(path string) (Span, bool) {
	s, ok := d.spans[path]
	return s, ok
}

// Paths returns every addressable path, unordered. For tests and for "did you mean".
func (d *Doc) Paths() []string {
	out := make([]string, 0, len(d.spans))
	for p := range d.spans {
		out = append(out, p)
	}
	return out
}

// Parse reads a single document. An empty input yields a Doc whose Value is nil.
func Parse(data []byte) (*Doc, error) {
	var n yaml.Node
	if err := yaml.Unmarshal(data, &n); err != nil {
		return nil, err
	}
	return build(&n)
}

// ParseAll reads a multi-document stream. Documents that are entirely empty are dropped, so
// a trailing `---` does not become a nil definition.
func ParseAll(data []byte) ([]*Doc, error) {
	var out []*Doc
	dec := yaml.NewDecoder(bytes.NewReader(data))
	for {
		var n yaml.Node
		if err := dec.Decode(&n); err != nil {
			if err == io.EOF {
				return out, nil
			}
			return nil, err
		}
		d, err := build(&n)
		if err != nil {
			return nil, err
		}
		if d.Value == nil {
			continue
		}
		out = append(out, d)
	}
}

func build(n *yaml.Node) (*Doc, error) {
	d := &Doc{spans: map[string]Span{}}
	root := n
	if root.Kind == yaml.DocumentNode {
		if len(root.Content) == 0 {
			return d, nil
		}
		root = root.Content[0]
	}
	v, _, err := d.node(root, "", "", Range{})
	if err != nil {
		return nil, err
	}
	d.Value = v
	return d, nil
}

// node converts one YAML node and registers its span under both spellings, returning the
// value and the node's own extent (so a parent's extent is its children's, computed once).
func (d *Doc) node(n *yaml.Node, phys, logi string, key Range) (any, Range, error) {
	switch n.Kind {
	case yaml.MappingNode:
		return d.mapping(n, phys, logi, key)

	case yaml.SequenceNode:
		out := make([]any, 0, len(n.Content))
		r := start(n)
		for i, c := range n.Content {
			// A sequence element is addressed by its `id` when it has one -- that is what
			// makes `tasks.fetch` an address at all, and it is the only reason the logical
			// spelling differs from the physical one below a task.
			cp := fmt.Sprintf("%s[%d]", phys, i)
			cl := fmt.Sprintf("%s.%d", logi, i)
			if id, ok := elementID(c); ok {
				cl = logi + "." + id
			}
			v, cr, err := d.node(c, cp, cl, Range{})
			if err != nil {
				return nil, r, err
			}
			out = append(out, v)
			r = extend(r, cr)
		}
		d.set(phys, logi, Span{Key: key, Value: r})
		return out, r, nil

	case yaml.AliasNode:
		v, _, err := d.node(n.Alias, phys, logi, key)
		// The alias is where the value was *written* for the reader's purposes; the anchor
		// is where it was defined. Underlining the anchor would send them to another file's
		// worth of scrolling for a value they can see.
		r := start(n)
		r.EndCol = n.Column + len(n.Value) + 1
		d.spans[phys] = Span{Key: key, Value: r}
		if logi != phys {
			d.spans[logi] = Span{Key: key, Value: r}
		}
		return v, r, err

	case yaml.ScalarNode:
		r := scalarRange(n)
		v, err := scalar(n)
		if err != nil {
			return nil, r, err
		}
		d.set(phys, logi, Span{Key: key, Value: r})
		return v, r, nil
	}

	var v any
	if err := n.Decode(&v); err != nil {
		return nil, Range{}, err
	}
	return v, start(n), nil
}

func (d *Doc) mapping(n *yaml.Node, phys, logi string, key Range) (any, Range, error) {
	out := make(map[string]any, len(n.Content)/2)
	r := start(n)
	var merges []*yaml.Node

	for i := 0; i+1 < len(n.Content); i += 2 {
		kn, vn := n.Content[i], n.Content[i+1]
		var name string
		if err := kn.Decode(&name); err != nil {
			return nil, r, fmt.Errorf("line %d: object key must be a scalar: %w", kn.Line, err)
		}
		if name == mergeKey {
			merges = append(merges, vn)
			continue
		}
		v, vr, err := d.node(vn, join(phys, name), join(logi, name), scalarRange(kn))
		if err != nil {
			return nil, r, err
		}
		out[name] = v
		r = extend(extend(r, scalarRange(kn)), vr)
	}

	// An explicit key beats a merged one -- YAML's own precedence, so there is no new rule to
	// learn. Applied after the loop because a merge may appear above the key it is overridden
	// by, and the spans below follow the same order for the same reason: a merged key that
	// lost must not leave its location behind on the key that won.
	for _, m := range merges {
		src, err := mergeTarget(m)
		if err != nil {
			return nil, r, err
		}
		for i := 0; i+1 < len(src.Content); i += 2 {
			var name string
			if err := src.Content[i].Decode(&name); err != nil {
				return nil, r, fmt.Errorf("line %d: object key must be a scalar: %w", src.Content[i].Line, err)
			}
			if _, taken := out[name]; taken || name == mergeKey {
				continue
			}
			v, _, err := d.node(src.Content[i+1], join(phys, name), join(logi, name), scalarRange(src.Content[i]))
			if err != nil {
				return nil, r, err
			}
			out[name] = v
		}
	}

	d.set(phys, logi, Span{Key: key, Value: r})
	return out, r, nil
}

// set registers a span under both spellings. A merged key reaches here from inside the anchor,
// where it is written -- which is the location a reader needs, not the `<<` line.
func (d *Doc) set(phys, logi string, s Span) {
	d.spans[phys] = s
	if logi != phys {
		d.spans[logi] = s
	}
}

func join(prefix, name string) string {
	if prefix == "" {
		return name
	}
	return prefix + "." + name
}

// elementID reports the `id` of a sequence element, which is what makes `tasks.<id>` an
// address. Only a plain scalar id qualifies: an id that is itself an expression has no stable
// spelling to address it by.
func elementID(n *yaml.Node) (string, bool) {
	if n.Kind != yaml.MappingNode {
		return "", false
	}
	for i := 0; i+1 < len(n.Content); i += 2 {
		if n.Content[i].Value == "id" && n.Content[i+1].Kind == yaml.ScalarNode {
			v := n.Content[i+1].Value
			if v != "" && !strings.ContainsAny(v, ".[]") {
				return v, true
			}
		}
	}
	return "", false
}

// scalar keeps numeric literals exact: decoding into `any` floats big integers (a 54-digit id
// once left here as 1.2374829758395876e+53). Scalars ride as json.Number; non-JSON literals
// (0x1F, 1_000, .inf) fall back to yaml.
func scalar(n *yaml.Node) (any, error) {
	if n.Tag == "!!int" || n.Tag == "!!float" {
		// A bare number is itself a valid JSON document, so this rejects exactly the YAML
		// spellings JSON cannot express.
		if json.Valid([]byte(n.Value)) {
			return json.Number(n.Value), nil
		}
	}
	var v any
	if err := n.Decode(&v); err != nil {
		return nil, fmt.Errorf("line %d: %w", n.Line, err)
	}
	return v, nil
}

// mergeKey is YAML's merge key. Without handling it here the alias landed under a literal
// "<<" field, which the server ignores as unknown and the canonical re-marshal strips -- so a
// definition using anchors silently lost every merged key. yaml.v3's own decoder merges
// correctly, so the two readers of one file disagreed.
const mergeKey = "<<"

// mergeTarget resolves a `<<` value to the mapping it contributes.
//
// The SEQUENCE form is refused rather than implemented: YAML 1.1 gives EARLIER entries
// precedence, the opposite of every other merge in use (`{...a, ...b}`, `{**a, **b}`, the CSS
// cascade), so `<<: [*base, *override]` would silently do the reverse of what it reads as. One
// anchor, or nesting, covers the same ground with no ambiguity.
func mergeTarget(n *yaml.Node) (*yaml.Node, error) {
	if n.Kind == yaml.SequenceNode {
		return nil, fmt.Errorf("line %d: `<<` takes a single mapping - a sequence is refused "+
			"because YAML gives its EARLIER entries precedence, so it reads backwards; "+
			"merge into one anchor instead", n.Line)
	}
	target := n
	if target.Kind == yaml.AliasNode {
		target = target.Alias
	}
	if target == nil || target.Kind != yaml.MappingNode {
		return nil, fmt.Errorf("line %d: `<<` needs a mapping, or an alias to one", n.Line)
	}
	return target, nil
}

func start(n *yaml.Node) Range {
	return Range{Line: n.Line, Col: n.Column, EndLine: n.Line, EndCol: n.Column}
}

func extend(r, by Range) Range {
	if by.Empty() {
		return r
	}
	if r.Empty() {
		return by
	}
	if by.EndLine > r.EndLine || (by.EndLine == r.EndLine && by.EndCol > r.EndCol) {
		r.EndLine, r.EndCol = by.EndLine, by.EndCol
	}
	return r
}

func scalarRange(n *yaml.Node) Range {
	r := start(n)
	if nl := strings.Count(n.Value, "\n"); nl > 0 {
		// A block scalar's Line is its `|` marker, so the end is the marker's line plus the
		// content's; the column is the last line's width, without its indent. Approximate.
		last := n.Value[strings.LastIndexByte(n.Value, '\n')+1:]
		r.EndLine, r.EndCol = n.Line+nl, n.Column+len(last)
		return r
	}
	w := len(n.Value)
	if n.Style == yaml.SingleQuotedStyle || n.Style == yaml.DoubleQuotedStyle {
		w += 2
	}
	r.EndCol = n.Column + w
	return r
}

package lsp

// What a `$<resolver>:` directive is worth a popup for. A STRUCTURAL directive shows what it
// yields -- the value it fills or spreads, as the YAML its author would have written, from the
// call the structural pass makes. A CODE directive is never run here (it shells out, and its
// answer is a string), so it says only that. specs/source-resolution.md §The editor's guess
// about a path.

import (
	"bytes"
	"path/filepath"
	"slices"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"

	"genroc/internal/defdoc"
	"genroc/internal/schema"
	"genroc/internal/sources"
)

func directiveHover(d *document, path string) string {
	if d.file == "" {
		return ""
	}
	v, ok := d.ValueAt(path)
	if !ok {
		return ""
	}
	leaf, ok := v.(string)
	if !ok {
		return ""
	}
	name, _, ok := defdoc.Directive(leaf)
	if !ok {
		return ""
	}
	cfg, err := sources.FindProjectConfig(filepath.Dir(d.file))
	if err != nil {
		return ""
	}
	// From the text as WRITTEN: the site is a node here, and resolving would remove it.
	written := sources.Doc{Value: deepCopy(d.Value), File: d.file}
	value, structural, err := sources.StructuralValueAt(written, cfg, path)
	if err != nil {
		// The diagnostic on the same line already says why.
		return ""
	}
	if structural {
		return yamlBlock(renderValue(value, overriddenKeys(d.Doc, path)))
	}
	return "`" + name + "` runs at apply, in the code phase, and fills this slot with a string."
}

// overriddenKeys is what the mapping around a spread already writes, which the spread does not
// take. Only a `<<` has any; a slot site replaces its leaf whole.
func overriddenKeys(doc *defdoc.Doc, path string) map[string]bool {
	out := map[string]bool{}
	if path != defdoc.MergeKey && !strings.HasSuffix(path, "."+defdoc.MergeKey) {
		return out
	}
	if parent, ok := doc.ValueAt(defdoc.ParentPath(path)); ok {
		if m, ok := parent.(map[string]any); ok {
			for k := range m {
				out[k] = true
			}
		}
	}
	return out
}

func yamlBlock(body string) string {
	if body == "" {
		return ""
	}
	return "```yaml\n" + body + "```"
}

// spreadOrder is how a spread reads: the name, then what goes in, then what comes out.
var spreadOrder = []string{"name", "input_schema", "result_schema", "raises"}

// renderValue prints what a structural directive yields. A key the mapping around a spread
// already writes stays in the picture with a note: the popup answers what the file yields, and
// the precedence is the one fact a reader would otherwise get wrong.
func renderValue(value any, overridden map[string]bool) string {
	root := toNode(value, spreadOrder, false)
	root.Style = 0
	for i := 0; root.Kind == yaml.MappingNode && i+1 < len(root.Content); i += 2 {
		key, val := root.Content[i], root.Content[i+1]
		if !overridden[key.Value] {
			continue
		}
		// yaml.v3 prints a key's line comment on the NEXT pair when the value is inline, so
		// the note rides the value there and the key only where the value opens a block.
		if inline(val) {
			val.LineComment = "the key written here wins"
		} else {
			key.LineComment = "the key written here wins"
		}
	}
	return encodeYAML(root)
}

func encodeYAML(root *yaml.Node) string {
	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(root); err != nil {
		return ""
	}
	if err := enc.Close(); err != nil {
		return ""
	}
	return buf.String()
}

// toNode builds the tree with keys in reading order rather than yaml.v3's sorted one, and a
// mapping or list of scalars on one line, the way these files are written. `named` marks a
// mapping whose keys are the author's names, which sort alphabetically; every other key is a
// keyword and follows schema.KeywordRank.
func toNode(v any, order []string, named bool) *yaml.Node {
	switch t := v.(type) {
	case map[string]any:
		keys := make([]string, 0, len(t))
		for k := range t {
			keys = append(keys, k)
		}
		sort.Slice(keys, func(i, j int) bool {
			ri, rj := rank(keys[i], order, named), rank(keys[j], order, named)
			if ri != rj {
				return ri < rj
			}
			return keys[i] < keys[j]
		})
		n := &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map", Style: yaml.FlowStyle}
		for _, k := range keys {
			child := toNode(t[k], nil, namesUnder(k))
			if child.Kind == yaml.MappingNode || !inline(child) {
				n.Style = 0
			}
			n.Content = append(n.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: k}, child)
		}
		return n
	case []any:
		n := &yaml.Node{Kind: yaml.SequenceNode, Tag: "!!seq", Style: yaml.FlowStyle}
		for _, e := range t {
			child := toNode(e, nil, false)
			if child.Kind != yaml.ScalarNode {
				n.Style = 0
			}
			n.Content = append(n.Content, child)
		}
		return n
	default:
		n := &yaml.Node{}
		if err := n.Encode(v); err != nil {
			return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: "?"}
		}
		return n
	}
}

// inline reports whether a node prints on one line: a short scalar, or a flow collection. A long
// scalar is left to a block, where a description reads as a line of its own.
func inline(n *yaml.Node) bool {
	switch n.Kind {
	case yaml.ScalarNode:
		return len(n.Value) <= 40
	case yaml.MappingNode, yaml.SequenceNode:
		return n.Style == yaml.FlowStyle
	}
	return false
}

// rank orders keys: the caller's own list first, then the keyword order, then the rest.
func rank(k string, order []string, named bool) int {
	if named {
		return 0
	}
	if i := slices.Index(order, k); i >= 0 {
		return i
	}
	if i := schema.KeywordRank(k); i >= 0 {
		return len(order) + i
	}
	return len(order) + len(schema.KeywordOrder())
}

// namesUnder reports whether the keys under k are the author's rather than keywords.
func namesUnder(k string) bool {
	return k == "properties" || k == "$defs" || k == "raises"
}

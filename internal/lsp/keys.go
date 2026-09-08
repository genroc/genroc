package lsp

// Which keys are legal where, read out of the generated schema.
//
// The schema rather than reflection over the Go types, because seven of them decode by hand
// and carry a hand-written JSONSchemaBytes: reflection sees no fields on an Action, a
// SwitchMap or a Retry, which are the nodes most worth completing. The discriminator JSON
// Schema cannot express is no obstacle to a consumer that is us — each variant carries
// `type: {const: fetch}`, and we read it. specs/language-server.md §5.

import (
	"encoding/json"
	"strings"

	"genroc/internal/defdoc"
	"genroc/internal/defschema"
)

// legalKeys returns the keys allowed in the mapping at path, minus the ones already written.
func legalKeys(doc *defdoc.Doc, path string) []completionItem {
	root, ok := processSchema()
	if !ok {
		return nil
	}
	node, ok := walk(root, doc, path)
	if !ok {
		return nil
	}
	props, _ := node["properties"].(map[string]any)
	if len(props) == 0 {
		return nil
	}
	present := map[string]bool{}
	if v, ok := doc.ValueAt(path); ok {
		if m, ok := v.(map[string]any); ok {
			for k := range m {
				present[k] = true
			}
		}
	}
	required := map[string]bool{}
	if req, ok := node["required"].([]any); ok {
		for _, r := range req {
			if name, ok := r.(string); ok {
				required[name] = true
			}
		}
	}

	out := []completionItem{}
	for name, sub := range props {
		if present[name] {
			continue
		}
		m, _ := sub.(map[string]any)
		detail := ""
		if required[name] {
			detail = "required"
		}
		out = append(out, completionItem{
			Label:         name,
			Kind:          kindProperty,
			Detail:        detail,
			Documentation: describeNode(m),
		})
	}
	return out
}

// walk follows a DOCUMENT path down the schema, resolving refs and choosing a oneOf branch by
// the `type` the document actually carries — which is what `discriminator` meant.
//
// It tracks the ABSOLUTE path as it descends, because the discriminator is read out of the
// document at the node being entered: a relative path would look up `type` at the root.
func walk(root map[string]any, doc *defdoc.Doc, path string) (map[string]any, bool) {
	node, ok := resolve(root, root)
	if !ok {
		return nil, false
	}
	here := ""
	rest := path
	for {
		node = branchFor(root, node, doc, here)
		if rest == "" {
			return node, true
		}
		seg, tail := cutSegment(rest)
		if items, isArray := node["items"].(map[string]any); isArray {
			// A sequence has one `items` for every element, so the segment is spent getting
			// inside it rather than selecting among alternatives.
			node, ok = resolve(root, items)
			if !ok {
				return nil, false
			}
			here, rest = joinPath(here, seg), tail
			continue
		}
		props, _ := node["properties"].(map[string]any)
		next, ok := props[seg].(map[string]any)
		if !ok {
			return nil, false
		}
		node, ok = resolve(root, next)
		if !ok {
			return nil, false
		}
		here, rest = joinPath(here, seg), tail
	}
}

// branchFor picks the oneOf arm whose `type` const matches the document's, so an action
// completes as the action it is rather than as the union of six. An unset or unrecognised
// `type` leaves the union, which is the honest answer while it is still being typed.
func branchFor(root, node map[string]any, doc *defdoc.Doc, path string) map[string]any {
	arms, ok := node["oneOf"].([]any)
	if !ok {
		return node
	}
	kind, _ := valueField(doc, path, "type")
	for _, arm := range arms {
		m, ok := arm.(map[string]any)
		if !ok {
			continue
		}
		if m, ok = resolve(root, m); !ok {
			continue
		}
		props, _ := m["properties"].(map[string]any)
		typ, _ := props["type"].(map[string]any)
		if c, ok := typ["const"].(string); ok && c == kind {
			return m
		}
	}
	return node
}

func valueField(doc *defdoc.Doc, path, field string) (string, bool) {
	v, ok := doc.ValueAt(joinPath(path, field))
	if !ok {
		return "", false
	}
	s, ok := v.(string)
	return s, ok
}

func resolve(root, node map[string]any) (map[string]any, bool) {
	for i := 0; i < 8; i++ {
		ref, ok := node["$ref"].(string)
		if !ok {
			return node, true
		}
		name, ok := strings.CutPrefix(ref, "#/$defs/")
		if !ok {
			return node, true
		}
		defs, _ := root["$defs"].(map[string]any)
		next, ok := defs[name].(map[string]any)
		if !ok {
			return nil, false
		}
		node = next
	}
	return nil, false // a $ref cycle; the schema is generated, so this is a bug not an input
}

func describeNode(m map[string]any) string {
	d, _ := m["description"].(string)
	return d
}

func cutSegment(path string) (string, string) {
	if i := strings.IndexByte(path, '.'); i >= 0 {
		return path[:i], path[i+1:]
	}
	return path, ""
}

func joinPath(prefix, name string) string {
	if prefix == "" {
		return name
	}
	return prefix + "." + name
}

// processSchema decodes the generated schema. It is a projection of the Go types, so it
// cannot change while the process runs.
func processSchema() (map[string]any, bool) {
	var root map[string]any
	if err := json.Unmarshal(defschema.Process(), &root); err != nil {
		return nil, false
	}
	return root, true
}

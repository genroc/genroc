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
	"fmt"
	"strconv"
	"strings"

	"genroc/internal/defdoc"
	"genroc/internal/defschema"
	"genroc/internal/schema"
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
	node = choose(root, node, doc, path, "")
	// A cursor on a sequence is writing one of its ELEMENTS, and a sequence has no keys of its
	// own — which is what a cursor on a list dash resolves to.
	if items, isArray := node["items"].(map[string]any); isArray {
		if node, ok = resolve(root, items); !ok {
			return nil
		}
		node = choose(root, node, doc, path, "")
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
		out = append(out, completionItem{
			Label:  name,
			Kind:   kindProperty,
			Detail: keyDetail(name, m, required[name]),
			// Editors sort on this string, and with none they fall back to a fuzzy score
			// that ties across a whole vocabulary — leaving `$anchor` at the top of a list
			// of JSON Schema keywords.
			SortText:      sortKey(name, required[name]),
			Documentation: describeNode(m),
		})
	}
	return out
}

// walk follows a DOCUMENT path down the schema, resolving refs and choosing among union arms.
//
// It tracks the ABSOLUTE path as it descends, because a discriminator is read out of the
// document at the node being entered: a relative path would look up `type` at the root.
func walk(root map[string]any, doc *defdoc.Doc, path string) (map[string]any, bool) {
	node, ok := resolve(root, root)
	if !ok {
		return nil, false
	}
	here := ""
	rest := path
	for {
		if rest == "" {
			// The terminal node is returned AS DECLARED, union and all: a caller reading the
			// keys wants the arm the document selects, and one reading the variants wants the
			// union it selects from. `choose` is theirs to apply.
			return node, true
		}
		seg, tail := cutSegment(rest)
		node = choose(root, node, doc, here, seg)
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
			// An open map types every key the same way — a user schema's `properties`, a
			// child_map's `children` — so an undeclared segment descends there.
			if next, ok = node["additionalProperties"].(map[string]any); !ok {
				return nil, false
			}
		}
		node, ok = resolve(root, next)
		if !ok {
			return nil, false
		}
		here, rest = joinPath(here, seg), tail
	}
}

// choose picks among a union's arms. The document's own `type` decides where there is one —
// that is what `discriminator` meant, and it is why a `fetch` completes as a fetch. Where there
// is none, the arm that can take the NEXT step decides: a `switch` is a scalar shorthand or a
// list of cases, and an index says which of those is being written.
func choose(root, node map[string]any, doc *defdoc.Doc, path, next string) map[string]any {
	arms := unionArms(node)
	if arms == nil {
		return node
	}
	kind, _ := valueField(doc, path, "type")
	var indexed, keyed, object map[string]any
	for _, arm := range arms {
		m, ok := arm.(map[string]any)
		if !ok {
			continue
		}
		if m, ok = resolve(root, m); !ok {
			continue
		}
		props, _ := m["properties"].(map[string]any)
		if typ, _ := props["type"].(map[string]any); typ != nil {
			if c, ok := typ["const"].(string); ok && c == kind {
				return m
			}
		}
		if _, isArray := m["items"]; isArray && indexed == nil {
			indexed = m
		}
		if len(props) > 0 {
			if object == nil {
				object = m
			}
			if _, has := props[next]; has && keyed == nil {
				keyed = m
			}
		}
	}
	switch {
	case next != "" && isIndex(next) && indexed != nil:
		return indexed
	case keyed != nil:
		return keyed
	case next == "" && object != nil:
		return object
	}
	return node
}

func unionArms(node map[string]any) []any {
	if arms, ok := node["oneOf"].([]any); ok {
		return arms
	}
	arms, _ := node["anyOf"].([]any)
	return arms
}

func isIndex(seg string) bool {
	_, err := strconv.Atoi(seg)
	return err == nil
}

// describeKey is what a key MEANS, read off the schema that declares it — the prose the struct
// tag already carries, which is what a reader hovering `only_once:` is asking for.
func describeKey(doc *defdoc.Doc, path string) string {
	root, ok := processSchema()
	if !ok || path == "" {
		return ""
	}
	parent := defdoc.ParentPath(path)
	node, ok := walk(root, doc, parent)
	if !ok {
		return ""
	}
	node = choose(root, node, doc, parent, lastSegment(path))
	props, _ := node["properties"].(map[string]any)
	name := lastSegment(path)
	field, _ := props[name].(map[string]any)
	if d := describeNode(field); d != "" {
		return d
	}
	// The discriminator carries no prose of its own — `{"const": "delay"}` says nothing a
	// reader wants. What they are pointing at is the variant it selects, so answer with that.
	if name == "type" {
		return describeNode(node)
	}
	return ""
}

func lastSegment(path string) string {
	if i := strings.LastIndexByte(path, '.'); i >= 0 {
		return path[i+1:]
	}
	return path
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

// processSchema decodes the generated schema, and restores the one thing it cannot carry: a
// user schema nests user schemas. The published document leaves those positions permissive
// because openapi-typescript turns a self-$ref into a cycle tsc rejects (internal/schema), and
// nothing here is generating TypeScript — so the recursion goes back in and hover and
// completion work at every depth of an `input_schema`.
func processSchema() (map[string]any, bool) {
	var root map[string]any
	if err := json.Unmarshal(defschema.Process(), &root); err != nil {
		return nil, false
	}
	defs, _ := root["$defs"].(map[string]any)
	user, _ := defs[userSchemaDef].(map[string]any)
	props, _ := user["properties"].(map[string]any)
	if props == nil {
		return root, true
	}
	// Marked so a consumer can tell "the author's own schema" from the definition language
	// around it — the two have different closed sets for `type`. LSP-local: processSchema
	// parses a fresh copy per request and nothing here is published.
	user[userSchemaMarker] = true
	self := map[string]any{"$ref": "#/$defs/" + userSchemaDef}
	list := map[string]any{"type": "array", "items": self}
	for name, nested := range map[string]map[string]any{
		"properties": {"type": "object", "additionalProperties": self},
		"$defs":      {"type": "object", "additionalProperties": self},
		"items":      self, "additionalProperties": self,
		"oneOf": list, "anyOf": list,
	} {
		field, _ := props[name].(map[string]any)
		for k, v := range nested {
			field[k] = v
		}
	}
	// The same repair where a user schema is written into an ACTION. Those slots are
	// hand-written in model.Action's template as permissive objects, so nothing marks them as
	// schemas for a reader standing in one.
	pointAtUserSchema(defs, self)
	return root, true
}

// pointAtUserSchema rewrites `responses` values and `result_schema` in every action variant to
// the user-schema def, which is what they hold.
func pointAtUserSchema(defs map[string]any, self map[string]any) {
	action, _ := defs["ModelAction"].(map[string]any)
	arms, _ := action["oneOf"].([]any)
	nullable := map[string]any{"anyOf": []any{self, map[string]any{"type": "null"}}}
	for _, arm := range arms {
		m, _ := arm.(map[string]any)
		props, _ := m["properties"].(map[string]any)
		if r, ok := props["responses"].(map[string]any); ok {
			r["additionalProperties"] = nullable
		}
		if r, ok := props["result_schema"].(map[string]any); ok {
			for k, v := range self {
				r[k] = v
			}
		}
		// child_map nests one child spec per key, each with a result_schema of its own.
		children, _ := props["children"].(map[string]any)
		if inner, ok := children["additionalProperties"].(map[string]any); ok {
			nested, _ := inner["properties"].(map[string]any)
			if r, ok := nested["result_schema"].(map[string]any); ok {
				for k, v := range self {
					r[k] = v
				}
			}
		}
	}
}

// userSchemaDef is the generated name for schema.Schema. specs/language-server.md §5.
const userSchemaDef = "SchemaSchema"

// userSchemaMarker tags that def after the repair, so a walk can recognise it.
const userSchemaMarker = "x-genroc-user-schema"

// keyDetail is the line shown BESIDE a key in the list — its type, and whether it is required.
// The description needs a panel opened; this is what a reader sees while scrolling.
func keyDetail(name string, node map[string]any, required bool) string {
	parts := []string{}
	if required {
		parts = append(parts, "required")
	}
	kind := typeName(node)
	if kind == "" {
		// A JSON Schema keyword: the published document cannot carry its type without
		// breaking the generated client, so the kind comes from the package that owns it.
		kind = schema.KeywordKind(name)
	}
	if kind != "" {
		parts = append(parts, kind)
	}
	return strings.Join(parts, " ")
}

// typeName reads a node's type for display, following the one shape a union takes here: a
// `oneOf` of variants, which reads as the alternatives it offers.
func typeName(node map[string]any) string {
	switch t := node["type"].(type) {
	case string:
		return t
	case []any:
		names := make([]string, 0, len(t))
		for _, v := range t {
			if name, ok := v.(string); ok {
				names = append(names, name)
			}
		}
		return strings.Join(names, "|")
	}
	if _, ok := node["$ref"]; ok {
		return "object"
	}
	if arms := unionArms(node); len(arms) > 0 {
		return "one of " + strconv.Itoa(len(arms))
	}
	return ""
}

// sortKey orders a completion list the way the thing being written is READ: a required key
// first, then a JSON Schema keyword by schema.KeywordOrder — the same order `genctl schema`
// prints one in — and anything else alphabetically after.
func sortKey(name string, required bool) string {
	if required {
		return "0" + name
	}
	// A `default` takes any type, so it has no kind — its place in the order is what says it
	// is a keyword at all.
	if rank := schema.KeywordRank(name); rank >= 0 {
		return fmt.Sprintf("1%03d", rank)
	}
	return "2" + name
}

// isUserSchema reports whether a slot holds the author's OWN JSON Schema rather than the
// definition language around it. A nullable slot declares it as one arm of a union
// (`responses`), so the arms count as much as the node itself.
func isUserSchema(root, node map[string]any) bool {
	if marked, _ := node[userSchemaMarker].(bool); marked {
		return true
	}
	for _, arm := range unionArms(node) {
		m, ok := arm.(map[string]any)
		if !ok {
			continue
		}
		if m, ok = resolve(root, m); !ok {
			continue
		}
		if marked, _ := m[userSchemaMarker].(bool); marked {
			return true
		}
	}
	return false
}

// soleSchemaSlotNotAnObject finds the one slot holding a user schema whose value is not an
// object. It answers "which schema was that about" for the one failure the schema decoder
// cannot place: a slot that is a scalar has no path INSIDE the schema to report, and
// encoding/json adds its own context only to its own type errors.
func soleSchemaSlotNotAnObject(doc *defdoc.Doc) (string, bool) {
	root, ok := processSchema()
	if !ok {
		return "", false
	}
	var found string
	seen := map[defdoc.Span]bool{}
	for _, p := range doc.Paths() {
		// An object is a well-formed schema, and a null one decodes (checkDoc reports the ones
		// that are meaningless) — neither can be what raised this.
		v, _ := doc.ValueAt(p)
		if _, isObject := v.(map[string]any); isObject || v == nil {
			continue
		}
		node, ok := walk(root, doc, p)
		if !ok || !isUserSchema(root, node) {
			continue
		}
		span, ok := doc.Span(p)
		if !ok || seen[span] {
			continue
		}
		seen[span] = true
		found = p
	}
	return found, len(seen) == 1
}

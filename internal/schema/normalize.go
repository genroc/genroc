package schema

import (
	"fmt"
	"sort"
	"strings"
)

type normContext struct {
	definitions map[string]*defEntry
	anchors     map[string]*defEntry
	references  []*refSite
	sites       map[*node]*refSite // ref-carrying node → its collected site (for base-aware re-resolution)
}

// defEntry holds a collected definition and its eventual flattened name.
type defEntry struct {
	OriginalName string
	NewName      string
	Node         *node
	Used         bool
}

// refSite holds a collected $ref and a pointer to the node containing it
// so the value can be rewritten in place later.
type refSite struct {
	RefValue     string
	Node         *node
	ResourceBase string
}

// ErrUnsupportedRef is returned when a $ref value is structurally outside the
// supported subset (e.g. an external URL or a relative JSON Pointer).
type ErrUnsupportedRef struct{ Ref string }

func (e ErrUnsupportedRef) Error() string {
	return fmt.Sprintf("unsupported $ref %q: must be \"#/$defs/<name>\" or \"#<anchor>\"", e.Ref)
}

// ErrUnresolvedAnchor is returned when a "#<anchor>" ref names an anchor that
// is not registered in the root resource.
type ErrUnresolvedAnchor struct{ Ref string }

func (e ErrUnresolvedAnchor) Error() string {
	anchor := strings.TrimPrefix(e.Ref, "#")
	return fmt.Sprintf("unresolved $ref %q: anchor %q is not defined in the root resource", e.Ref, anchor)
}

// ErrUnresolvedRef is returned when a "#/$defs/<name>" ref cannot be matched to
// any known definition.
type ErrUnresolvedRef struct{ Ref string }

func (e ErrUnresolvedRef) Error() string {
	return fmt.Sprintf("unresolved $ref %q: no matching definition", e.Ref)
}

// normalize flattens all $defs to the root, removes unused definitions,
// and rewrites $ref values to point to the new flat locations.
func normalize(schema *node) (*node, error) {
	if schema == nil {
		return nil, nil
	}
	ctx := &normContext{
		definitions: make(map[string]*defEntry),
		anchors:     make(map[string]*defEntry),
		references:  make([]*refSite, 0),
		sites:       make(map[*node]*refSite),
	}

	// Phase 1: collect all definitions, anchors, and references from the whole tree.
	walkTree(schema, nil, func(nd *node, path []string, resourceBase string) {
		if len(path) >= 2 && path[len(path)-2] == "$defs" {
			key := strings.Join(path, "/")
			if _, exists := ctx.definitions[key]; !exists {
				def := &defEntry{OriginalName: path[len(path)-1], Node: nd}
				ctx.definitions[key] = def
				if nd.Anchor != "" && resourceBase == "" {
					ctx.anchors[nd.Anchor] = def
				}
			}
		} else if nd.Anchor != "" && resourceBase == "" {
			key := strings.Join(append(cloneStrings(path), "$anchor", nd.Anchor), "/")
			if _, exists := ctx.definitions[key]; !exists {
				def := &defEntry{OriginalName: nd.Anchor, Node: nd}
				ctx.definitions[key] = def
				ctx.anchors[nd.Anchor] = def
			}
		}
		if nd.Ref != "" {
			site := &refSite{
				RefValue:     nd.Ref,
				Node:         nd,
				ResourceBase: resourceBase,
			}
			ctx.references = append(ctx.references, site)
			ctx.sites[nd] = site
		}
	})

	// Phase 2: resolve each ref to its target definition and mark it as used.
	for _, ref := range ctx.references {
		def, err := ctx.resolveRef(ref.RefValue, ref.ResourceBase)
		if err != nil {
			return nil, err
		}
		if def != nil {
			def.Used = true
		}
	}

	// Phase 3: strip $defs, $id, and $anchor from every node in the tree.
	walkTree(schema, nil, func(nd *node, _ []string, _ string) {
		nd.ID = ""
		nd.Defs = nil
		nd.Anchor = ""
	})
	for _, def := range ctx.definitions {
		walkTree(def.Node, nil, func(nd *node, _ []string, _ string) {
			nd.ID = ""
			nd.Defs = nil
			nd.Anchor = ""
		})
	}

	// Build root $defs from used definitions, resolving name collisions.
	// Shallower definitions claim their name first: a top-level definition keeps
	// its exact name and a nested one that collides gets the unique suffix. This
	// is what lets FlattenNamed guarantee that its named entries — the generated
	// schema names during process generation — always win a collision.
	//
	// Two collisions are not real conflicts and produce no suffix:
	//   - a pure-$ref definition aliasing a same-named definition (a schema that
	//     IS a reference to a shared def, e.g. input_schema {$ref:#/$defs/input}
	//     over a def named "input") — the target binds directly under the name
	//     instead of leaving an input → input_1 indirection;
	//   - a definition content-equal to the one already holding the name (the
	//     same definition arriving twice, e.g. baked into two schemas) — shared.
	defKeys := make([]string, 0, len(ctx.definitions))
	for k, def := range ctx.definitions {
		if def.Used {
			defKeys = append(defKeys, k)
		}
	}
	sort.Slice(defKeys, func(i, j int) bool {
		di, dj := strings.Count(defKeys[i], "/"), strings.Count(defKeys[j], "/")
		if di != dj {
			return di < dj
		}
		return defKeys[i] < defKeys[j]
	})
	rootDefs := make(map[string]*node)
	for _, k := range defKeys {
		def := ctx.definitions[k]
		if def.NewName != "" {
			continue // already named as some alias's target
		}
		if isPureRef(def.Node) {
			if target := ctx.aliasTarget(def); target != nil && target.OriginalName == def.OriginalName {
				if target.NewName == "" {
					target.NewName = getUniqueName(def.OriginalName, rootDefs)
					rootDefs[target.NewName] = target.Node
				}
				def.NewName = target.NewName // refs to the alias land on the target
				continue
			}
		}
		if existing, taken := rootDefs[def.OriginalName]; taken && nodesEqual(existing, def.Node) {
			def.NewName = def.OriginalName
			continue
		}
		def.NewName = getUniqueName(def.OriginalName, rootDefs)
		rootDefs[def.NewName] = def.Node
	}

	// Rewrite $ref values to the new flat paths.
	for _, ref := range ctx.references {
		def, _ := ctx.resolveRef(ref.RefValue, ref.ResourceBase)
		if def != nil && def.Used {
			ref.Node.Ref = "#/$defs/" + def.NewName
		}
	}

	if len(rootDefs) > 0 {
		schema.Defs = rootDefs
	}
	return schema, nil
}

// walkTree calls fn(node, path, resourceBase) for the given node and all nested
// schema nodes, covering every keyword in node that can contain sub-schemas.
func walkTree(nd *node, path []string, fn func(*node, []string, string)) {
	var walk func(*node, []string, string)
	walk = func(n *node, p []string, resourceBase string) {
		if n == nil {
			return
		}
		if n.ID != "" && len(p) > 0 {
			resourceBase = strings.Join(p, "/")
		}
		fn(n, p, resourceBase)
		for name, prop := range n.Properties {
			if prop != nil {
				walk(prop, append(cloneStrings(p), "properties", name), resourceBase)
			}
		}
		for name, def := range n.Defs {
			if def != nil {
				walk(def, append(cloneStrings(p), "$defs", name), resourceBase)
			}
		}
		if n.Items != nil {
			walk(n.Items, append(cloneStrings(p), "items"), resourceBase)
		}
		if n.AdditionalProperties != nil {
			walk(n.AdditionalProperties, append(cloneStrings(p), "additionalProperties"), resourceBase)
		}
		for i, s := range n.OneOf {
			if s != nil {
				walk(s, append(cloneStrings(p), "oneOf", fmt.Sprintf("%d", i)), resourceBase)
			}
		}
		for i, s := range n.AnyOf {
			if s != nil {
				walk(s, append(cloneStrings(p), "anyOf", fmt.Sprintf("%d", i)), resourceBase)
			}
		}
		for i, s := range n.AllOf {
			if s != nil {
				walk(s, append(cloneStrings(p), "allOf", fmt.Sprintf("%d", i)), resourceBase)
			}
		}
	}
	walk(nd, path, "")
}

func (ctx *normContext) resolveRef(ref string, resourceBase string) (*defEntry, error) {
	if strings.HasPrefix(ref, "#/$defs/") {
		path := strings.TrimPrefix(ref, "#/")
		def := ctx.resolveDef(path, resourceBase)
		if def == nil {
			return nil, ErrUnresolvedRef{Ref: ref}
		}
		return def, nil
	}
	if strings.HasPrefix(ref, "#") && !strings.HasPrefix(ref, "#/") {
		anchor := strings.TrimPrefix(ref, "#")
		def := ctx.anchors[anchor]
		if def == nil {
			return nil, ErrUnresolvedAnchor{Ref: ref}
		}
		return def, nil
	}
	return nil, ErrUnsupportedRef{Ref: ref}
}

// resolveDef matches a "$defs/<name>" path to a collected definition, innermost
// resource first (nearest-wins, matching JSON Schema resource scoping) before falling
// back to the root. Root-first would make a definition sharing its resource's name
// resolve to the resource itself — a self-loop that orphans the real definition.
func (ctx *normContext) resolveDef(path string, resourceBase string) *defEntry {
	if resourceBase != "" {
		if def, ok := ctx.definitions[resourceBase+"/"+path]; ok {
			return def
		}
	}
	if def, ok := ctx.definitions[path]; ok {
		return def
	}
	return nil
}

// isPureRef reports whether nd is nothing but a $ref pointer — no other keyword
// refines it — so it carries no meaning of its own beyond naming its target.
func isPureRef(nd *node) bool {
	return nd != nil && nd.Ref != "" &&
		len(nd.Type) == 0 && nd.Properties == nil && nd.Required == nil &&
		nd.AdditionalProperties == nil &&
		nd.Items == nil && nd.OneOf == nil && nd.AnyOf == nil && nd.AllOf == nil &&
		nd.Enum == nil && nd.Minimum == nil && nd.Maximum == nil &&
		nd.MinLength == nil && nd.MaxLength == nil &&
		nd.MinItems == nil && nd.MaxItems == nil &&
		nd.Defs == nil && nd.Anchor == "" && nd.ID == "" &&
		nd.Default == nil && !nd.Secret && nd.Description == ""
}

// aliasTarget follows a chain of pure-$ref definitions from def to the first
// definition with content of its own. Returns nil when def is not an alias, the
// chain cannot be resolved, or it cycles without reaching content.
func (ctx *normContext) aliasTarget(def *defEntry) *defEntry {
	seen := map[*defEntry]bool{}
	cur := def
	for isPureRef(cur.Node) && !seen[cur] {
		seen[cur] = true
		site := ctx.sites[cur.Node]
		if site == nil {
			return nil
		}
		next, err := ctx.resolveRef(site.RefValue, site.ResourceBase)
		if err != nil || next == nil {
			return nil
		}
		cur = next
	}
	if cur == def || isPureRef(cur.Node) {
		return nil
	}
	return cur
}

func getUniqueName(name string, existing map[string]*node) string {
	newName := name
	for i := 1; existing[newName] != nil; i++ {
		newName = fmt.Sprintf("%s_%d", name, i)
	}
	return newName
}

func cloneStrings(s []string) []string {
	out := make([]string, len(s))
	copy(out, s)
	return out
}

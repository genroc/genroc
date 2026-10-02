package schema

import (
	"encoding/json"
	"sort"
	"strings"
)

// ─── Defs: the shared root-definitions handle ───────────────────────────────────

// Defs is a handle over a SHARED, MUTABLE map: every Schema given it via WithDefs observes a
// later Set through its $refs. The recursive output-type fixpoint relies on that aliasing.
type Defs struct {
	m map[string]*node
}

func NewDefs() Defs {
	return Defs{m: make(map[string]*node)}
}

// Set inserts or replaces the named definition in place: every Schema sharing this
// handle observes the update.
func (d Defs) Set(name string, s Schema) {
	d.m[name] = s.n
}

// Get returns the named definition bare, without the handle attached — its own $refs
// point back into this set, so attach the handle (WithDefs) before resolving them.
func (d Defs) Get(name string) (Schema, bool) {
	n, ok := d.m[name]
	if !ok {
		return Schema{}, false
	}
	return Schema{n: n}, true
}

func (d Defs) Has(name string) bool {
	_, ok := d.m[name]
	return ok
}

func (d Defs) Len() int {
	return len(d.m)
}

// Names returns the definition names in sorted order.
func (d Defs) Names() []string {
	out := make([]string, 0, len(d.m))
	for k := range d.m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// IsZero reports whether the handle holds no definitions, treating an empty-but-live
// map the same as no map: encoding/json's `omitzero` calls this, so a definition-less
// process omits "$defs" rather than emitting `"$defs": {}`.
func (d Defs) IsZero() bool {
	return len(d.m) == 0
}

func (d Defs) MarshalJSON() ([]byte, error) {
	return json.Marshal(d.m)
}

// UnmarshalJSON parses a name→schema object (strict keyword allowlist).
func (d *Defs) UnmarshalJSON(data []byte) error {
	var m map[string]*node
	if err := json.Unmarshal(data, &m); err != nil {
		return err
	}
	d.m = m
	return nil
}

// WithDefs shares the handle's map, not a copy. An empty-but-live handle attaches too, so the
// schema sees definitions seeded later; only a zero Defs is a no-op.
func (s Schema) WithDefs(d Defs) Schema {
	if d.m == nil {
		return s
	}
	return s.keepingContext(wrap(s.n, d.m))
}

// WithMergedDefs returns a copy of s whose root $defs are the union of its own and the
// handle's — s's own definitions win a name clash. Unlike WithDefs the maps merge into
// a fresh map, so neither receiver nor handle observes later changes. Empty handle: no-op.
func (s Schema) WithMergedDefs(d Defs) Schema {
	if len(d.m) == 0 {
		return s
	}
	own := s.rootDefs()
	merged := make(map[string]*node, len(own)+len(d.m))
	for k, v := range d.m {
		merged[k] = v
	}
	for k, v := range own {
		merged[k] = v
	}
	return s.keepingContext(wrap(s.n, merged))
}

// MergeInto hoists s's root $defs into d (in place) and returns s defs-free, refs rewritten. A
// content-equal def is reused, a different one renamed; existing entries keep their names.
func (s Schema) MergeInto(d Defs) (Schema, error) {
	if s.n == nil || len(s.n.Defs) == 0 {
		return s, nil
	}
	cloned, err := deepClone(s.n)
	if err != nil {
		return Schema{}, err
	}
	moved := cloned.Defs
	cloned.Defs = nil

	// Assign a target name per definition, deterministically (sorted names).
	names := make([]string, 0, len(moved))
	for name := range moved {
		names = append(names, name)
	}
	sort.Strings(names)
	rename := make(map[string]string, len(moved))
	insert := make(map[string]*node, len(moved))
	for _, name := range names {
		def := moved[name]
		if target, ok := findEqualDef(d.m, name, def); ok {
			rename[name] = target // reuse the existing content-equal definition
			continue
		}
		newName := name
		if _, taken := d.m[name]; taken {
			newName = getUniqueName(name, d.m)
		}
		rename[name] = newName
		insert[newName] = def
		d.m[newName] = def // claim immediately so later names stay unique
	}

	applyRename(cloned, rename)
	for _, def := range insert {
		applyRename(def, rename)
	}
	return Schema{n: cloned}, nil
}

// findEqualDef finds an existing definition content-equal to def modulo the insertion
// rename: a self-referencing definition must match an earlier copy whose self-refs spell
// the renamed name, or recursion re-merges as fresh input_1, input_2 … every time.
func findEqualDef(existing map[string]*node, name string, def *node) (string, bool) {
	names := make([]string, 0, len(existing))
	for n := range existing {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, en := range names {
		if nodesEqual(existing[en], def) {
			return en, true
		}
	}
	if !referencesDef(def, name) {
		return "", false
	}
	for _, en := range names {
		if en == name {
			continue // no rename would occur; the plain comparison above covered it
		}
		clone, err := deepClone(def)
		if err != nil {
			return "", false
		}
		applyRename(clone, map[string]string{name: en})
		if nodesEqual(existing[en], clone) {
			return en, true
		}
	}
	return "", false
}

func referencesDef(root *node, name string) bool {
	found := false
	walkTree(root, nil, func(nd *node, _ []string, _ string) {
		if nd.Ref == "#/$defs/"+name {
			found = true
		}
	})
	return found
}

// applyRename rewrites every "#/$defs/<old>" ref in the tree per the rename map.
func applyRename(root *node, rename map[string]string) {
	walkTree(root, nil, func(nd *node, _ []string, _ string) {
		const prefix = "#/$defs/"
		if !strings.HasPrefix(nd.Ref, prefix) {
			return
		}
		if newName, ok := rename[strings.TrimPrefix(nd.Ref, prefix)]; ok {
			nd.Ref = prefix + newName
		}
	})
}

// Flatten resolves the handle's definitions against each other (nested $defs hoisted,
// cross-refs rewritten, every named entry kept) into a fresh flat handle — how a
// user-supplied pool whose entries reference one another is brought into normal form.
func (d Defs) Flatten() (Defs, error) {
	named := make(map[string]Schema, len(d.m))
	for k, v := range d.m {
		named[k] = Schema{n: v} // bare nodes: cross-refs resolve at the container level
	}
	return FlattenNamed(named)
}

// JSONSchemaBytes INLINES the value schema rather than $ref-ing it: openapi.json would keep a
// dangling `#/$defs/SchemaSchema`.
func (Defs) JSONSchemaBytes() ([]byte, error) {
	value, err := Schema{}.JSONSchemaBytes()
	if err != nil {
		return nil, err
	}
	return []byte(`{"type":"object","additionalProperties":` + string(value) + `}`), nil
}

// DefsHandle returns a handle over the schema's own root $defs, sharing its map: a Set
// through it is visible to the schema and every sub-schema carrying the same defs.
func (s Schema) DefsHandle() Defs {
	return Defs{m: s.rootDefs()}
}

// WithoutDefs drops every $defs attachment, root and nested. Use it before storing a navigated
// sub-schema into the defs set that owns it, which would otherwise form a marshal cycle.
func (s Schema) WithoutDefs() Schema {
	return s.keepingContext(Schema{n: stripDefsDeep(s.n)})
}

// keepingContext carries guards and vars onto a re-anchored copy of the SAME context: defs
// methods only. Navigation must not — guards are keyed from the root.
func (s Schema) keepingContext(out Schema) Schema {
	out.guards = s.guards
	out.vars = s.vars
	return out
}

// stripDefsDeep walks rather than JSON round-tripping: an attached defs map can reach back into
// this tree and cycle the marshaler.
func stripDefsDeep(n *node) *node {
	if n == nil {
		return &node{}
	}
	m := mapChildren(n, func(sl childSlot, c *node) *node {
		if sl.kind == slotDefs || c == nil {
			return c // not descending into $defs is what terminates the walk
		}
		return stripDefsDeep(c)
	})
	m.Defs = nil
	return m
}

// FlattenNamed bundles named schemas into one flat set: each a root definition under its name
// (collisions suffixed), nested defs hoisted, $refs rewritten.
func FlattenNamed(named map[string]Schema) (Defs, error) {
	defs := make(map[string]*node, len(named))
	refs := make([]*node, 0, len(named))
	for name, s := range named {
		entry, err := deepClone(s.n)
		if err != nil {
			return Defs{}, err
		}
		if entry == nil {
			entry = &node{}
		}
		entry.ID = name
		defs[name] = entry
		refs = append(refs, &node{Ref: "#/$defs/" + name})
	}
	// AllOf is the internal bundling vehicle: it makes every named schema "used" so
	// normalize keeps it, while their $refs are rewritten against the merged root.
	container := &node{Defs: defs, AllOf: refs}
	normalized, err := normalize(container)
	if err != nil {
		return Defs{}, err
	}
	if normalized == nil || normalized.Defs == nil {
		return NewDefs(), nil
	}
	return Defs{m: normalized.Defs}, nil
}

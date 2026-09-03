package db

import (
	"encoding/json"
	"sort"

	"genroc/internal/model"
)

// minExternalizeBytes is the floor. Moving a value out costs an entry in the objects list -- its
// path, hash and size -- so a value smaller than its own entry makes the slot BIGGER. When the
// largest remaining candidate is under this, going finer cannot help and the cut coarsens
// instead. specs/object-store.md §Choosing what to externalize.
const minExternalizeBytes = 128

// node is one position in the value being cut, sized once so the search is arithmetic.
type node struct {
	path     []any
	value    any
	size     int64 // encoded bytes of this node in the ORIGINAL value
	depth    int
	parent   *node
	children []*node
	already  bool // an *ObjectRef that was already external when this value was read
}

// cutForSize decides which parts of v move to the object store so the stored slot fits target,
// and returns the value with those parts removed plus a ref for each.
//
// It externalizes the FEWEST, LARGEST leaves that get under the target, rather than everything
// over a per-piece threshold. Leaves first is what preserves sharing: a task input holding a
// bundle beside per-instance data must cut the bundle alone, or every instance hashes a
// different value and stores its own copy.
//
// The selection is SPLICED AND MEASURED before any ref is made. Node sizes drive the search --
// measuring at every step is quadratic in the value's bytes -- but they are an estimate, and the
// one that decides is the encoded size of the value that will actually be stored.
func cutForSize(v any, target int64) (any, []*model.ObjectRef, []*pendingObject, error) {
	root, err := buildTree(v, nil, 0, nil)
	if err != nil {
		return nil, nil, nil, err
	}
	nodes := collect(root)
	chosen := map[*node]bool{}
	var data, objects int64 = root.size, 0
	for _, n := range nodes {
		if n.already {
			chosen[n] = true
			data -= n.size
			objects += entrySize(n.path)
		}
	}

	byDepth := map[int][]*node{}
	maxDepth := 0
	for _, n := range nodes {
		byDepth[n.depth] = append(byDepth[n.depth], n)
		if n.depth > maxDepth {
			maxDepth = n.depth
		}
	}

	// Rounds, not one pass: the search runs on the estimate, then the candidate is spliced and
	// measured, and a measurement still over target re-seeds the estimate from the truth and
	// selects again. One round is the normal case; a second happens where the estimate was
	// optimistic, which is exactly where being wrong would have mattered.
	for {
		before := len(chosen)
		selectNodes(byDepth, maxDepth, chosen, &data, &objects, target)
		if len(chosen) == 0 {
			return v, nil, nil, nil
		}
		size, err := storedSize(v, chosen)
		if err != nil {
			return nil, nil, nil, err
		}
		if size <= target || len(chosen) == before {
			break // under target, or nothing left to choose
		}
		data = size - objects
	}
	return applyCut(deepCopy(v), root, chosen)
}

// selectNodes takes candidates deepest-first until the estimate fits. Only when a whole level is
// exhausted does the cut coarsen, and choosing a parent then un-chooses everything under it.
func selectNodes(byDepth map[int][]*node, maxDepth int, chosen map[*node]bool, data, objects *int64, target int64) {
	for depth := maxDepth; depth >= 0 && *data+*objects > target; depth-- {
		level := byDepth[depth]
		// Size descending, then path ascending. The tie-break is not cosmetic: two instances
		// must choose the SAME cut or identical content produces different objects and shares
		// nothing, and Go's map iteration is randomized.
		sort.Slice(level, func(i, j int) bool {
			if level[i].size != level[j].size {
				return level[i].size > level[j].size
			}
			return pathLess(level[i].path, level[j].path)
		})
		for _, n := range level {
			if *data+*objects <= target {
				break
			}
			if n.already || chosen[n] || coveredByAncestor(n, chosen) {
				continue
			}
			// An already-external descendant is a MARKER, not content: marshalling this node
			// would bake it into an object whose content is opaque, so nothing could ever
			// resolve it — and it would drop out of the referenced set the next write diffs
			// against, releasing the claim while the content still points at it. Go finer
			// instead; the descendant stays its own ref, which is also what shares it.
			if holdsAlready(n) {
				continue
			}
			if n.size < minExternalizeBytes {
				break // nothing smaller at this level can help; coarsen instead
			}
			for _, d := range collect(n)[1:] { // drop descendants this node now contains
				if chosen[d] {
					delete(chosen, d)
					*data += d.size
					*objects -= entrySize(d.path)
				}
			}
			chosen[n] = true
			*data -= n.size
			*objects += entrySize(n.path)
		}
	}
}

// holdsAlready reports whether anything under n was already external when the value was read.
func holdsAlready(n *node) bool {
	for _, d := range collect(n)[1:] {
		if d.already {
			return true
		}
	}
	return false
}

// storedSize is what this selection actually costs the row: the spliced value's encoded bytes
// plus one objects-list entry per ref. Spliced in memory and thrown away — no hashing, no
// object content, nothing written until the selection is settled.
func storedSize(v any, chosen map[*node]bool) (int64, error) {
	stripped := deepCopy(v)
	picked := pickedInCutOrder(chosen)
	var entries int64
	for _, n := range picked {
		entries += entrySize(n.path)
		if len(n.path) == 0 {
			stripped = nil
			continue
		}
		removeAt(stripped, n.path)
	}
	b, err := json.Marshal(stripped)
	if err != nil {
		return 0, err
	}
	return int64(len(b)) + entries, nil
}

// pickedInCutOrder is the chosen set deepest-first, so removing a child cannot disturb a path
// still to be walked. Shared by the measurement and the cut so the two cannot diverge.
func pickedInCutOrder(chosen map[*node]bool) []*node {
	picked := make([]*node, 0, len(chosen))
	for n := range chosen {
		picked = append(picked, n)
	}
	sort.Slice(picked, func(i, j int) bool {
		if picked[i].depth != picked[j].depth {
			return picked[i].depth > picked[j].depth
		}
		return pathLess(picked[i].path, picked[j].path)
	})
	return picked
}

// applyCut removes the chosen nodes from the value and turns each into a ref plus the object to
// write. Deepest first, so removing a child cannot disturb a path still to be walked.
func applyCut(v any, root *node, chosen map[*node]bool) (any, []*model.ObjectRef, []*pendingObject, error) {
	picked := pickedInCutOrder(chosen)

	var refs []*model.ObjectRef
	var objs []*pendingObject
	out := v
	for _, n := range picked {
		if n.already {
			ref := n.value.(*model.ObjectRef)
			refs = append(refs, &model.ObjectRef{Ref: ref.Ref, Size: ref.Size, Path: n.path})
			if len(n.path) == 0 {
				out = nil
			} else {
				model.Place(out, n.path, nil)
				removeAt(out, n.path)
			}
			continue
		}
		b, err := json.Marshal(n.value)
		if err != nil {
			return nil, nil, nil, err
		}
		h := hashContent(b)
		refs = append(refs, &model.ObjectRef{Ref: h, Size: int64(len(b)), Path: n.path})
		objs = append(objs, &pendingObject{Hash: h, Content: string(b), Size: int64(len(b))})
		if len(n.path) == 0 {
			out = nil
			continue
		}
		removeAt(out, n.path)
	}
	// Stable order out, for the same reason the tie-break exists.
	sort.Slice(refs, func(i, j int) bool { return pathLess(refs[i].Path, refs[j].Path) })
	return out, refs, objs, nil
}

// removeAt deletes the value at path: a map key goes away entirely (so an absent slot is absent,
// not null), an array element becomes null (removing it would renumber every later index and
// invalidate the paths beside it).
func removeAt(root any, path []any) {
	if len(path) == 0 {
		return
	}
	cur := root
	for i := 0; i < len(path)-1; i++ {
		switch n := cur.(type) {
		case map[string]any:
			key, ok := path[i].(string)
			if !ok {
				return
			}
			cur = n[key]
		case []any:
			idx, ok := path[i].(int)
			if !ok || idx < 0 || idx >= len(n) {
				return
			}
			cur = n[idx]
		default:
			return
		}
	}
	switch n := cur.(type) {
	case map[string]any:
		if key, ok := path[len(path)-1].(string); ok {
			delete(n, key)
		}
	case []any:
		if idx, ok := path[len(path)-1].(int); ok && idx >= 0 && idx < len(n) {
			n[idx] = nil
		}
	}
}

// buildTree sizes every node bottom-up in one pass. Sizes are computed rather than measured per
// node: json.Marshal at every position would be quadratic in the value's bytes, and the encoding
// is exactly composable -- a map is braces plus keys, colons, commas and its children.
func buildTree(v any, path []any, depth int, parent *node) (*node, error) {
	n := &node{path: path, value: v, depth: depth, parent: parent}
	switch t := v.(type) {
	case *model.ObjectRef:
		// Already external when this value was read. It stays out, and its cost is its entry.
		n.already = true
		n.size = 0
		return n, nil
	case map[string]any:
		keys := make([]string, 0, len(t))
		for k := range t {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		n.size = 2 // {}
		for i, k := range keys {
			child, err := buildTree(t[k], childPathOf(path, k), depth+1, n)
			if err != nil {
				return nil, err
			}
			n.children = append(n.children, child)
			kb, err := json.Marshal(k)
			if err != nil {
				return nil, err
			}
			n.size += int64(len(kb)) + 1 + child.rawSize() // "key":value
			if i > 0 {
				n.size++ // ,
			}
		}
		return n, nil
	case []any:
		n.size = 2 // []
		for i, item := range t {
			child, err := buildTree(item, childPathOf(path, i), depth+1, n)
			if err != nil {
				return nil, err
			}
			n.children = append(n.children, child)
			n.size += child.rawSize()
			if i > 0 {
				n.size++
			}
		}
		return n, nil
	}
	b, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	n.size = int64(len(b))
	return n, nil
}

// rawSize is what this node occupies in its parent's encoding. An already-external node is a
// marker its parent does not carry, so it costs nothing there.
func (n *node) rawSize() int64 {
	if n.already {
		return 4 // null, the placeholder its absence leaves in a sizing pass
	}
	return n.size
}

// collect returns n and every descendant, n first.
func collect(n *node) []*node {
	out := []*node{n}
	for _, c := range n.children {
		out = append(out, collect(c)...)
	}
	return out
}

func coveredByAncestor(n *node, chosen map[*node]bool) bool {
	for p := n.parent; p != nil; p = p.parent {
		if chosen[p] {
			return true
		}
	}
	return false
}

// entrySize is what one ref costs in the objects list: its path, hash and size.
func entrySize(path []any) int64 {
	b, err := json.Marshal(&model.ObjectRef{Ref: "0123456789abcdef0123456789abcdef", Size: 1 << 20, Path: path})
	if err != nil {
		return 96
	}
	return int64(len(b)) + 1 // plus the separating comma
}

// pathLess orders paths so a tie between equally sized candidates resolves the same way in every
// process. Shorter first, then by segment; a string sorts before a number at the same position.
func pathLess(a, b []any) bool {
	for i := 0; i < len(a) && i < len(b); i++ {
		as, aIsStr := a[i].(string)
		bs, bIsStr := b[i].(string)
		switch {
		case aIsStr && bIsStr:
			if as != bs {
				return as < bs
			}
		case aIsStr != bIsStr:
			return aIsStr
		default:
			ai, _ := a[i].(int)
			bi, _ := b[i].(int)
			if ai != bi {
				return ai < bi
			}
		}
	}
	return len(a) < len(b)
}

// deepCopy duplicates the containers a decoded value is made of, so a cut can remove from the
// copy without touching the original. Leaves are shared: they are never mutated, only dropped.
func deepCopy(v any) any {
	switch t := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, val := range t {
			out[k] = deepCopy(val)
		}
		return out
	case []any:
		out := make([]any, len(t))
		for i, val := range t {
			out[i] = deepCopy(val)
		}
		return out
	}
	return v
}

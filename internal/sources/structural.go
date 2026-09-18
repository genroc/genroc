package sources

// The structural phase: a resolver that may change what the typechecker sees, run before
// validation. specs/source-resolution.md §The two phases and §`$process`.

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"

	"genroc/internal/defdoc"
	"genroc/internal/schema"
	"genroc/internal/validation"
)

// spread reports whether this site pre-fills the mapping around it rather than filling a slot.
// Position is the whole rule: the same directive string means both.
func (s site) spread() bool {
	return len(s.loc) > 0 && s.loc[len(s.loc)-1] == any(defdoc.MergeKey)
}

// resolveStructuralPass resolves every structural directive in docs, mutating them in place.
// stack is the chain of files being resolved, which is how a spread cycle is refused by path.
func resolveStructuralPass(docs []sourceDoc, cfg projectConfig, stack []string) (int, error) {
	sites, err := findSites(docs, cfg)
	if err != nil {
		return 0, err
	}
	n := 0
	for _, s := range sites {
		if cfg.Resolvers[s.resolverIdx].Phase != phaseStructural {
			continue
		}
		rc := cfg.Resolvers[s.resolverIdx]
		if rc.Name != builtinProcess || len(rc.Command) > 0 {
			return 0, fmt.Errorf("%s: %s: structural resolver %q is not implemented - only the "+
				"built-in %q runs today", docs[s.docIdx].File, s.Pointer, rc.Name, builtinProcess)
		}
		here, err := filepath.Abs(docs[s.docIdx].File)
		if err != nil {
			return 0, err
		}
		value, err := resolveProcessDirective(docs[s.docIdx].File, s.Argument, append(stack, here))
		if err != nil {
			return 0, fmt.Errorf("%s: %s: %w", docs[s.docIdx].File, s.Pointer, err)
		}
		if err := applyStructural(docs, s, value); err != nil {
			return 0, err
		}
		n++
	}
	return n, nil
}

// applyStructural writes a structural result into the document: a spread pre-fills the mapping
// the `<<` sits in and an explicit key beats it, a slot site replaces the leaf.
func applyStructural(docs []sourceDoc, s site, value map[string]any) error {
	if !s.spread() {
		return splice(docs, s, value)
	}
	parent, err := containerOf(docs, s)
	if err != nil {
		return err
	}
	// The explicit keys are already in parent; only what it does not carry is taken. Deleting
	// the directive last keeps parent non-empty for a caller inspecting it mid-merge.
	for k, v := range value {
		if _, taken := parent[k]; !taken {
			parent[k] = v
		}
	}
	delete(parent, defdoc.MergeKey)
	return nil
}

// containerOf returns the mapping a spread site sits in.
func containerOf(docs []sourceDoc, s site) (map[string]any, error) {
	node := docs[s.docIdx].Value
	for _, seg := range s.loc[:len(s.loc)-1] {
		switch k := seg.(type) {
		case string:
			m, ok := node.(map[string]any)
			if !ok {
				return nil, fmt.Errorf("%s: cannot descend into %s", docs[s.docIdx].File, s.Pointer)
			}
			node = m[k]
		case int:
			a, ok := node.([]any)
			if !ok || k >= len(a) {
				return nil, fmt.Errorf("%s: cannot descend into %s", docs[s.docIdx].File, s.Pointer)
			}
			node = a[k]
		}
	}
	m, ok := node.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("%s: %s: `%s` is only a spread inside a mapping",
			docs[s.docIdx].File, s.Pointer, defdoc.MergeKey)
	}
	return m, nil
}

// resolveProcessDirective answers `$process: <path>` with the call-site pre-fill for a child of
// that definition: name, input_schema, result_schema and raises. Three of the four are inferred
// rather than read -- a definition's output type is a Shape and `raises` is a scan over its raise
// clauses -- which is why this is built in and no external binary can produce it. `input_schema`
// is the one COPY, and the one the caller can be checked against later.
func resolveProcessDirective(fromFile, argument string, stack []string) (map[string]any, error) {
	target, err := filepath.Abs(filepath.Join(filepath.Dir(fromFile), argument))
	if err != nil {
		return nil, err
	}
	// The spread graph must be acyclic even though the CALL graph need not be: a recursive
	// child is ordinary, but it cannot type itself by reference. specs/source-resolution.md
	// §Ordering, and the recursion it cannot type.
	for i, f := range stack {
		if f == target {
			return nil, fmt.Errorf("spread cycle: %s", strings.Join(append(append([]string{}, stack[i:]...), target), " -> "))
		}
	}

	docs, err := loadSourceDocs([]string{target})
	if err != nil {
		return nil, err
	}
	if len(docs) != 1 {
		return nil, fmt.Errorf("%s holds %d definitions - a spread names exactly one", argument, len(docs))
	}
	cfg, err := findProjectConfig(filepath.Dir(target))
	if err != nil {
		return nil, err
	}
	if _, err := resolveStructuralPass(docs, cfg, append(stack, target)); err != nil {
		return nil, err
	}

	def, err := decodeDefinition(docs[0])
	if err != nil {
		return nil, err
	}
	sf, err := validation.Generate(def)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", argument, err)
	}

	out := map[string]any{"name": sf.Process}
	// The input side is a COPY, not an inference: a definition's input_schema is written by its
	// author, so the spread reproduces it. That makes the registration check against the child a
	// check that the copy is still current, and it is what lets an editor check the call offline.
	if def.InputSchema != nil {
		in, err := selfContainedSchema(*def.InputSchema, sf.Defs)
		if err != nil {
			return nil, err
		}
		if in != nil {
			out["input_schema"] = in
		}
	}
	result, err := selfContainedSchema(sf.ProcessOutput, sf.Defs)
	if err != nil {
		return nil, err
	}
	if result != nil {
		out["result_schema"] = result
	}
	if len(sf.Raises) > 0 {
		raises := map[string]any{}
		for code, s := range sf.Raises {
			r, err := selfContainedSchema(s, sf.Defs)
			if err != nil {
				return nil, err
			}
			raises[code] = r
		}
		out["raises"] = raises
	}
	return out, nil
}

// selfContainedSchema renders one schema out of a definition's pool so it can stand alone in a
// slot: canonical, carrying only the `$defs` its refs reach, and unwrapped where the whole
// answer is a ref to a non-recursive definition. A nil answer means the definition types nothing
// there, which is a different fact from typing it as empty.
func selfContainedSchema(s schema.Schema, pool schema.Defs) (any, error) {
	if s.IsZero() {
		return nil, nil
	}
	raw, err := json.Marshal(s.Canonicalize().WithDefs(pool))
	if err != nil {
		return nil, err
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, err
	}
	contained, err := selfContained(doc)
	if err != nil {
		return nil, err
	}
	return unwrapRootRef(contained)
}

// unwrapRootRef replaces a document that is nothing but `$ref` with the definition it names,
// where that definition does not reach itself. The indirection is inference's -- every process
// output is stored as a ref (validation.Generate) -- and carrying it into a `result_schema`
// would make every spread read as a pointer to a type instead of as the type.
func unwrapRootRef(doc map[string]any) (map[string]any, error) {
	ref, ok := doc["$ref"].(string)
	if !ok || len(doc) > 2 {
		return doc, nil
	}
	defs, _ := doc["$defs"].(map[string]any)
	name := strings.TrimPrefix(ref, "#/$defs/")
	body, ok := defs[name].(map[string]any)
	if !ok {
		return doc, nil
	}
	// Reachable from itself means the ref is load-bearing: a recursive type has to keep a name.
	inner := map[string]bool{}
	collectRefs(body, inner)
	if inner[name] {
		return doc, nil
	}
	out := map[string]any{}
	for k, v := range body {
		out[k] = v
	}
	rest := map[string]any{}
	for k, v := range defs {
		if k != name {
			rest[k] = v
		}
	}
	kept, err := reachableDefs(rest, out)
	if err != nil {
		return nil, err
	}
	if len(kept) > 0 {
		out["$defs"] = kept
	}
	return out, nil
}

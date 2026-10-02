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
	all, err := findSites(docs, cfg)
	if err != nil {
		return 0, err
	}
	var sites []site
	for _, s := range all {
		if cfg.Resolvers[s.resolverIdx].Phase == phaseStructural {
			sites = append(sites, s)
		}
	}
	values, err := structuralValues(docs, cfg, sites, stack)
	if err != nil {
		return 0, err
	}
	for i, s := range sites {
		if err := applyStructural(docs, s, values[i]); err != nil {
			return 0, err
		}
	}
	return len(sites), nil
}

// structuralValues answers every site, parallel to sites. A registered resolver runs ONCE with
// every site that named it -- N directives must not mean N processes.
func structuralValues(docs []sourceDoc, cfg projectConfig, sites []site, stack []string) ([]any, error) {
	out := make([]any, len(sites))
	byResolver := map[int][]site{}
	var order []int
	for i, s := range sites {
		s.ord = i
		if _, seen := byResolver[s.resolverIdx]; !seen {
			order = append(order, s.resolverIdx)
		}
		byResolver[s.resolverIdx] = append(byResolver[s.resolverIdx], s)
	}
	for _, idx := range order {
		rc, group := cfg.Resolvers[idx], byResolver[idx]
		if len(rc.Command) == 0 {
			if rc.Name != builtinProcess {
				return nil, fmt.Errorf("structural resolver %q has no command and is not one genctl answers itself", rc.Name)
			}
			for _, s := range group {
				here, err := filepath.Abs(docs[s.docIdx].File)
				if err != nil {
					return nil, err
				}
				value, err := resolveProcessDirective(docs[s.docIdx].File, s.Argument, append(stack, here))
				if err != nil {
					return nil, fmt.Errorf("%s: %s: %w", docs[s.docIdx].File, renderPointer(s.Pointer), err)
				}
				out[s.ord] = value
			}
			continue
		}
		// No schemas: phase 1 runs before inference, so the manifest carries no types and no pool.
		processes, err := byProcess(nil, docs, group)
		if err != nil {
			return nil, err
		}
		m := manifest{Mode: phaseStructural, Processes: processes}
		values, err := runStructuralResolver(cfg, rc, m)
		if err != nil {
			return nil, err
		}
		for i, s := range m.flatten() {
			if err := refuseNestedStructural(docs[s.docIdx].File, cfg, rc, s, values[i]); err != nil {
				return nil, err
			}
			out[s.ord] = values[i]
		}
	}
	return out, nil
}

// refuseNestedStructural refuses a structural directive inside a resolver's answer: the pass runs
// once, so it would reach the server as a literal. specs/source-resolution.md §Registered structural resolvers.
func refuseNestedStructural(file string, cfg projectConfig, rc resolverConfig, s site, value any) error {
	at := append([]any(nil), s.Pointer...)
	if s.spread() {
		at = at[:len(at)-1]
	}
	var walk func(v any, path []any) error
	walk = func(v any, path []any) error {
		switch t := v.(type) {
		case map[string]any:
			for k, child := range t {
				if err := walk(child, append(path, k)); err != nil {
					return err
				}
			}
		case []any:
			for i, child := range t {
				if err := walk(child, append(path, i)); err != nil {
					return err
				}
			}
		case string:
			name, argument, ok := defdoc.Directive(t)
			if !ok {
				return nil
			}
			if idx, _, ok := cfg.matchResolver(name, argument); ok && cfg.Resolvers[idx].Phase == phaseStructural {
				return fmt.Errorf("%s: %s: resolver %q answered with the structural directive %q, which is never "+
					"resolved: structural resolution is one pass, so return the resolved value instead",
					file, renderPointer(path), rc.Name, t)
			}
		}
		return nil
	}
	return walk(value, at)
}

// unescapeDocs collapses `$$name: …` to `$name: …` in every string leaf. It must run LAST, after
// every walk that looks for a directive, which is why callers finalise rather than the pass.
// specs/source-resolution.md §Escaping on the way IN.
func unescapeDocs(docs []sourceDoc) {
	for i := range docs {
		docs[i].Value = unescapeDirectives(docs[i].Value)
	}
}

func unescapeDirectives(v any) any {
	switch t := v.(type) {
	case map[string]any:
		for k, child := range t {
			t[k] = unescapeDirectives(child)
		}
	case []any:
		for i, child := range t {
			t[i] = unescapeDirectives(child)
		}
	case string:
		if un, ok := defdoc.UnescapeDirective(t); ok {
			return un
		}
	}
	return v
}

// applyStructural writes a structural result into the document: a spread pre-fills the mapping
// the `<<` sits in and an explicit key beats it, a slot site replaces the leaf.
func applyStructural(docs []sourceDoc, s site, value any) error {
	if !s.spread() {
		return splice(docs, s, value)
	}
	fields, ok := value.(map[string]any)
	if !ok {
		return fmt.Errorf("%s: %s: `%s` spreads a mapping, and %q answered with %s",
			docs[s.docIdx].File, renderPointer(s.Pointer), defdoc.MergeKey, s.Resolver, kindOf(value))
	}
	parent, err := containerOf(docs, s)
	if err != nil {
		return err
	}
	// The explicit keys are already in parent; only what it does not carry is taken. Deleting
	// the directive last keeps parent non-empty for a caller inspecting it mid-merge.
	for k, v := range fields {
		if _, taken := parent[k]; !taken {
			parent[k] = v
		}
	}
	delete(parent, defdoc.MergeKey)
	return nil
}

func kindOf(v any) string {
	switch v.(type) {
	case nil:
		return "null"
	case []any:
		return "a list"
	case string:
		return "a string"
	case bool:
		return "a boolean"
	case map[string]any:
		return "a mapping"
	}
	return "a number"
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
// that definition. specs/source-resolution.md §`$process`.
func resolveProcessDirective(fromFile, argument string, stack []string) (map[string]any, error) {
	target, err := filepath.Abs(filepath.Join(filepath.Dir(fromFile), argument))
	if err != nil {
		return nil, err
	}
	// The spread graph must be acyclic though the CALL graph need not be.
	// specs/source-resolution.md §Ordering, and the recursion it cannot type.
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

	// No code phase runs on a child, so the document is final once its own spreads are in.
	unescapeDocs(docs)

	def, err := decodeDefinition(docs[0])
	if err != nil {
		return nil, err
	}
	sf, err := validation.Generate(def)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", argument, err)
	}

	out := map[string]any{"name": sf.Process}
	// The input side is a COPY of the authored schema, not an inference: the registration check
	// against the child then checks that the copy is current.
	if def.InputSchema != nil {
		in, err := selfContainedSchema(*def.InputSchema, sf.Defs, false)
		if err != nil {
			return nil, err
		}
		if in != nil {
			out["input_schema"] = in
		}
	}
	result, err := selfContainedSchema(sf.ProcessOutput, sf.Defs, true)
	if err != nil {
		return nil, err
	}
	if result != nil {
		out["result_schema"] = result
	}
	if len(sf.Raises) > 0 {
		raises := map[string]any{}
		for code, s := range sf.Raises {
			r, err := selfContainedSchema(s, sf.Defs, true)
			if err != nil {
				return nil, err
			}
			raises[code] = r
		}
		out["raises"] = raises
	}
	return out, nil
}

// selfContainedSchema renders one schema to stand alone in a slot, with only the `$defs` its refs
// reach. nil means nothing is typed there, not empty. Only an INFERRED schema is canonicalized:
// Canonicalize drops `description`, which the authored copy is kept for.
func selfContainedSchema(s schema.Schema, pool schema.Defs, inferred bool) (any, error) {
	if s.IsZero() {
		return nil, nil
	}
	if inferred {
		s = s.Canonicalize()
	}
	raw, err := json.Marshal(s.WithDefs(pool))
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

// unwrapRootRef replaces a document that is only a `$ref` with the non-recursive definition it
// names: every process output is stored as a ref, and a spread should read as the type itself.
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

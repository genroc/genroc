package sources

// Schema DOCUMENTS rather than typed schemas: what a resolver hands back and what a
// `result_schema` holds are plain maps, and these render one out of a definition's pool so
// it can stand alone in a slot.

import (
	"encoding/json"
	"fmt"
	"maps"
	"strings"

	"genroc/internal/numeric"
	"genroc/internal/schema"
)

// schemaDoc renders one schema as the JSON document it is, `$defs` included — a Schema carries
// its pool, which is what makes an answer self-contained before it is narrowed.
func schemaDoc(s schema.Schema) (map[string]any, error) {
	raw, err := json.Marshal(s)
	if err != nil {
		return nil, fmt.Errorf("render schema: %w", err)
	}
	var doc map[string]any
	// numeric.Decode, not json.Unmarshal: a schema carries `default`, and a default is a literal
	// someone wrote. specs/number-precision.md.
	if err := numeric.Decode(raw, &doc); err != nil {
		return nil, fmt.Errorf("render schema: %w", err)
	}
	return doc, nil
}

// selfContained narrows `$defs` to what the document's refs actually reach, so what is printed
// can be piped into a generator whole. Refs BETWEEN definitions are followed, which is what
// keeps a task output that references itself resolvable.
func selfContained(doc map[string]any) (map[string]any, error) {
	// The pool travels with every arm of a union, not only with the root, so it is collected
	// from wherever it sits and printed once — three copies of the same definitions is not a
	// document anyone wants to read or pipe.
	pool := map[string]any{}
	body, _ := hoistDefs(doc, pool).(map[string]any)
	if len(pool) == 0 {
		return body, nil
	}

	collapseAliases(pool, body)
	kept, err := reachableDefs(pool, body)
	if err != nil {
		return nil, err
	}
	if len(kept) > 0 {
		body["$defs"] = kept
	}
	return body, nil
}

// reachableDefs is the subset of pool that from can reach, following refs between definitions —
// which is what keeps a task output that references itself resolvable. Shared with the resolver
// manifest, which narrows a process's pool to what its sites' fragments name.
func reachableDefs(pool map[string]any, from ...any) (map[string]any, error) {
	want := map[string]bool{}
	for _, v := range from {
		collectRefs(v, want)
	}
	kept := map[string]any{}
	for {
		next := ""
		for name := range want {
			if _, done := kept[name]; !done {
				next = name
				break
			}
		}
		if next == "" {
			return kept, nil
		}
		def, ok := pool[next]
		if !ok {
			// A ref with no definition is a bug upstream, not something to hide by dropping it.
			return nil, fmt.Errorf("schema references $defs/%s, which the pool does not carry", next)
		}
		kept[next] = def
		collectRefs(def, want)
	}
}

// collapseAliases rewrites a ref to an alias-only definition — one whose whole document is a
// `$ref` — as a ref to what it names, and drops it: inference declares `<id>_output` for every
// task, and where the output simply IS another definition the leftover says nothing. pool and docs
// are rewritten IN PLACE, and docs must be EVERY document that can reference the pool.
func collapseAliases(pool map[string]any, docs ...any) {
	alias := map[string]string{}
	for name, def := range pool {
		if to, ok := soleRef(def); ok {
			if _, defined := pool[to]; defined {
				alias[name] = to
			}
		}
	}
	final := map[string]string{}
	for name := range alias {
		if to, ok := chaseAlias(alias, name); ok {
			final[name] = to
		}
	}
	if len(final) == 0 {
		return
	}
	for _, doc := range docs {
		rewriteRefs(doc, final)
	}
	for _, def := range pool {
		rewriteRefs(def, final)
	}
	for name := range final {
		delete(pool, name)
	}
}

func collectRefs(v any, out map[string]bool) {
	switch t := v.(type) {
	case map[string]any:
		for k, val := range t {
			if k == "$ref" {
				if s, ok := val.(string); ok {
					if name, ok := strings.CutPrefix(s, "#/$defs/"); ok {
						out[name] = true
					}
				}
				continue
			}
			collectRefs(val, out)
		}
	case []any:
		for _, e := range t {
			collectRefs(e, out)
		}
	}
}

// soleRef is the name a document points at when that is ALL it is: a `$ref` beside a
// `description` still carries something the ref does not.
func soleRef(v any) (string, bool) {
	doc, ok := v.(map[string]any)
	if !ok || len(doc) != 1 {
		return "", false
	}
	ref, ok := doc["$ref"].(string)
	if !ok {
		return "", false
	}
	return strings.CutPrefix(ref, "#/$defs/")
}

// chaseAlias follows an alias to the definition that is not one. A cycle of aliases names no
// type at all, so it is left exactly as it is rather than collapsed to an arbitrary member.
func chaseAlias(alias map[string]string, name string) (string, bool) {
	// One probe per alias, plus the one that finds the target is not itself an alias.
	to := name
	for range len(alias) + 1 {
		next, ok := alias[to]
		if !ok {
			return to, to != name
		}
		to = next
	}
	return "", false
}

func rewriteRefs(v any, alias map[string]string) {
	switch t := v.(type) {
	case map[string]any:
		for k, sub := range t {
			if k == "$ref" {
				if ref, ok := sub.(string); ok {
					if name, ok := strings.CutPrefix(ref, "#/$defs/"); ok {
						if to, ok := alias[name]; ok {
							t[k] = "#/$defs/" + to
						}
					}
				}
				continue
			}
			rewriteRefs(sub, alias)
		}
	case []any:
		for _, e := range t {
			rewriteRefs(e, alias)
		}
	}
}

// hoistDefs returns v with every `$defs` removed, merging them into pool. The pool is one
// object shared by every level, so merging cannot lose a definition.
func hoistDefs(v any, pool map[string]any) any {
	switch node := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(node))
		for k, sub := range node {
			if k == "$defs" {
				defs, _ := sub.(map[string]any)
				maps.Copy(pool, defs)
				continue
			}
			out[k] = hoistDefs(sub, pool)
		}
		return out
	case []any:
		out := make([]any, len(node))
		for i, sub := range node {
			out[i] = hoistDefs(sub, pool)
		}
		return out
	}
	return v
}

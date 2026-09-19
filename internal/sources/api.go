package sources

// The exported surface. Source resolution lived in `cmd/genctl` until the language server
// needed the STRUCTURAL phase: a `<<` spread changes which keys a document has, so an editor
// that skips it reports a document nobody applies -- `unknown field "<<"` on valid text.
// Everything here is a name for something already spelled out unexported below; nothing is a
// second implementation. specs/source-resolution.md, specs/language-server.md section 4.

import (
	"strconv"
	"strings"

	"genroc/internal/model"
	"genroc/internal/schema"
)

// Config is a project's resolver registry, read from the nearest `.genroc`.
type Config = projectConfig

// Doc is one definition document with the file it came from -- a directive's path resolves
// against that file, and an error has to name it.
type Doc = sourceDoc

// Site is one resolution directive found in a document.
type Site = site

// FindProjectConfig reads the nearest `.genroc` at or above dir.
func FindProjectConfig(dir string) (Config, error) { return findProjectConfig(dir) }

// DefaultDefinitionPaths is the `definitions:` patterns a bare command reads.
func DefaultDefinitionPaths(dir string) []string { return defaultDefinitionPaths(dir) }

// LoadDocs reads every definition in files, keeping each document's origin.
func LoadDocs(files []string) ([]Doc, error) { return loadSourceDocs(files) }

// ResolveStructuralPass resolves every structural directive in docs, MUTATING them in place,
// and returns how many it resolved. stack is the chain of files being resolved, by which a
// spread cycle is refused; a caller starting fresh passes nil.
func ResolveStructuralPass(docs []Doc, cfg Config, stack []string) (int, error) {
	return resolveStructuralPass(docs, cfg, stack)
}

// StructuralValueAt answers ONE structural directive without applying it: the value the site at
// path would be filled or spread with, which is what an editor shows over it. structural is
// false, with no error, where the directive at path is a code-phase one -- the editor never runs
// that phase -- or where path holds no directive; an error is the resolution's own. doc is the
// text as written, since a resolved document no longer holds the site, and it is not mutated.
func StructuralValueAt(doc Doc, cfg Config, path string) (value any, structural bool, err error) {
	docs := []sourceDoc{doc}
	sites, i, err := siteAt(docs, cfg, path)
	if err != nil || i < 0 || cfg.Resolvers[sites[i].resolverIdx].Phase != phaseStructural {
		return nil, false, err
	}
	values, err := structuralValues(docs, cfg, []site{sites[i]}, nil)
	if err != nil {
		return nil, false, err
	}
	return values[0], true, nil
}

// siteAt finds the directive an editor path names among a document's sites: -1 for none.
func siteAt(docs []sourceDoc, cfg projectConfig, path string) ([]site, int, error) {
	sites, err := findSites(docs, cfg)
	if err != nil {
		return nil, -1, err
	}
	for i, s := range sites {
		if pointerAddress(s.Pointer) == path {
			return sites, i, nil
		}
	}
	return sites, -1, nil
}

// pointerAddress spells a pointer the way defdoc addresses a node: dotted, a task by id, an
// index by number. Not renderPointer, which quotes a key no identifier can spell.
func pointerAddress(p []any) string {
	parts := make([]string, len(p))
	for i, seg := range p {
		switch v := seg.(type) {
		case string:
			parts[i] = v
		case int:
			parts[i] = strconv.Itoa(v)
		}
	}
	return strings.Join(parts, ".")
}

// ResolveCode runs the code phase: every phase-2 resolver, shelling out to the command each
// one names. mode is the resolver protocol's mode ("resolve" or "types").
func ResolveCode(docs []Doc, mode string) (int, error) { return resolveDocs(docs, mode) }

// DecodeDefinition decodes one document into a definition.
func DecodeDefinition(d Doc) (*model.ProcessDefinition, error) { return decodeDefinition(d) }

// Splice writes a value into the slot a site names.
func Splice(docs []Doc, s Site, value any) error { return splice(docs, s, value) }

// SelfContained narrows a schema document's `$defs` to what its refs reach.
func SelfContained(doc map[string]any) (map[string]any, error) { return selfContained(doc) }

// SchemaDoc renders one schema as the JSON document it is printed as.
func SchemaDoc(s schema.Schema) (map[string]any, error) { return schemaDoc(s) }

// CollectRefs adds every `$defs` name v references to out.
func CollectRefs(v any, out map[string]bool) { collectRefs(v, out) }

// ReachableDefs is the subset of pool that from can reach.
func ReachableDefs(pool map[string]any, from ...any) (map[string]any, error) {
	return reachableDefs(pool, from...)
}

// CollapseAliases rewrites refs to alias-only definitions, in place.
func CollapseAliases(pool map[string]any, docs ...any) { collapseAliases(pool, docs...) }

// Suffixes reports the argument suffixes a resolver accepts, and whether any entry carries the
// name at all. An empty list with ok=true accepts anything — one entry that accepts everything
// makes the whole name unfiltered, because matchResolver takes the first entry that fits.
//
// It is here for the editor, which offers a path before there is an argument to match. The
// RESOLVER still treats the argument verbatim (findSites): this is what may be suggested, never
// what is accepted.
func Suffixes(c Config, name string) ([]string, bool) {
	var out []string
	known := false
	for _, r := range c.Resolvers {
		if r.Name != name {
			continue
		}
		known = true
		if len(r.Ext) == 0 {
			return nil, true
		}
		out = append(out, r.Ext...)
	}
	return out, known
}

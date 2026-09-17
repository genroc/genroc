package lsp

// The STRUCTURAL phase of source resolution, run before the verdict. A `<<` spread changes
// which keys a document HAS -- it fills `name`, `result_schema` and `raises` from a child
// definition -- so analysing the text as written reports a document nobody applies: `unknown
// field "<<"`, and then every key the spread would have supplied as missing. The pass itself is
// `internal/sources`, the same one `apply` runs; nothing here decides what a directive means.
// specs/source-resolution.md, specs/language-server.md §4.

import (
	"path/filepath"

	"genroc/internal/defdoc"
	"genroc/internal/sources"
)

// resolveStructural returns doc's value with every structural directive resolved, leaving the
// document itself untouched: positions come from the index beside it, and an injected key has
// no node there, so the verdict reads a copy and every diagnostic still locates against what
// the author wrote.
//
// A document with no path on disk is returned as-is. A directive's argument is relative to the
// file holding it, so an unsaved buffer has nothing to resolve against.
func resolveStructural(doc *defdoc.Doc, file string) (any, error) {
	if file == "" {
		return doc.Value, nil
	}
	cfg, err := sources.FindProjectConfig(filepath.Dir(file))
	if err != nil {
		// A malformed `.genroc` is the project's problem, not this document's, and the file it
		// names is not the one open. Analysing the text as written is the better failure.
		return doc.Value, nil
	}
	copied := deepCopy(doc.Value)
	docs := []sources.Doc{{Value: copied, File: file}}
	if _, err := sources.ResolveStructuralPass(docs, cfg, nil); err != nil {
		return nil, err
	}
	return docs[0].Value, nil
}

// deepCopy clones the containers and shares the leaves, which are values. Marshalling through
// JSON would be shorter and would round a `default` someone wrote (specs/number-precision.md).
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
	default:
		return v
	}
}

// resolveStructuralInPlace resolves INTO doc.Value, for a caller that reads values back through
// `doc.ValueAt` -- those are references into the same containers the pass fills, so a copy would
// leave them as written. The index and the value disagree afterwards (an injected key has no
// span), which is why this is only for a document the caller parsed itself and throws away.
func resolveStructuralInPlace(doc *defdoc.Doc, file string) {
	if file == "" {
		return
	}
	cfg, err := sources.FindProjectConfig(filepath.Dir(file))
	if err != nil {
		return
	}
	// A directive that will not resolve leaves the document as written, which is the answer a
	// closed set falls back to; the diagnostics path is where it is reported.
	_, _ = sources.ResolveStructuralPass([]sources.Doc{{Value: doc.Value, File: file}}, cfg, nil)
}

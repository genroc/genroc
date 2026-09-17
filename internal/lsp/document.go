package lsp

// A document as the server sees it: the text as written, indexed for positions, and -- read
// lazily, and only through the decoded definition -- the value an apply would see. Every
// handler starts from one of these, so this is the ONE place text becomes a document and the
// one place structural resolution can run. Handlers that parsed for themselves saw the text as
// written, and three were each found reporting a document nobody applies.
// specs/source-resolution.md, specs/language-server.md §4.

import (
	"encoding/json"
	"path/filepath"
	"strings"

	"genroc/internal/defdoc"
	"genroc/internal/model"
	"genroc/internal/numeric"
	"genroc/internal/sources"
)

type document struct {
	// Doc is the text as WRITTEN: spans, addresses, and the value at each. It answers what a
	// cursor is on, where a diagnostic lands, whether a key is already typed -- never what the
	// definition declares, since a key a directive supplies has no node here.
	*defdoc.Doc
	// file is where the text lives on disk, which a directive's relative argument resolves
	// against. "" is a buffer with no file, and is analysed as written.
	file string
	// The resolved value, computed on first use so a handler that reads only shape pays
	// nothing, and reachable only through resolve(): meaning is read from the definition it
	// decodes to, which is the whole rule.
	resolved    any
	resolveErr  error
	resolvedYet bool
}

func parseDocuments(text, file string) ([]*document, error) {
	docs, err := defdoc.ParseAll([]byte(text))
	if err != nil {
		return nil, err
	}
	out := make([]*document, len(docs))
	for i, doc := range docs {
		out[i] = &document{Doc: doc, file: file}
	}
	return out, nil
}

// parseRepaired parses text, retrying with the cursor's line closed off when the document as
// written will not parse. Only the one line is touched: a repair that rewrote more would
// answer about a document the author is not looking at.
func parseRepaired(text, file string, line int) (*document, bool) {
	if doc, ok := soleDocContaining(text, file, line); ok {
		return doc, true
	}
	lines := splitLines(text)
	if line < 1 || line > len(lines) {
		return nil, false
	}
	// A half-typed key (`ur`) is not YAML either, so `: ` is one of the repairs -- the others
	// close a string, an interpolation, or a list the author has not finished. An unclosed `[`
	// swallows every line below it, so without `]` the whole document stops parsing while a
	// `type: [string,` is being written.
	for _, suffix := range []string{`"`, `}"`, `"}`, `: `, `]`} {
		patched := append([]string(nil), lines...)
		patched[line-1] += suffix
		if doc, ok := soleDocContaining(strings.Join(patched, "\n"), file, line); ok {
			return doc, true
		}
	}
	return nil, false
}

func soleDocContaining(text, file string, line int) (*document, bool) {
	docs, err := parseDocuments(text, file)
	if err != nil {
		return nil, false
	}
	for _, doc := range docs {
		if _, ok := doc.At(line, 1); ok {
			return doc, true
		}
	}
	// A cursor on a line the index does not reach -- a blank line inside a mapping -- still
	// belongs to whichever document is the only one there is.
	if len(docs) == 1 {
		return docs[0], true
	}
	return nil, false
}

// resolve runs the structural phase over a DEEP COPY: positions come from Doc's index and an
// injected key has no node there, so the verdict reads the copy and every diagnostic still
// locates against what the author wrote. The error is the verdict's to report; the value is
// the text as written whenever there is one.
func (d *document) resolve() (any, error) {
	if d.resolvedYet {
		return d.resolved, d.resolveErr
	}
	d.resolvedYet = true
	d.resolved = d.Value
	if d.file == "" {
		return d.resolved, nil
	}
	cfg, err := sources.FindProjectConfig(filepath.Dir(d.file))
	if err != nil {
		// A malformed `.genroc` is the project's problem, not this document's, and the file it
		// names is not the one open. Analysing the text as written is the better failure.
		return d.resolved, nil
	}
	docs := []sources.Doc{{Value: deepCopy(d.Value), File: d.file}}
	if _, err := sources.ResolveStructuralPass(docs, cfg, nil); err != nil {
		d.resolveErr = err
		return d.resolved, err
	}
	d.resolved = docs[0].Value
	return d.resolved, nil
}

// definition decodes as far as the document allows: unknown keys are tolerated, and a directive
// that would not resolve is left as written. Hover and completion read here -- a reader asking
// about one slot is not asking about a typo in another (§7b). The verdict is analyse's alone.
func (d *document) definition() (*model.ProcessDefinition, bool) {
	value, _ := d.resolve()
	raw, err := json.Marshal(value)
	if err != nil {
		return nil, false
	}
	var def model.ProcessDefinition
	if err := numeric.Decode(raw, &def); err != nil {
		return nil, false
	}
	return &def, true
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

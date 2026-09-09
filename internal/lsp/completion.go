package lsp

// Completion. Two questions share one entry point because one cursor decides between them:
// inside a `$:` or `${ }` the answer is what the SCOPE holds (validation's context view);
// anywhere else it is which KEYS are legal here (the generated schema). specs/language-server.md §5.

import (
	"strings"

	"genroc/internal/defdoc"
	"genroc/internal/schema"
	"genroc/internal/validation"
)

// completeAt answers for a cursor given as a 1-based line and byte column.
//
// The document is usually mid-edit and often unparseable, so everything here works from the
// raw LINE first and consults the parsed document only to find which slot the line sits in.
func completeAt(text string, line, col int) []completionItem {
	src := lineAt(text, line)
	if expr, ok := expressionPrefix(src, col); ok {
		return completeExpression(text, line, col, expr)
	}
	return completeKey(text, line, col)
}

// completeKey offers the keys legal in the mapping the cursor sits in. A cursor on a
// half-typed key resolves to that key's own node, whose PARENT is the mapping being filled in.
func completeKey(text string, line, col int) []completionItem {
	doc, ok := parseRepaired(text, line)
	if !ok {
		return nil
	}
	if path, ok := doc.At(line, col); ok {
		// A cursor ON a key is someone typing that key, and what they want is its SIBLINGS —
		// the keys legal beside it. Only a cursor in the whitespace of a mapping means
		// "inside".
		span, _ := doc.Span(path)
		if span.Key.Contains(line, col) {
			path = defdoc.ParentPath(path)
		} else if v, found := doc.ValueAt(path); found {
			if _, isMapping := v.(map[string]any); !isMapping {
				path = defdoc.ParentPath(path)
			}
		}
		return legalKeys(doc, path)
	}

	// A blank line below the last key is where the next key goes, and no node covers it. The
	// nearest line above at the same indent names a SIBLING, so the mapping being filled in is
	// that sibling's parent.
	sibling, keyCol, found := sameIndentAbove(text, line, col)
	if !found {
		return nil
	}
	path, ok := doc.At(sibling, keyCol)
	if !ok {
		return nil
	}
	return legalKeys(doc, defdoc.ParentPath(path))
}

// expressionPrefix reports the dotted path being typed, when the cursor is inside an
// expression. The scan is over raw text: a half-typed `$: order.` is not valid YAML, let alone
// a valid expression, and waiting for it to become one is waiting until the author no longer
// needs help.
func expressionPrefix(src string, col int) (string, bool) {
	i := col - 1
	if i < 0 || i > len(src) {
		return "", false
	}
	head := src[:i]
	open := strings.LastIndex(head, "${")
	marker := strings.LastIndex(head, "$:")
	switch {
	case open > marker:
		if strings.Contains(head[open:], "}") {
			return "", false // the interpolation closed before the cursor
		}
	case marker >= 0:
		// A `$:` leaf runs to the end of the scalar, so there is nothing to close.
	default:
		return "", false
	}
	return dottedTail(head), true
}

// dottedTail is the identifier path immediately left of the cursor: `f(order.li` yields
// `order.li`, which splits below into what to navigate and what the client filters on.
func dottedTail(head string) string {
	i := len(head)
	for i > 0 {
		c := head[i-1]
		if c == '.' || c == '_' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' {
			i--
			continue
		}
		break
	}
	return head[i:]
}

func completeExpression(text string, line, col int, prefix string) []completionItem {
	ctx, ok := scopeAt(text, line, col)
	if !ok {
		return nil
	}
	// The client filters on the last segment, so navigate to the container and offer all of
	// it. Offering a pre-filtered list would fight the editor's own matching.
	container := ""
	if i := strings.LastIndexByte(prefix, '.'); i >= 0 {
		container = prefix[:i]
	}
	if container != "" {
		narrowed, err := ctx.At(container)
		if err != nil {
			return nil
		}
		ctx = narrowed
	}
	return membersOf(ctx)
}

func membersOf(s schema.Schema) []completionItem {
	if resolved, err := s.Resolve(); err == nil {
		s = resolved
	}
	props := s.Properties()
	if len(props) == 0 {
		return nil
	}
	required := map[string]bool{}
	for _, name := range s.Required() {
		required[name] = true
	}
	out := make([]completionItem, 0, len(props))
	for name, sub := range props {
		detail := sub.Summary()
		if !required[name] {
			detail += " (may be absent)"
		}
		out = append(out, completionItem{
			Label:         name,
			Kind:          kindField,
			Detail:        detail,
			Documentation: sub.Description(),
		})
	}
	return out
}

// scopeAt finds the expression context governing a cursor. The document is parsed with the
// cursor's line REPAIRED when it will not parse on its own — a mid-typed expression usually
// leaves an unterminated quote, and refusing to answer until it is closed refuses exactly when
// the author is asking.
func scopeAt(text string, line, col int) (schema.Schema, bool) {
	doc, ok := parseRepaired(text, line)
	if !ok {
		return schema.Schema{}, false
	}
	def, ok := definitionOf(doc)
	if !ok {
		return schema.Schema{}, false
	}
	contexts, err := validation.SlotContexts(def)
	if err != nil {
		return schema.Schema{}, false
	}
	path, ok := doc.At(line, col)
	if !ok {
		return schema.Schema{}, false
	}
	_, ctx, found := enclosingSlot(contexts, path)
	return ctx, found
}

// parseRepaired parses text, retrying with the cursor's line closed off when the document as
// written will not parse. Only the one line is touched: a repair that rewrote more would
// answer about a document the author is not looking at.
func parseRepaired(text string, line int) (*defdoc.Doc, bool) {
	if doc, ok := soleDocContaining(text, line); ok {
		return doc, true
	}
	lines := splitLines(text)
	if line < 1 || line > len(lines) {
		return nil, false
	}
	// A half-typed key (`ur`) is not YAML either, so `: ` is one of the repairs — the others
	// close a string or an interpolation the author has not finished.
	for _, suffix := range []string{`"`, `}"`, `"}`, `: `} {
		patched := append([]string(nil), lines...)
		patched[line-1] += suffix
		if doc, ok := soleDocContaining(strings.Join(patched, "\n"), line); ok {
			return doc, true
		}
	}
	return nil, false
}

func soleDocContaining(text string, line int) (*defdoc.Doc, bool) {
	docs, err := defdoc.ParseAll([]byte(text))
	if err != nil {
		return nil, false
	}
	for _, doc := range docs {
		if _, ok := doc.At(line, 1); ok {
			return doc, true
		}
	}
	// A cursor on a line the index does not reach — a blank line inside a mapping — still
	// belongs to whichever document is the only one there is.
	if len(docs) == 1 {
		return docs[0], true
	}
	return nil, false
}

// sameIndentAbove finds the nearest non-blank line above `line` whose first content sits at
// the cursor's own column, and returns where that content starts. A sequence entry counts by
// its first key (`- id: x` puts `id` at the indent its siblings use), which is why the search
// is over the column rather than over the leading dash.
func sameIndentAbove(text string, line, col int) (int, int, bool) {
	lines := splitLines(text)
	for i := line - 2; i >= 0; i-- {
		if i >= len(lines) {
			continue
		}
		start := indentOf(lines[i])
		if start < 0 {
			continue // blank
		}
		if start+1 == col {
			return i + 1, col, true
		}
		if start+1 < col {
			return 0, 0, false // an outer level: the cursor is deeper than anything above it
		}
	}
	return 0, 0, false
}

// indentOf is the 0-based column of a line's first content, or -1 when it has none. A sequence
// dash is skipped: `  - id: x` has its key at 4, the indent its siblings are written at.
func indentOf(line string) int {
	i := 0
	for i < len(line) && line[i] == ' ' {
		i++
	}
	if i < len(line) && line[i] == '-' {
		i++
		for i < len(line) && line[i] == ' ' {
			i++
		}
	}
	if i >= len(line) {
		return -1
	}
	return i
}

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
	// A `case` holds an expression written BARE, so there is no `$:` for the scan above to
	// find and the cursor would otherwise be read as sitting on a key.
	if inBareExpression(text, line, col) {
		return completeExpression(text, line, col, dottedTail(src[:min(col-1, len(src))]))
	}
	return completeKey(text, line, col)
}

// inBareExpression reports whether the cursor is inside the VALUE of a slot that holds an
// expression with no `$:` marker.
func inBareExpression(text string, line, col int) bool {
	doc, ok := parseRepaired(text, line)
	if !ok {
		return false
	}
	path, ok := doc.At(line, col)
	if !ok || !isBareExpression(path) {
		return false
	}
	span, ok := doc.Span(path)
	return ok && span.Value.Contains(line, col)
}

// completeKey offers the keys legal in the mapping the cursor sits in. A cursor on a
// half-typed key resolves to that key's own node, whose PARENT is the mapping being filled in.
func completeKey(text string, line, col int) []completionItem {
	doc, ok := parseRepaired(text, line)
	if !ok {
		return nil
	}
	// On a line with nothing before the cursor, INDENTATION decides. `At` would answer with
	// whichever container happens to span the line — the outermost one, not the mapping being
	// filled in — because an empty line is inside every ancestor at once.
	if blankBefore(text, line, col) {
		anchorLine, anchorCol, sibling, found := keyAbove(text, line, col)
		if !found {
			// Nothing above sits at or outside this indent, so the cursor is at the top
			// level of the document.
			return legalKeys(doc, "")
		}
		path, ok := doc.At(anchorLine, anchorCol)
		if !ok {
			return nil
		}
		if sibling {
			path = defdoc.ParentPath(path)
		}
		return legalKeys(doc, path)
	}

	path, ok := doc.At(line, col)
	if !ok {
		return nil
	}
	// A cursor ON a key is someone typing that key, and what they want is its SIBLINGS — the
	// keys legal beside it. Only a cursor in the whitespace of a mapping means "inside".
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

// blankBefore reports whether only whitespace precedes the cursor on its line.
func blankBefore(text string, line, col int) bool {
	src := lineAt(text, line)
	if col-1 > len(src) {
		return strings.TrimSpace(src) == ""
	}
	return strings.TrimSpace(src[:col-1]) == ""
}

// keyAbove finds what the cursor's indentation puts it under. The first line above at the SAME
// indent is a sibling, so the mapping being filled in is that sibling's parent; the first at a
// SMALLER indent is the key whose value the cursor is inside.
func keyAbove(text string, line, col int) (int, int, bool, bool) {
	lines := splitLines(text)
	for i := line - 2; i >= 0; i-- {
		if i >= len(lines) {
			continue
		}
		start := indentOf(lines[i])
		if start < 0 {
			continue // blank
		}
		switch {
		case start+1 == col:
			return i + 1, col, true, true
		case start+1 < col:
			return i + 1, start + 1, false, true
		}
	}
	return 0, 0, false, false
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
	// A value that may be absent is `anyOf[$ref, null]`, and the null arm blocks the $ref
	// beside it from resolving — so an optional object offered no members at all. Same shape,
	// same fix as schema.Summary.
	if s.HasNull() {
		if inner := s.StripNull(); !inner.IsZero() && !inner.IsNull() {
			s = inner
		}
	}
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
	path, ok := doc.At(line, col)
	if !ok {
		return schema.Schema{}, false
	}
	// The scope is the document WITHOUT the expression being written. A half-typed leaf does
	// not type, its slot recovers as {}, and everything that reads the slot — `self.previous`
	// most of all — then offers nothing, exactly where help was asked for.
	source := doc
	if blanked, ok := parseRepaired(blankValueAt(text, line), line); ok {
		source = blanked
	}
	def, ok := definitionOf(source)
	if !ok {
		return schema.Schema{}, false
	}
	contexts, err := validation.SlotContexts(def)
	if err != nil {
		return schema.Schema{}, false
	}
	_, ctx, found := enclosingSlot(contexts, path)
	return ctx, found
}

// blankValueAt empties the value on one line, keeping its key so the document still has the
// same shape — and the same paths — as the one the cursor was resolved against.
func blankValueAt(text string, line int) string {
	lines := splitLines(text)
	if line < 1 || line > len(lines) {
		return text
	}
	src := lines[line-1]
	colon := strings.Index(src, ": ")
	if colon < 0 {
		return text
	}
	lines[line-1] = src[:colon+2] + `""`
	return strings.Join(lines, "\n")
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

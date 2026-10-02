package lsp

// Hover answers the two questions a reader has over a definition: what type is this slot, and
// — over an expression — what does this one produce. Both come from `genctl schema`'s own
// views, so the editor and the command cannot disagree. specs/schema-command.md.

import (
	"regexp"
	"strings"

	"genroc/internal/defdoc"
	"genroc/internal/model"
	"genroc/internal/schema"
	"genroc/internal/validation"
)

// hoverAt returns the markdown for a cursor, and the range it describes. An empty string means
// there is nothing to say, which is the common answer and must not become a popup.
func hoverAt(text, file string, line, col int) (string, defdoc.Range, bool) {
	// A comment is not a slot: the cursor would resolve to the mapping around it and describe
	// the line above.
	if at := commentColumn(lineAt(text, line)); at >= 0 && col > at {
		return "", defdoc.Range{}, false
	}
	// Off the raw line, before the index: a `<<` defdoc merges has no node of its own, so the
	// cursor would resolve to the mapping around it.
	if r, ok := mergeKeyUnder(lineAt(text, line), line, col); ok {
		return mergeKeyHover, r, true
	}
	docs, err := parseDocuments(text, file)
	if err != nil {
		return "", defdoc.Range{}, false
	}
	for _, d := range docs {
		path, ok := d.At(line, col)
		if !ok {
			continue
		}
		span, _ := d.Span(path)
		// A directive is not a slot: what it YIELDS is the answer, and that is a structure.
		if md := directiveHover(d, path); md != "" {
			return md, span.Value, true
		}
		def, ok := d.definition()
		if !ok {
			return "", defdoc.Range{}, false
		}
		if md := describe(d.Doc, def, path, lineAt(text, line), line, col); md != "" {
			return md, span.Value, true
		}
		return "", defdoc.Range{}, false
	}
	return "", defdoc.Range{}, false
}

// describe answers with ONE line: the type of the thing under the cursor. A slot's scope is
// `genctl schema context`'s question, not hover's.
func describe(doc *defdoc.Doc, def *model.ProcessDefinition, path, src string, line, col int) string {
	contexts, err := validation.SlotContexts(def)
	if err != nil {
		return ""
	}
	types, _ := validation.TypeSlots(def)

	// A cursor on a KEY inside a shape asks what the key HOLDS, not what its expression evaluates to.
	if md := shapeKeyHover(doc, types, path, line, col); md != "" {
		return md
	}

	_, ctx, found := enclosingSlot(contexts, path)
	if !found {
		return firstOf(typeOnly(types, path), describeKey(doc, path))
	}

	expr, ok := expressionAt(doc, path)
	if !ok {
		// A `${ }` inside a longer string types as that string; the interpolation the cursor is
		// IN has a type of its own.
		expr, ok = interpolationUnder(src, col)
	}
	if !ok {
		return firstOf(typeOnly(types, path), describeKey(doc, path))
	}
	// The member path under the cursor (`count` in `self.previous.count ?? 0` asks about
	// `self.previous.count`), only when it types: the scan also picks up words inside strings.
	if symbol, found := symbolUnder(src, col); found && symbol != expr {
		// A lambda parameter is bound by the EXPRESSION, not by the slot, so the scope for
		// this one lookup carries what `map` binds. The whole expression binds its own.
		if t, err := ctx.WithVars(ctx.LambdaVars(expr)).Infer(symbol); err == nil {
			return "`" + symbol + "` → " + mdType(t.Summary())
		}
	}
	return typed(ctx, expr)
}

// typeOnly names a slot and its type, unless that type is `unknown` — a slot nothing narrows
// says nothing, and the key's own description is the better answer there.
func typeOnly(types map[string]schema.Schema, path string) string {
	t, ok := types[path]
	if !ok {
		return ""
	}
	summary := t.Summary()
	if summary == "unknown" {
		return ""
	}
	return "**" + path + "** — " + mdType(summary)
}

// mdType puts a type in a code span: as markdown, `array<string>` holds an unknown HTML tag the
// renderer drops.
func mdType(summary string) string { return "`" + summary + "`" }

// enclosingSlot walks up from a path to the slot whose context governs it: an expression in
// `tasks.a.action.url` is written in `tasks.a.action`'s scope.
func enclosingSlot(contexts map[string]schema.Schema, path string) (string, schema.Schema, bool) {
	for p := path; p != ""; p = defdoc.ParentPath(p) {
		if ctx, ok := contexts[p]; ok {
			return p, ctx, true
		}
	}
	return "", schema.Schema{}, false
}

// expressionAt returns the bare expression at a path — the `$:` leaf or the sole `${ }` in a
// template — since that is what Infer takes. A literal, or a template with text around it,
// types as the string it is and says nothing worth a popup.
func expressionAt(doc *defdoc.Doc, path string) (string, bool) {
	v, ok := doc.ValueAt(path)
	if !ok {
		return "", false
	}
	s, ok := v.(string)
	if !ok {
		return "", false
	}
	trimmed := strings.TrimSpace(s)
	// A `case` is written BARE — it is an expression slot, not a Shape, so it carries no `$:`
	// and hover would otherwise answer with what the key means instead of what it evaluates to.
	if isBareExpression(path) {
		return trimmed, trimmed != ""
	}
	if inner, ok := strings.CutPrefix(trimmed, "$:"); ok {
		return strings.TrimSpace(inner), true
	}
	if body, ok := strings.CutPrefix(trimmed, "${"); ok {
		if inner, closed := strings.CutSuffix(body, "}"); closed && !strings.Contains(inner, "${") {
			return strings.TrimSpace(inner), true
		}
	}
	return "", false
}

// interpolationUnder returns the `${ … }` body the column sits inside, read off the RAW line:
// mapping a column through YAML's escaping would be a second grammar to keep true.
func interpolationUnder(line string, col int) (string, bool) {
	i := col - 1
	if i < 0 || i > len(line) {
		return "", false
	}
	open := strings.LastIndex(line[:min(i+1, len(line))], "${")
	if open < 0 {
		return "", false
	}
	close := strings.Index(line[open:], "}")
	if close < 0 || open+close < i {
		return "", false
	}
	inner := strings.TrimSpace(line[open+2 : open+close])
	if inner == "" || strings.Contains(inner, "${") {
		return "", false
	}
	return inner, true
}

// mergeKeyHover is what `<<` means, in one line. It is prose the schema cannot carry: defdoc
// consumes the key before the server sees a document, so no struct field describes it.
const mergeKeyHover = "Merges a mapping into this one: an anchored mapping (`<<: *base`), a nested one, " +
	"or what a `$<resolver>:` directive answers (`$process` spreads a child's types). " +
	"A key written beside it wins."

var mergeKeyRe = regexp.MustCompile(`^\s*(?:-\s+)?(<<)\s*:`)

// mergeKeyUnder reports whether the cursor is on a `<<` key, and that key's range.
func mergeKeyUnder(src string, line, col int) (defdoc.Range, bool) {
	m := mergeKeyRe.FindStringSubmatchIndex(src)
	if m == nil {
		return defdoc.Range{}, false
	}
	start, end := m[2], m[3]
	if col-1 < start || col-1 > end {
		return defdoc.Range{}, false
	}
	return defdoc.Range{Line: line, Col: start + 1, EndLine: line, EndCol: end + 1}, true
}

// commentColumn is where a comment opens on line, or -1. A `#` inside a quoted scalar is not
// one -- a URL fragment would lose its hover -- and neither is one without a space in front,
// which is YAML's own rule: `a#b` is a plain scalar.
func commentColumn(line string) int {
	var quote byte
	for i := 0; i < len(line); i++ {
		c := line[i]
		switch {
		case quote != 0:
			if c == '\\' && quote == '"' {
				i++
			} else if c == quote {
				quote = 0
			}
		case c == '\'' || c == '"':
			quote = c
		case c == '#' && (i == 0 || line[i-1] == ' ' || line[i-1] == '\t'):
			return i
		}
	}
	return -1
}

// lineAt returns one 1-based line of text, or "" past the end.
func lineAt(text string, line int) string {
	lines := strings.Split(text, "\n")
	if line < 1 || line > len(lines) {
		return ""
	}
	return lines[line-1]
}

// typed answers with the expression's type, and NOTHING when it has none: the diagnostic for
// that position already heads the same popup.
func typed(ctx schema.Schema, expr string) string {
	t, err := ctx.Infer(expr)
	if err != nil {
		return ""
	}
	return "`" + expr + "` → " + mdType(t.Summary())
}

// symbolUnder returns the member path the cursor is on, truncated AT its segment. It scans raw
// text because the expression AST carries no offsets (specs/language-server.md §6), so an
// indexed path like `a[0].b` falls back to the whole expression.
func symbolUnder(line string, col int) (string, bool) {
	i := col - 1
	if i < 0 || i > len(line) {
		return "", false
	}
	start := i
	for start > 0 && isPathByte(line[start-1]) {
		start--
	}
	end := i
	for end < len(line) && isIdentByte(line[end]) {
		end++
	}
	symbol := strings.Trim(line[start:end], ".")
	// A member path never starts with a digit, so this drops the numeric literals an operator
	// sits between — `0` types fine and says nothing anyone hovered to find out.
	if symbol == "" || !isNameStart(symbol[0]) {
		return "", false
	}
	return symbol, true
}

func isPathByte(c byte) bool { return c == '.' || isIdentByte(c) }

func isIdentByte(c byte) bool { return isNameStart(c) || c >= '0' && c <= '9' }

func isNameStart(c byte) bool {
	return c == '_' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z'
}

// firstOf is the answer order: a type where there is one, else what the key means.
func firstOf(answers ...string) string {
	for _, a := range answers {
		if a != "" {
			return a
		}
	}
	return ""
}

// isBareExpression reports whether a slot holds an expression written without `$:`. Only a
// `case` does — in a switch clause or an on_error rule. specs/task-scopes.md.
func isBareExpression(path string) bool {
	seg := strings.Split(path, ".")
	return len(seg) == 5 && seg[0] == "tasks" && seg[4] == "case" &&
		(seg[2] == "switch" || seg[2] == "on_error")
}

package lsp

// Hover answers the two questions a reader has over a definition: what type is this slot, and
// — over an expression — what does this one produce. Both come from `genctl schema`'s own
// views, so the editor and the command cannot disagree. specs/schema-command.md.

import (
	"strings"

	"genroc/internal/defdoc"
	"genroc/internal/model"
	"genroc/internal/schema"
	"genroc/internal/validation"
)

// hoverAt returns the markdown for a cursor, and the range it describes. An empty string means
// there is nothing to say, which is the common answer and must not become a popup.
func hoverAt(text string, line, col int) (string, defdoc.Range, bool) {
	docs, err := defdoc.ParseAll([]byte(text))
	if err != nil {
		return "", defdoc.Range{}, false
	}
	for _, doc := range docs {
		path, ok := doc.At(line, col)
		if !ok {
			continue
		}
		def, ok := definitionOf(doc)
		if !ok {
			return "", defdoc.Range{}, false
		}
		span, _ := doc.Span(path)
		if md := describe(doc, def, path, lineAt(text, line), col); md != "" {
			return md, span.Value, true
		}
		return "", defdoc.Range{}, false
	}
	return "", defdoc.Range{}, false
}

// definitionOf decodes a document as far as it goes. Unknown keys are TOLERATED here, unlike in
// the diagnostics path: a reader hovering one slot is not asking about a typo in another, and
// refusing to answer until the whole file is clean is the behaviour §7b was written against.
func definitionOf(doc *defdoc.Doc) (*model.ProcessDefinition, bool) {
	raw, err := marshal(doc.Value)
	if err != nil {
		return nil, false
	}
	var def model.ProcessDefinition
	if err := decodeLenient(raw, &def); err != nil {
		return nil, false
	}
	return &def, true
}

// describe answers with ONE line: the type of the thing under the cursor. A hover is read at a
// glance, and the scope a slot carries is a different question — `genctl schema context` is
// where that one is asked.
func describe(doc *defdoc.Doc, def *model.ProcessDefinition, path, line string, col int) string {
	contexts, err := validation.SlotContexts(def)
	if err != nil {
		return ""
	}
	types, _ := validation.TypeSlots(def)

	_, ctx, found := enclosingSlot(contexts, path)
	if !found {
		return firstOf(typeOnly(types, path), describeKey(doc, path))
	}

	expr, ok := expressionAt(doc, path)
	if !ok {
		// A `${ }` inside a longer string types as the string it renders into, so the whole
		// leaf says nothing — but the interpolation the cursor is IN has a type of its own,
		// and that is the one being written.
		expr, ok = interpolationUnder(line, col)
	}
	if !ok {
		return firstOf(typeOnly(types, path), describeKey(doc, path))
	}
	// The symbol the cursor is actually on, when it is a member path and not the whole
	// expression: pointing at `count` in `(self.previous.count ?? 0) + 1` asks about
	// `self.previous.count`. Only when it types -- the scan cannot tell a member path from a
	// word inside a string literal, and an error would replace the answer the reader came for.
	if symbol, found := symbolUnder(line, col); found && symbol != expr {
		if t, err := ctx.Infer(symbol); err == nil {
			return "`" + symbol + "` → **" + t.Summary() + "**"
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
	return "**" + path + "** — " + summary
}

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

// interpolationUnder returns the `${ … }` body the column sits inside, from the raw source
// line. The line is used rather than the decoded scalar because a column is what the protocol
// hands over, and mapping it back through YAML's own escaping would be a second grammar.
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

// lineAt returns one 1-based line of text, or "" past the end.
func lineAt(text string, line int) string {
	lines := strings.Split(text, "\n")
	if line < 1 || line > len(lines) {
		return ""
	}
	return lines[line-1]
}

// typed answers with the expression's type, and with NOTHING when it has none: the editor
// already shows the diagnostic for that position at the top of the same popup, and saying it
// twice is what a reader sees.
func typed(ctx schema.Schema, expr string) string {
	t, err := ctx.Infer(expr)
	if err != nil {
		return ""
	}
	return "`" + expr + "` → **" + t.Summary() + "**"
}

// symbolUnder returns the member path the cursor is on, truncated AT the segment it is in:
// `previous` in `self.previous.count` answers `self.previous`, so walking a path shows each
// level's own type. The scan is over raw text because the expression AST carries no offsets
// (specs/language-server.md §6) — an indexed path like `a[0].b` is not spelled here and falls
// back to the whole expression.
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

// firstOf is the answer order: a type where there is one, else what the key means. Without the
// second, hover was silent on most of a file — every key, every literal — because dropping the
// scope line took the only thing it had to say there.
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

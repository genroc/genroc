package lsp

// Hover answers the two questions a reader has over a definition: what type is this slot, and
// — over an expression — what does this one produce. Both come from `genctl schema`'s own
// views, so the editor and the command cannot disagree. specs/schema-command.md.

import (
	"fmt"
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

func describe(doc *defdoc.Doc, def *model.ProcessDefinition, path, line string, col int) string {
	contexts, err := validation.SlotContexts(def)
	if err != nil {
		return ""
	}
	types, _ := validation.TypeSlots(def)

	slot, ctx, found := enclosingSlot(contexts, path)
	if !found {
		return typeOnly(types, path)
	}

	var out []string
	// An expression is the interesting case: its type is what the author is guessing at.
	expr, ok := expressionAt(doc, path)
	if !ok {
		// A `${ }` inside a longer string types as the string it renders into, so the whole
		// leaf says nothing — but the interpolation the cursor is IN has a type of its own,
		// and that is the one being written.
		expr, ok = interpolationUnder(line, col)
	}
	if ok {
		if t, err := ctx.Infer(expr); err == nil {
			out = append(out, "`"+expr+"` → **"+t.Summary()+"**")
		} else {
			out = append(out, "`"+expr+"` → _"+err.Error()+"_")
		}
	}
	if t, ok := types[path]; ok {
		out = append(out, "**"+path+"** — "+t.Summary())
	}
	out = append(out, fmt.Sprintf("_in scope at `%s`_: %s", slot, ctx.MemberNames()))
	return strings.Join(out, "\n\n")
}

func typeOnly(types map[string]schema.Schema, path string) string {
	t, ok := types[path]
	if !ok {
		return ""
	}
	return "**" + path + "** — " + t.Summary()
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

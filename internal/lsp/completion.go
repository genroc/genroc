package lsp

// Completion. Two questions share one entry point because one cursor decides between them:
// inside a `$:` or `${ }` the answer is what the SCOPE holds (validation's context view);
// anywhere else it is which KEYS are legal here (the generated schema). specs/language-server.md §5.

import (
	"fmt"
	"slices"
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
	if refs, ok := routingValues(text, line, col); ok {
		return refs
	}
	if types, ok := typeValues(text, line, col); ok {
		return types
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
	src := lineAt(text, line)
	// On a line with nothing before the cursor, INDENTATION decides. `At` would answer with
	// whichever container happens to span the line — the outermost one, not the mapping being
	// filled in — because an empty line is inside every ancestor at once.
	if blankBefore(text, line, col) {
		// The cursor is in the indent, or on a list dash, of a line that HAS content: it is
		// beside that line's key, so that key's mapping is the answer — including which of its
		// keys are already written.
		if strings.TrimSpace(src) != "" {
			if path, ok := mappingUnder(doc, line, indentOf(src)+1); ok {
				return keyEdits(legalKeys(doc, path), src, col)
			}
		}
		anchorLine, anchorCol, sibling, found := keyAbove(text, line, col)
		if !found {
			// Nothing above sits at or outside this indent, so the cursor is at the top
			// level of the document.
			return keyEdits(legalKeys(doc, ""), src, col)
		}
		path, ok := doc.At(anchorLine, anchorCol)
		if !ok {
			return nil
		}
		if sibling {
			path = defdoc.ParentPath(path)
		}
		return keyEdits(legalKeys(doc, path), src, col)
	}

	// A line with content: the cursor may be past its end, or in the gap a `- ` leaves, where
	// nothing covers it but the sequence or the document itself. The line's own key is what it
	// sits beside, so resolve from there instead of falling out to the root.
	path, ok := mappingUnder(doc, line, col)
	if !ok {
		if start := indentOf(src) + 1; start != col {
			path, ok = mappingUnder(doc, line, start)
		}
	}
	if !ok {
		return nil
	}
	return keyEdits(legalKeys(doc, path), src, col)
}

// keyEdits gives each key the range it replaces: the whole word the cursor is in, which is
// wider than the cursor itself — a key chosen from inside one must swallow the rest of it.
//
// The colon the item carries is written only where the line ENDS at that word. Anywhere else
// something is already there — the key's own `: value`, or the content of the line the cursor
// is merely indented on — and a second colon would break it.
func keyEdits(items []completionItem, src string, col int) []completionItem {
	from, to := wordAround(src, col)
	writesColon := strings.TrimSpace(src[to-1:]) == ""
	for i := range items {
		items[i].replaceFrom, items[i].replaceTo = from, to
		if !writesColon {
			items[i].insert = ""
		}
	}
	return items
}

// wordAround is the 1-based column span of the key the cursor is in, empty at the cursor where
// it is in none. `$` is a key byte: `$ref` and `$defs` are keys a schema author writes.
func wordAround(src string, col int) (from, to int) {
	i := col - 1
	if i > len(src) {
		i = len(src)
	}
	from, to = i, i
	for from > 0 && isKeyByte(src[from-1]) {
		from--
	}
	for to < len(src) && isKeyByte(src[to]) {
		to++
	}
	return from + 1, to + 1
}

func isKeyByte(c byte) bool { return c == '$' || isIdentByte(c) }

// mappingUnder resolves a position to the mapping whose keys belong there, or reports that
// none does. A cursor ON a key is someone typing that key, and what they want is its SIBLINGS;
// only a cursor in the whitespace of a mapping means "inside".
func mappingUnder(doc *defdoc.Doc, line, col int) (string, bool) {
	path, ok := doc.At(line, col)
	if !ok || path == "" {
		return "", false
	}
	span, _ := doc.Span(path)
	if span.Key.Contains(line, col) {
		return defdoc.ParentPath(path), true
	}
	v, found := doc.ValueAt(path)
	if !found {
		return "", false
	}
	switch v.(type) {
	case map[string]any:
		return path, true
	case []any:
		// A sequence holds no keys of its own: the cursor is between its elements.
		return "", false
	}
	return defdoc.ParentPath(path), true
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
		if c == ']' {
			// An index is part of the path — `rows[0].id` reads the element's own field — and
			// stepping over the whole group is what keeps a `-` or a quote inside it from
			// ending the scan. Unbalanced, there is no path to navigate.
			j := openingBracket(head, i)
			if j < 0 {
				break
			}
			i = j
			continue
		}
		if c == '.' || c == '_' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' {
			i--
			continue
		}
		break
	}
	return head[i:]
}

// openingBracket is the index of the `[` matching the `]` that ends head[:end], or -1.
func openingBracket(head string, end int) int {
	depth := 0
	for i := end; i > 0; i-- {
		switch head[i-1] {
		case ']':
			depth++
		case '[':
			depth--
			if depth == 0 {
				return i - 1
			}
		}
	}
	return -1
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
	if members := membersOf(ctx); len(members) > 0 {
		return members
	}
	return indexOf(ctx, prefix, col)
}

// indexOf answers for an ARRAY, which has no members to name: it is indexed. The `.` the reader
// has just typed is not legal after one, so the item REPLACES it — `input.who.` becomes
// `input.who[0]` rather than `input.who.[0]`.
func indexOf(s schema.Schema, prefix string, col int) []completionItem {
	dot := strings.LastIndexByte(prefix, '.')
	if dot < 0 {
		return nil
	}
	element, err := s.Index()
	if err != nil {
		return nil
	}
	return []completionItem{{
		Label:       "[0]",
		Kind:        kindField,
		Detail:      element.Summary(),
		replaceFrom: col - len(prefix) + dot,
	}}
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
	out := make([]completionItem, 0, len(props))
	for name, sub := range props {
		detail := sub.Summary()
		if s.MayBeAbsent(name) {
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
	// close a string, an interpolation, or a list the author has not finished. An unclosed `[`
	// swallows every line below it, so without `]` the whole document stops parsing while a
	// `type: [string,` is being written.
	for _, suffix := range []string{`"`, `}"`, `"}`, `: `, `]`} {
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

// routingValues offers what a `goto` may name: every task in this document, plus the two words
// that are not tasks. A routing slot is the one place a VALUE has a closed set, and without
// this the cursor reads as sitting on a key and the clause's own siblings are offered instead.
func routingValues(text string, line, col int) ([]completionItem, bool) {
	src := lineAt(text, line)
	doc, ok := parseRepaired(text, line)
	if !ok {
		return nil, false
	}
	if _, ok := valueSlot(doc, src, line, col, usableRouting); !ok {
		return nil, false
	}
	// The token already typed is REPLACED, not appended to: `$` is not a word character, so an
	// editor left with no range would insert `$tick` beside the `$` the reader just typed.
	from := replaceFrom(src, col)
	out := []completionItem{
		{Label: "end", Kind: kindValue, Detail: "terminate the instance", replaceFrom: from},
		{Label: "next", Kind: kindValue, Detail: "advance to the next task in the list", replaceFrom: from},
	}
	for _, id := range taskIDs(doc) {
		out = append(out, completionItem{
			Label: "$" + id, Kind: kindValue, Detail: "task", replaceFrom: from,
		})
	}
	return out, true
}

// valueSlot resolves the cursor to a slot whose VALUE it is inside, trying the position itself
// and then the line's own key. Both are needed: a key with nothing after it has a zero-width
// value node, so the cursor past it resolves to whatever encloses it instead.
func valueSlot(doc *defdoc.Doc, src string, line, col int, usable func(*defdoc.Doc, string) bool) (string, bool) {
	if path, ok := doc.At(line, col); ok && usable(doc, path) {
		if span, found := doc.Span(path); found && span.Value.Contains(line, col) {
			return path, true
		}
	}
	if !afterKey(src, col) && !insideFlowList(src, col) {
		return "", false
	}
	path, ok := doc.At(line, indentOf(src)+1)
	if !ok || !usable(doc, path) {
		return "", false
	}
	return path, true
}

// usableRouting reports whether a slot names a task. A `switch` written as a LIST holds routing
// clauses, and a cursor among them is writing a clause's keys.
func usableRouting(doc *defdoc.Doc, path string) bool {
	if !isRoutingSlot(path) {
		return false
	}
	switch v, _ := doc.ValueAt(path); v.(type) {
	case []any, map[string]any:
		return false
	}
	return true
}

// afterKey reports whether the cursor sits past `key:` on a line with no value yet — where the
// value's own node has no extent to contain anything.
func afterKey(src string, col int) bool {
	i := strings.Index(src, ":")
	return i >= 0 && col > i+1 && strings.TrimSpace(src[i+1:]) == ""
}

// insideFlowList reports whether the cursor is between the brackets of a list written after a
// key on this line — `type: [string, |]`. yaml.v3 gives an empty flow list a ZERO-WIDTH span
// and a partly written one no element to stand in, so neither position resolves through the
// index; the slot is the line's key either way.
func insideFlowList(src string, col int) bool {
	i := strings.Index(src, ":")
	if i < 0 {
		return false
	}
	depth := 0
	for j := i + 1; j < col-1 && j < len(src); j++ {
		switch src[j] {
		case '[':
			depth++
		case ']':
			depth--
		}
	}
	return depth > 0
}

// replaceFrom is the 1-based column the completion should overwrite from: the start of the
// token being typed, `$` included.
func replaceFrom(src string, col int) int {
	start := col - 1
	if start > len(src) {
		start = len(src)
	}
	for start > 0 && isRoutingToken(src[start-1]) {
		start--
	}
	return start + 1
}

func isRoutingToken(c byte) bool { return c == '$' || c == '-' || isIdentByte(c) }

func taskIDs(doc *defdoc.Doc) []string {
	tasks, _ := doc.ValueAt("tasks")
	list, _ := tasks.([]any)
	out := make([]string, 0, len(list))
	for _, t := range list {
		m, _ := t.(map[string]any)
		if id, ok := m["id"].(string); ok && id != "" {
			out = append(out, id)
		}
	}
	return out
}

// typeValues offers what a `type` may name. There are two closed sets and the schema says
// which: a user schema's JSON types, and — where the node is a discriminated union — the
// variants its arms declare. Without this the cursor after `type:` reads as sitting on a key
// and the surrounding keys come back.
func typeValues(text string, line, col int) ([]completionItem, bool) {
	src := lineAt(text, line)
	doc, ok := parseRepaired(text, line)
	if !ok {
		return nil, false
	}
	path, ok := valueSlot(doc, src, line, col, isTypeSlot)
	if !ok {
		return nil, false
	}
	names, detail, alsoAList := typeNamesAt(doc, path)
	if len(names) == 0 {
		return nil, false
	}
	from := replaceFrom(src, col)
	out := make([]completionItem, 0, len(names)+1)
	for i, name := range names {
		out = append(out, completionItem{
			Label:       name,
			Kind:        kindValue,
			Detail:      detail[i],
			SortText:    fmt.Sprintf("%03d", i),
			insert:      typeLiteral(name),
			replaceFrom: from,
		})
	}
	// The list form is the SECOND thing `type` takes, and it is how a nullable property is
	// declared. Offered last, and not once the cursor is already inside one — the names are
	// what goes there, and a second `[]` would nest.
	if v, _ := doc.ValueAt(path); alsoAList && !isList(v) {
		out = append(out, completionItem{
			Label:       "[]",
			Kind:        kindValue,
			Detail:      `several types, e.g. [string, "null"]`,
			SortText:    fmt.Sprintf("%03d", len(names)),
			replaceFrom: from,
		})
	}
	return out, true
}

func isTypeSlot(doc *defdoc.Doc, path string) bool {
	if lastSegment(path) != "type" {
		return false
	}
	// A list is a `type` mid-edit as much as a scalar is: the cursor inside one reaches the slot
	// through the line's key (insideFlowList), not through the element it is standing in.
	if v, found := doc.ValueAt(path); found {
		switch v.(type) {
		case string, []any, nil:
		default:
			return false
		}
	}
	names, _, _ := typeNamesAt(doc, path)
	return len(names) > 0
}

func isList(v any) bool {
	_, ok := v.([]any)
	return ok
}

// typeNamesAt reads the closed set a `type` at path may take, with what each one is, and
// whether the slot also takes a LIST of them — which is a user schema's `type` and never a
// discriminated union's, where two variants at once is not a thing.
func typeNamesAt(doc *defdoc.Doc, path string) (names, detail []string, alsoAList bool) {
	root, ok := processSchema()
	if !ok {
		return nil, nil, false
	}
	node, ok := walk(root, doc, defdoc.ParentPath(path))
	if !ok {
		return nil, nil, false
	}
	if marked, _ := node[userSchemaMarker].(bool); marked {
		names := offeredTypeNames()
		return names, make([]string, len(names)), schema.KeywordKind("type") == "string|array"
	}
	// A discriminated union names its variants in the arms themselves, which is where the
	// prose describing each one lives too.
	for _, arm := range unionArms(node) {
		m, _ := arm.(map[string]any)
		if m, ok = resolve(root, m); !ok {
			continue
		}
		props, _ := m["properties"].(map[string]any)
		typ, _ := props["type"].(map[string]any)
		if c, ok := typ["const"].(string); ok {
			names = append(names, c)
			detail = append(detail, firstSentence(describeNode(m)))
		}
	}
	return names, detail, false
}

// offeredTypeNames is schema.TypeNames without `array`: beside the list form it read as a
// second spelling of it, and the two mean opposite things. `type: array` is still ACCEPTED —
// this is what the editor puts in a list, not what the language takes.
func offeredTypeNames() []string {
	return slices.DeleteFunc(schema.TypeNames(), func(n string) bool { return n == "array" })
}

// typeLiteral is how a type name is written in the document. Bare `null` is YAML's null VALUE,
// so `type: null` decodes as no type at all — silently, which is the worst kind.
func typeLiteral(name string) string {
	if name == "null" {
		return `"null"`
	}
	return name
}

// firstSentence is as much of a description as fits beside a value in a list.
func firstSentence(s string) string {
	if i := strings.IndexAny(s, ".—\n"); i > 0 {
		return strings.TrimSpace(s[:i])
	}
	return s
}

package lsp

// What an `on_error` rule can catch. The codes come from `errcode.Catchable`, which is where
// they are declared and described, so one added there is offered here with no edit; this file
// answers the two questions errcode cannot — which KIND of task the cursor is in, and what that
// task's own `raises` declares.
//
// THIS DOCUMENT ONLY. A child's own raise set lives in the child's file, and reading it here
// would make one buffer's answer depend on the state of another.

import (
	"fmt"
	"maps"
	"slices"
	"strings"

	"genroc/internal/defdoc"
	"genroc/internal/errcode"
	"genroc/internal/model"
)

// offeredCode is a code and what produces it — the half a reader cannot guess from the name.
type offeredCode struct{ code, detail string }

// errorCodeValues offers what an `on_error` rule's `code` may match. A recognised slot answers
// even when the set comes out EMPTY: a list of codes never takes a key, and the rule's own
// siblings are what the cursor there used to be given.
func errorCodeValues(text string, line, col int) ([]completionItem, bool) {
	src := lineAt(text, line)
	doc, ok := parseRepaired(text, line)
	if !ok {
		return nil, false
	}
	path, ok := valueSlot(doc, src, line, col, isErrorCodeSlot)
	if !ok {
		if path, ok = dashSlot(doc, text, line, col); !ok || !isErrorCodeSlot(doc, path) {
			return nil, false
		}
	}
	from := replaceFrom(src, col, isCodeToken)
	codes := catchableCodes(doc, taskOf(path))
	out := make([]completionItem, 0, len(codes))
	for i, c := range codes {
		out = append(out, completionItem{
			Label:       c.code,
			Kind:        kindValue,
			Detail:      c.detail,
			SortText:    fmt.Sprintf("%03d", i),
			replaceFrom: from,
		})
	}
	return out, true
}

// isErrorCodeSlot reports whether path is an on_error rule's `code`, or one entry of it. The
// index a list adds is part of the same slot; `on_error.N.raise.code` is NOT — that code is
// being invented, and nothing can offer it.
func isErrorCodeSlot(_ *defdoc.Doc, path string) bool {
	seg := strings.Split(path, ".")
	if n := len(seg); n > 0 && isIndex(seg[n-1]) {
		seg = seg[:n-1]
	}
	n := len(seg)
	return n >= 5 && seg[0] == "tasks" && seg[n-1] == "code" && isIndex(seg[n-2]) && seg[n-3] == "on_error"
}

// dashSlot resolves a cursor on a block list's dash with nothing typed after it. Such an
// element has no node to stand in, so the slot is the KEY that opened the list — which sits at
// a smaller indent, exactly where keyAbove looks from the dash's own column.
func dashSlot(doc *defdoc.Doc, text string, line, col int) (string, bool) {
	src := lineAt(text, line)
	dash := strings.IndexByte(src, '-')
	if dash < 0 || col <= dash+1 || strings.TrimSpace(src[dash+1:]) != "" {
		return "", false
	}
	anchorLine, anchorCol, sibling, found := keyAbove(text, line, dash+1)
	if !found || sibling {
		return "", false
	}
	return doc.At(anchorLine, anchorCol)
}

func taskOf(path string) string {
	if i := strings.Index(path, ".on_error."); i >= 0 {
		return path[:i]
	}
	return ""
}

// catchableCodes is what a task's on_error can see: the codes errcode classifies for this kind
// of task, after the ones its action declares. A task whose action reports none — a delay, or an
// action not written yet — answers with nothing, which is the honest answer.
func catchableCodes(doc *defdoc.Doc, taskPath string) []offeredCode {
	v, _ := doc.ValueAt(taskPath)
	task, _ := v.(map[string]any)
	action, _ := task["action"].(map[string]any)

	var kinds errcode.Kind
	var out []offeredCode
	switch model.ActionType(stringField(action, "type")) {
	case model.ActionTypeFetch:
		kinds, out = errcode.KindFetch, slices.Clone(fetchPatterns)
	case model.ActionTypeExternal:
		kinds, out = errcode.KindExternal, declaredRaises(action)
	case model.ActionTypeChild, model.ActionTypeChildMap, model.ActionTypeChildList:
		kinds = errcode.KindChild
		for _, entry := range childEntries(action) {
			out = append(out, declaredRaises(entry)...)
		}
	}
	// only_once needs an action to protect, and R5 bounds a child task's rules by the raise set
	// — so `only_once.interrupted` is not offered there, though the engine can report it.
	// specs/child-error-handling.md R5.
	if only, _ := task["only_once"].(bool); only && action != nil && kinds != errcode.KindChild {
		kinds |= errcode.KindOnlyOnce
	}
	for _, info := range errcode.Catchable(kinds) {
		out = append(out, offeredCode{string(info.Code), info.Means})
	}
	return dedupe(out)
}

// childEntries is where a child task names the processes it spawns: on the action itself, or
// one entry per key under `children`.
func childEntries(action map[string]any) []map[string]any {
	children, ok := action["children"].(map[string]any)
	if !ok {
		return []map[string]any{action}
	}
	out := make([]map[string]any, 0, len(children))
	for _, key := range slices.Sorted(maps.Keys(children)) {
		if entry, ok := children[key].(map[string]any); ok {
			out = append(out, entry)
		}
	}
	return out
}

func declaredRaises(at map[string]any) []offeredCode {
	declared, _ := at["raises"].(map[string]any)
	out := make([]offeredCode, 0, len(declared))
	for _, code := range slices.Sorted(maps.Keys(declared)) {
		out = append(out, offeredCode{code, "declared in raises"})
	}
	return out
}

func stringField(m map[string]any, key string) string {
	s, _ := m[key].(string)
	return s
}

func dedupe(codes []offeredCode) []offeredCode {
	seen := make(map[string]bool, len(codes))
	out := make([]offeredCode, 0, len(codes))
	for _, c := range codes {
		if seen[c.code] {
			continue
		}
		seen[c.code] = true
		out = append(out, c)
	}
	return out
}

// fetchPatterns are the families a reader names with a wildcard instead of a code, offered
// before the codes they cover. They are spellings, not codes — nothing ever stores one — which
// is why errcode does not carry them: the status family is unbounded (errcode.HTTP), and
// `pre.%` is how an only_once task names the one family it may retry.
var fetchPatterns = []offeredCode{
	{"http.4%", "any 4xx status accepted_status did not admit"},
	{"http.5%", "any 5xx status accepted_status did not admit"},
	{errcode.NotReached + "%", "the request never left — the family an only_once task may retry"},
}

func isCodeToken(c byte) bool { return c == '.' || c == '%' || isIdentByte(c) }

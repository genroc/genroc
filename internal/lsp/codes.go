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
	"strconv"
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
func errorCodeValues(text, file string, line, col int) ([]completionItem, bool) {
	src := lineAt(text, line)
	d, ok := parseRepaired(text, file, line)
	if !ok {
		return nil, false
	}
	path, ok := valueSlot(d.Doc, src, line, col, isErrorCodeSlot)
	if !ok {
		if path, ok = dashSlot(d.Doc, text, line, col); !ok || !isErrorCodeSlot(d.Doc, path) {
			return nil, false
		}
	}
	from := replaceFrom(src, col, isCodeToken)
	// Through the definition, not the document: a child's raise set can arrive by a `<<`
	// spread, and only the decoded definition carries what that supplied.
	var task *model.Task
	if def, ok := d.definition(); ok {
		task = taskIn(def, taskOf(path))
	}
	codes := catchableCodes(task)
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
// of task, after the ones its action declares. A task whose action reports none -- a delay, or an
// action not written yet -- answers with nothing, which is the honest answer; nil is a task the
// document does not have yet, and answers the same way.
func catchableCodes(task *model.Task) []offeredCode {
	var action *model.Action
	only := false
	if task != nil {
		action = task.Action
		only = task.OnlyOnce != nil && *task.OnlyOnce
	}
	var actionType model.ActionType
	if action != nil {
		actionType = action.Type
	}
	// Shared with registration, which refuses a rule naming anything outside this set: what the
	// editor offers and what applies must be one answer. `only_once` needing an action, and R5
	// bounding a child task's rules by the raise set, are decided there.
	kinds := model.CatchableKinds(actionType, only)

	var out []offeredCode
	switch actionType {
	case model.ActionTypeFetch:
		out = slices.Clone(fetchPatterns)
	case model.ActionTypeExternal:
		out = declaredRaises(action.Raises)
	case model.ActionTypeChild, model.ActionTypeChildMap, model.ActionTypeChildList:
		// Three spellings reach one field: `child` and `child_list` declare on the action,
		// `child_map` once per entry under `children`.
		if len(action.Children) == 0 {
			out = declaredRaises(action.Raises)
		}
		for _, key := range slices.Sorted(maps.Keys(action.Children)) {
			out = append(out, declaredRaises(action.Children[key].Raises)...)
		}
	}
	for _, info := range errcode.Catchable(kinds) {
		out = append(out, offeredCode{string(info.Code), info.Means})
	}
	return dedupe(out)
}

// taskIn is the task a slot address names: `tasks.<id>` or `tasks[<n>]`, the two spellings
// defdoc gives one position.
func taskIn(def *model.ProcessDefinition, taskPath string) *model.Task {
	rest, ok := strings.CutPrefix(taskPath, "tasks")
	if !ok || def == nil {
		return nil
	}
	if index, ok := strings.CutPrefix(rest, "["); ok {
		n, err := strconv.Atoi(strings.TrimSuffix(index, "]"))
		if err != nil || n < 0 || n >= len(def.Tasks) {
			return nil
		}
		return def.Tasks[n]
	}
	id := strings.TrimPrefix(rest, ".")
	for _, t := range def.Tasks {
		if t != nil && t.ID == id {
			return t
		}
	}
	return nil
}

func declaredRaises(declared model.Raises) []offeredCode {
	out := make([]offeredCode, 0, len(declared))
	for _, code := range slices.Sorted(maps.Keys(declared)) {
		out = append(out, offeredCode{code, "declared in raises"})
	}
	return out
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

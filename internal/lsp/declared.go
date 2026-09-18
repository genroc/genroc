package lsp

// Key completion inside a shape whose slot carries a DECLARED schema.
// specs/declared-slot-schemas.md §6.
//
// This is a second source beside `processSchema`, not a repair of it. The generated schema
// describes the definition LANGUAGE, and "this mapping's keys come from the value of a sibling
// key, possibly via a file" is not expressible as a JSON Schema. The keys here are the author's
// own type in a key position, which is the one thing key completion has never had.
//
// It is `legalKeys`'s item shape fed from `membersOf`'s source: the required-first sortText,
// the colon the item writes and the drop-what-is-already-written rule all belong to the key
// position, while the type summary and MayBeAbsent all belong to a schema.Schema.

import (
	"strconv"
	"strings"

	"genroc/internal/model"
	"genroc/internal/schema"
)

// declaredKeys returns the keys the declaration governing this mapping allows, minus the ones
// already written, or false where no declaration reaches here.
//
// The declaration is read from the RESOLVED definition and never off `Doc`: a `$process` spread
// supplies one with no node in the document's index, and reading a declaration off the text as
// written is how three handlers were each found answering about a document nobody applies.
func declaredKeys(d *document, path string) ([]completionItem, bool) {
	def, ok := d.definition()
	if !ok {
		return nil, false
	}
	declared, rest, ok := declaredSlotAt(def, path)
	if !ok || declared == nil {
		return nil, false
	}
	// An empty remainder is the slot's own root — the cursor is directly inside the mapping the
	// declaration types — and navigating to it is not a walk of zero steps but an error.
	here := *declared
	if rest != "" {
		at, err := declared.At(rest)
		if err != nil {
			return nil, false
		}
		here = at
	}
	// A nullable or referenced node offers no members until it is unwrapped — the same shape,
	// and the same fix, as membersOf and schema.Summary.
	if here.HasNull() {
		if inner := here.StripNull(); !inner.IsZero() && !inner.IsNull() {
			here = inner
		}
	}
	if resolved, err := here.Resolve(); err == nil {
		here = resolved
	}
	props := here.Properties()
	if len(props) == 0 {
		return nil, false
	}
	present := writtenKeys(d, path)
	required := map[string]bool{}
	for _, name := range here.Required() {
		required[name] = true
	}
	out := []completionItem{}
	for name, sub := range props {
		if present[name] {
			continue
		}
		out = append(out, completionItem{
			Label:         name,
			Kind:          kindProperty,
			Detail:        declaredDetail(sub, required[name]),
			insert:        name + declaredKeySuffix(sub),
			SortText:      sortKey(name, required[name]),
			Documentation: sub.Description(),
		})
	}
	return out, true
}

// writtenKeys is what the mapping at path already carries, so a key is offered once.
func writtenKeys(d *document, path string) map[string]bool {
	present := map[string]bool{}
	v, ok := d.Doc.ValueAt(path)
	if !ok {
		return present
	}
	m, ok := v.(map[string]any)
	if !ok {
		return present
	}
	for k := range m {
		present[k] = true
	}
	return present
}

func declaredDetail(s schema.Schema, required bool) string {
	parts := []string{}
	if required {
		parts = append(parts, "required")
	}
	if sum := s.Summary(); sum != "" {
		parts = append(parts, sum)
	}
	return strings.Join(parts, " ")
}

// declaredKeySuffix mirrors keySuffix: a space where the value goes beside the key, nothing
// where a block opens below it.
func declaredKeySuffix(s schema.Schema) string {
	if s.HasProperties() || s.IsType("array") || s.IsType("object") {
		return ":"
	}
	return ": "
}

// declaredSlotAt finds the declaration governing a DOCUMENT path, and the remainder of that
// path inside it. The roots are the six slots that take one; everything below a root navigates
// the declared schema, which is `SlotAt`'s longest-prefix-then-navigate pattern and not a new
// one.
//
// A sequence element is addressed by its `id` where it has one (`tasks.call.action.body`) and by
// its index otherwise, and `defdoc` registers a physical spelling (`tasks[0]`) beside the
// logical one. All three reach the same task, so all three resolve here — matching only the
// index is how this landed offering nothing at all.
func declaredSlotAt(def *model.ProcessDefinition, path string) (*schema.Schema, string, bool) {
	if rest, ok := under(path, "output"); ok {
		return def.OutputSchema, rest, true
	}
	head, inTask, found := strings.Cut(path, ".")
	if !found {
		return nil, "", false
	}
	var task *model.Task
	if i, ok := bracketIndex(head); ok {
		if head[:strings.IndexByte(head, '[')] != "tasks" || i >= len(def.Tasks) {
			return nil, "", false
		}
		task = def.Tasks[i]
	} else {
		if head != "tasks" {
			return nil, "", false
		}
		var seg string
		seg, inTask, found = strings.Cut(inTask, ".")
		if !found {
			return nil, "", false
		}
		task = taskNamed(def, seg)
	}
	if task == nil {
		return nil, "", false
	}
	if rest, ok := under(inTask, "output"); ok {
		return task.OutputSchema, rest, true
	}
	if task.Action == nil {
		return nil, "", false
	}
	inAction, ok := under(inTask, "action")
	if !ok || inAction == "" {
		return nil, "", false
	}
	for name, declared := range map[string]*schema.Schema{
		"body":  task.Action.BodySchema,
		"query": task.Action.QuerySchema,
		"input": task.Action.InputSchema,
	} {
		if rest, ok := under(inAction, name); ok {
			return declared, rest, true
		}
	}
	// A child_map entry declares its own input: `children.<key>.input`.
	if rest, ok := under(inAction, "children"); ok && rest != "" {
		key, inEntry, _ := strings.Cut(rest, ".")
		entry, found := task.Action.Children[key]
		if !found {
			return nil, "", false
		}
		if inner, ok := under(inEntry, "input"); ok {
			return entry.InputSchema, inner, true
		}
	}
	return nil, "", false
}

// taskNamed resolves a path segment to a task: its `id`, or its position where it has none.
func taskNamed(def *model.ProcessDefinition, seg string) *model.Task {
	for _, t := range def.Tasks {
		if t.ID == seg {
			return t
		}
	}
	if i, err := strconv.Atoi(seg); err == nil && i >= 0 && i < len(def.Tasks) {
		return def.Tasks[i]
	}
	return nil
}

// bracketIndex reads the `[n]` of a physical path segment.
func bracketIndex(seg string) (int, bool) {
	open := strings.IndexByte(seg, '[')
	if open < 0 || !strings.HasSuffix(seg, "]") {
		return 0, false
	}
	i, err := strconv.Atoi(seg[open+1 : len(seg)-1])
	if err != nil || i < 0 {
		return 0, false
	}
	return i, true
}

// under reports whether path is `head` or sits inside it, returning what is left below it.
func under(path, head string) (string, bool) {
	if path == head {
		return "", true
	}
	if rest, ok := strings.CutPrefix(path, head+"."); ok {
		return rest, true
	}
	return "", false
}

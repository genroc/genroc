package lsp

// Go-to-definition. Two references in a definition point somewhere: a `goto` naming a task, and
// a child action naming a process. The first is in this document and is answered here; the
// second needs the project's file set, which lives behind `.genroc` (specs/source-resolution.md).

import (
	"strings"

	"genroc/internal/defdoc"
)

// reference is what the cursor names: a task in this document, or a process defined in some
// other file. The two resolve differently, so which one it is has to survive the lookup.
type reference struct {
	taskPath string // a path within this document, when the reference is a `goto`
	process  string // a process name, when it is a child action's `name`
}

// referenceAt reads the reference under the cursor. A cursor on anything else answers with
// nothing, which is most of a file.
func referenceAt(text string, line, col int) (reference, *defdoc.Doc, bool) {
	docs, err := defdoc.ParseAll([]byte(text))
	if err != nil {
		return reference{}, nil, false
	}
	for _, doc := range docs {
		path, ok := doc.At(line, col)
		if !ok {
			continue
		}
		if target, ok := gotoTarget(doc, path); ok {
			return reference{taskPath: target}, doc, true
		}
		if name, ok := childProcess(doc, path); ok {
			return reference{process: name}, doc, true
		}
		return reference{}, nil, false
	}
	return reference{}, nil, false
}

// childProcess reads the process a child action names. Three spellings reach one field:
// `child` and `child_list` carry `name` on the action, `child_map` carries one per entry
// under `children`.
func childProcess(doc *defdoc.Doc, path string) (string, bool) {
	if !strings.HasSuffix(path, ".name") || !strings.HasPrefix(path, "tasks.") {
		return "", false
	}
	parent := defdoc.ParentPath(path)
	if !strings.HasSuffix(parent, ".action") && !strings.Contains(parent, ".action.children.") {
		return "", false
	}
	v, ok := doc.ValueAt(path)
	if !ok {
		return "", false
	}
	name, ok := v.(string)
	if !ok || name == "" {
		return "", false
	}
	return name, true
}

// definitionAt resolves a reference that stays inside this document. A child action's process
// lives in another file, so the server resolves that one — see Server.definition.
func definitionAt(text string, line, col int) (defdoc.Range, bool) {
	ref, doc, ok := referenceAt(text, line, col)
	if !ok || ref.taskPath == "" {
		return defdoc.Range{}, false
	}
	span, ok := doc.Span(ref.taskPath)
	if !ok {
		return defdoc.Range{}, false
	}
	return span.Value, true
}

// gotoTarget reads a task reference and returns the path of the task it names. `$task-id` is
// the only routing spelling that points anywhere: `end` terminates and `next` is positional,
// so neither has a definition to jump to.
func gotoTarget(doc *defdoc.Doc, path string) (string, bool) {
	if !isRoutingSlot(path) {
		return "", false
	}
	v, ok := doc.ValueAt(path)
	if !ok {
		return "", false
	}
	ref, ok := v.(string)
	if !ok {
		return "", false
	}
	id, ok := strings.CutPrefix(strings.TrimSpace(ref), "$")
	if !ok || id == "" {
		return "", false
	}
	// `tasks.<id>` is the address the index registered the task under, which is exactly what
	// makes a task addressable by name — the same spelling a diagnostic carries. Whether it
	// exists is the caller's lookup: a goto to no task simply has nowhere to go.
	return "tasks." + id, true
}

// isRoutingSlot reports whether a path names a slot whose value is a task reference:
// `tasks.<id>.switch` in its scalar form, a `goto` in a switch case, or a `goto` in an
// on_error rule.
func isRoutingSlot(path string) bool {
	seg := strings.Split(path, ".")
	if len(seg) < 3 || seg[0] != "tasks" {
		return false
	}
	last := seg[len(seg)-1]
	return last == "switch" || last == "goto"
}

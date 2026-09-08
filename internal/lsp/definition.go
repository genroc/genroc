package lsp

// Go-to-definition. Two references in a definition point somewhere: a `goto` naming a task, and
// a child action naming a process. The first is in this document and is answered here; the
// second needs the project's file set, which lives behind `.genroc` (specs/source-resolution.md).

import (
	"strings"

	"genroc/internal/defdoc"
)

// definitionAt resolves the reference under the cursor to where it is defined, in this
// document. A cursor on anything else answers with nothing, which is most of a file.
func definitionAt(text string, line, col int) (defdoc.Range, bool) {
	docs, err := defdoc.ParseAll([]byte(text))
	if err != nil {
		return defdoc.Range{}, false
	}
	for _, doc := range docs {
		path, ok := doc.At(line, col)
		if !ok {
			continue
		}
		target, ok := gotoTarget(doc, path)
		if !ok {
			return defdoc.Range{}, false
		}
		span, ok := doc.Span(target)
		if !ok {
			return defdoc.Range{}, false
		}
		return span.Value, true
	}
	return defdoc.Range{}, false
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

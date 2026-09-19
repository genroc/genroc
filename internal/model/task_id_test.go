package model

import (
	"strings"
	"testing"
)

// A task id is READ BACK in two places, and neither survives arbitrary text: `outputs.<id>` in
// an expression, and `$<id>` in a routing slot. The colon is the one that bit — `goto: $a:b`
// naming a task called `a:b` read as a resolution directive named `a`, and where a resolver
// happened to carry that name it ran. specs/source-resolution.md.

func routingTask(id string) *Task {
	return &Task{ID: id, Switch: SwitchMap{{Goto: GotoEnd}}}
}

func TestTaskID_MustBeAnIdentifier(t *testing.T) {
	for _, c := range []struct{ name, id string }{
		{"a colon reads as a resolution directive in a goto", "a:b"},
		{"a dot cannot be addressed through outputs", "a.b"},
		{"a space is spellable in neither place", "a b"},
		{"a hyphen is subtraction in an expression", "a-b"},
		{"a leading digit is not an identifier", "1st"},
		{"a bracket is indexing", "a[0]"},
	} {
		t.Run(c.name, func(t *testing.T) {
			d := def(routingTask(c.id))
			err := d.Validate()
			if err == nil {
				t.Fatalf("accepted task id %q, which cannot be written back as outputs.%s or $%s",
					c.id, c.id, c.id)
			}
			if !strings.Contains(err.Error(), "must be a letter or underscore") {
				t.Fatalf("task id %q was refused for some other reason, so this pins nothing: %v", c.id, err)
			}
		})
	}
}

// An empty id reaches an older rule that words it better, and this pins that the identifier
// rule did not take the case over with a worse message.
func TestTaskID_EmptyKeepsItsOwnMessage(t *testing.T) {
	d := def(routingTask(""))
	err := d.Validate()
	if err == nil || !strings.Contains(err.Error(), "id is required") {
		t.Fatalf("want the `id is required` refusal, got %v", err)
	}
}

// The rule must not quietly re-reserve the routing KEYWORDS. It was a separate rule once and was
// dropped deliberately: the `$` sigil is what tells a task from a keyword, so a task called
// `end` is ordinary and `goto: $end` reaches it. tests/integration/reserved_ids_test.ts runs it.
func TestTaskID_AcceptsOrdinaryIdentifiersIncludingTheKeywords(t *testing.T) {
	for _, id := range []string{"tick", "eval_node", "timed_out", "_private", "step2", "End", "end", "next"} {
		d := def(routingTask(id))
		if err := d.Validate(); err != nil {
			t.Fatalf("refused %q, which is an ordinary identifier: %v", id, err)
		}
	}
}

package main

import (
	"os"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// main.go's dispatch is the list of commands that EXIST; commandDocs is the list `genctl -h`
// shows. A command in one and not the other is either unreachable or invisible, and neither
// fails any other way -- an undocumented command simply never appears in the map.
func TestEveryCommandIsDocumented(t *testing.T) {
	src, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatalf("read main.go: %v", err)
	}
	dispatch := regexp.MustCompile(`(?m)^\tcase "([a-z-]+)":\n\t\trun[A-Za-z]+Cmd\(`)
	var cases []string
	for _, m := range dispatch.FindAllStringSubmatch(string(src), -1) {
		cases = append(cases, m[1])
	}
	if len(cases) < 15 {
		t.Fatalf("found %d dispatch cases in main.go, which cannot be right: %v", len(cases), cases)
	}

	var listed []string
	for _, g := range helpGroups {
		listed = append(listed, g.names...)
	}
	for _, name := range cases {
		if _, ok := commandDocs[name]; !ok {
			t.Errorf("%q is dispatched but has no commandDocs entry, so `genctl -h` never names it", name)
		}
		if !slices.Contains(listed, name) {
			t.Errorf("%q is dispatched but is in no helpGroup, so `genctl -h` never names it", name)
		}
	}
	for name := range commandDocs {
		if !slices.Contains(cases, name) {
			t.Errorf("%q is documented but not dispatched: `genctl %s` answers \"unknown command\"", name, name)
		}
	}
	for _, name := range listed {
		if _, ok := commandDocs[name]; !ok {
			t.Errorf("helpGroups names %q, which has no doc", name)
		}
	}
}

// The map is only a map while it fits on a screen; the page under `<cmd> -h` is where length
// belongs. The numbers are generous -- this catches the prose creeping back, not a new command.
func TestTheShortHelpStaysShort(t *testing.T) {
	var b strings.Builder
	usageTo(&b)
	if lines := strings.Count(b.String(), "\n"); lines > 45 {
		t.Errorf("`genctl -h` is %d lines; it is the map, and detail belongs under `genctl <cmd> -h`", lines)
	}
	for _, name := range []string{"apply", "resolve", "channel"} {
		if summary := commandDocs[name].summary; len(summary) > 70 {
			t.Errorf("%s: summary is %d chars, and the map lines up on one screen width", name, len(summary))
		}
	}
}

// Every command's page names the command it is for. A usage line copied from a neighbour
// during an edit is otherwise invisible -- it reads as valid grammar for the wrong command.
func TestEveryPageNamesItsOwnCommand(t *testing.T) {
	for name, doc := range commandDocs {
		if len(doc.usage) == 0 {
			t.Errorf("%s: no usage line", name)
			continue
		}
		for _, u := range doc.usage {
			if strings.HasPrefix(u, " ") { // a continuation of the line above
				continue
			}
			if !strings.HasPrefix(u, name+" ") && u != name {
				t.Errorf("%s: usage line %q is not for this command", name, u)
			}
		}
		if doc.summary == "" {
			t.Errorf("%s: no summary, so it is a blank line in `genctl -h`", name)
		}
	}
}

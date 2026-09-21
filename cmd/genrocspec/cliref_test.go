package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// buildGenctl is what makes these tests bite: the reference is parsed out of help text, so a
// fixture would only pin the parser against a copy of the format it is parsing.
func buildGenctl(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "genctl")
	out, err := exec.Command("go", "build", "-o", path, "genroc/cmd/genctl").CombinedOutput()
	if err != nil {
		t.Fatalf("build genctl: %v\n%s", err, out)
	}
	return path
}

// Every command `genctl -h` names must reach a page. The parse reads rows by indentation, so
// the ways it fails are silent: a group heading whose format shifts takes its commands with it,
// and the trailing "genctl <command> -h" block reads as a command row until something rejects it.
func TestEveryCommandReachesAPage(t *testing.T) {
	genctl := buildGenctl(t)
	dir := t.TempDir()
	if err := writeCLIReference(dir, genctl); err != nil {
		t.Fatalf("writeCLIReference: %v", err)
	}

	pages := map[string]string{}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		b, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		pages[e.Name()] = string(b)
	}

	groups, err := readCLIGroups(genctl)
	if err != nil {
		t.Fatal(err)
	}
	total := 0
	for _, g := range groups {
		page, ok := pages[slug(g.title)+".md"]
		if !ok {
			t.Errorf("group %q reached no page; the reference would not name %d commands", g.title, len(g.commands))
			continue
		}
		for _, c := range g.commands {
			total++
			if strings.Contains(c.name, " ") {
				t.Errorf("%q was read as a command, so a line that is not a command row parsed as one", c.name)
			}
			if !strings.Contains(page, "\n## "+c.name+"\n") {
				t.Errorf("`genctl %s` has no section in %s, so the reference silently drops it", c.name, slug(g.title)+".md")
			}
		}
	}
	// `genctl -h` is a full screen of commands; a parse that collapsed to a handful would
	// still produce pages that look plausible one at a time.
	if total < 15 {
		t.Errorf("only %d commands were parsed out of `genctl -h`, which cannot be right", total)
	}
}

// Frontmatter is YAML, and the descriptions carry a colon. An unquoted one fails the content
// collection's PARSE, which reports a line and column rather than the page that is wrong.
func TestFrontmatterSurvivesAColon(t *testing.T) {
	page := renderCLIGroup(cliGroup{
		title:    "Channels",
		commands: []cliCommand{{name: "channel", summary: "move the pointers", grammar: []string{"channel list"}}},
	}, 3)
	want := "description: \"The genctl commands for channels: `channel`.\"\n"
	if !strings.Contains(page, want) {
		t.Errorf("description is not a quoted YAML scalar, so the collection fails to parse it:\n%s", page)
	}
}

// Help text is written for a terminal, where `tasks.<id>` and a `(**` glob are literal. Markdown
// reads the first as an HTML tag and drops it, and the second can open emphasis that closes on
// some later heading -- both of which render a plausible page with content missing from it.
func TestProseIsEscapedOutsideCodeSpansOnly(t *testing.T) {
	for _, c := range []struct{ in, want string }{
		{"tasks.<id>.output", "tasks.&lt;id>.output"},
		{"globbed (** matches any depth)", `globbed (\*\* matches any depth)`},
		{"`$<resolver>:` leaves resolve", "`$<resolver>:` leaves resolve"},
		{"`a**b` and c**d", "`a**b` and c\\*\\*d"},
	} {
		if got := escapeProse(c.in); got != c.want {
			t.Errorf("escapeProse(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

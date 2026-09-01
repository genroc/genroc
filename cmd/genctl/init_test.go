package main

import (
	"bufio"
	"io"
	"strings"
	"testing"
)

func newPrompter(input string) prompter {
	return prompter{in: bufio.NewReader(strings.NewReader(input)), out: io.Discard}
}

func TestInitPrompt_BlankTakesTheDefault(t *testing.T) {
	p := newPrompter("\n\n")
	if got := p.ask("name", "my-proj"); got != "my-proj" {
		t.Errorf("blank answer = %q, want the default — pressing enter must not blank the field", got)
	}
	if p.askYesNo("scripts", false) {
		t.Error("blank answer flipped a false default to true")
	}
}

func TestInitPrompt_ReadsAnAnswer(t *testing.T) {
	p := newPrompter("orders\ny\n")
	if got := p.ask("name", "dir-name"); got != "orders" {
		t.Errorf("name = %q, want %q", got, "orders")
	}
	if !p.askYesNo("scripts", false) {
		t.Error(`"y" did not select eval-node`)
	}
}

// EOF is what a closed pipe and a Ctrl-D both look like. Neither may hang or produce an empty
// project name.
func TestInitPrompt_EOFTakesDefaults(t *testing.T) {
	p := newPrompter("")
	if got := p.ask("name", "fallback"); got != "fallback" {
		t.Errorf("EOF = %q, want the default", got)
	}
	if p.askYesNo("scripts", false) {
		t.Error("EOF selected eval-node")
	}
}

func TestSanitizeName(t *testing.T) {
	for in, want := range map[string]string{
		"My Weird App.v2": "my-weird-app-v2",
		"orders":          "orders",
		"---":             "my-process",
		"":                "my-process",
	} {
		if got := sanitizeName(in); got != want {
			t.Errorf("sanitizeName(%q) = %q, want %q — a process name is an identifier in expressions", in, got, want)
		}
	}
}

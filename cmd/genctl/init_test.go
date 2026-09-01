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
	if got := p.ask("database", "sqlite"); got != "sqlite" {
		t.Errorf("blank answer = %q, want the default — pressing enter must not blank the field", got)
	}
	if p.askYesNo("scripts", false) {
		t.Error("blank answer flipped a false default to true")
	}
}

func TestInitPrompt_ReadsAnAnswer(t *testing.T) {
	p := newPrompter("postgres\ny\n")
	if got := p.ask("database", "sqlite"); got != "postgres" {
		t.Errorf("database = %q, want %q", got, "postgres")
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

// The wiring from an answer to what gets written. A pty harness could not drive this
// reliably, and "did the eval-node answer reach the file set and the next steps" is exactly
// the part that silently regresses.
func TestInitOptions_AnswersReachTheDecision(t *testing.T) {
	for _, tc := range []struct {
		name, input string
		want        options
	}{
		{"all defaults", "\n\n\n\n", options{dir: "genroc-app", evalNode: false, compose: true, postgres: false}},
		{"eval-node, no compose", "proj\ny\nn\n", options{dir: "proj", evalNode: true, compose: false, postgres: false}},
		{"postgres", "proj\nn\ny\npostgres\n", options{dir: "proj", evalNode: false, compose: true, postgres: true}},
		{"current directory", ".\ny\ny\n\n", options{dir: ".", evalNode: true, compose: true, postgres: false}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := options{dir: ".", compose: true}.prompt(newPrompter(tc.input))
			if got != tc.want {
				t.Errorf("answers %q gave %+v, want %+v", tc.input, got, tc.want)
			}
		})
	}
}

// The database question is only asked when a compose file is being written; asking otherwise
// consumes an answer meant for nothing and desynchronises every prompt after it.
func TestInitOptions_NoDatabaseQuestionWithoutCompose(t *testing.T) {
	got := options{dir: "x", compose: true}.prompt(newPrompter("n\nn\npostgres\n"))
	if got.postgres {
		t.Error("postgres was selected although no compose.yaml was requested")
	}
}

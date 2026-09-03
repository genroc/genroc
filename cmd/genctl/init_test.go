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
// A flag is an answer, so the prompt must not ask the question again — and must not then
// override it with its own default, which is how `genctl init --postgres` used to come back
// as SQLite. The answers below all say "sqlite" to everything.
func TestInitOptions_AFlagIsNotReopenedByThePrompt(t *testing.T) {
	sqlite := "\n\n\n\n\n"
	got := (options{dir: ".", auth: true, postgres: true, setPostgres: true}).
		prompt(newPrompter(sqlite))
	if !got.postgres {
		t.Error("--postgres was overridden by the prompt's sqlite default")
	}
}

func TestInitOptions_AnswersReachTheDecision(t *testing.T) {
	for _, tc := range []struct {
		name, input string
		want        options
	}{
		// Three questions now: the folder, script tasks, and the database. The login is NOT
		// asked — it is the default, and `--no-auth` is the answer for someone who has already
		// decided otherwise.
		{"all defaults", "\n\n\n", options{dir: "genroc-app", auth: true, email: defaultEmail}},
		{"eval-node", "proj\ny\n\n", options{dir: "proj", evalNode: true, auth: true, email: defaultEmail}},
		{"postgres", "proj\nn\npostgres\n",
			options{dir: "proj", postgres: true, auth: true, email: defaultEmail}},
		{"an email of one's own", ".\ny\n\nada@example.com\n",
			options{dir: ".", evalNode: true, auth: true, email: "ada@example.com"}},
		// With --no-auth there is no account to name, so the email is never asked for.
		{"no auth asks no email", "proj\nn\n\n", options{dir: "proj"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			seed := options{dir: ".", auth: true}
			if tc.name == "no auth asks no email" {
				seed.auth = false
			}
			got := seed.prompt(newPrompter(tc.input))
			if got != tc.want {
				t.Errorf("answers %q gave %+v, want %+v", tc.input, got, tc.want)
			}
		})
	}
}

// `^dev` and `^edge` both shipped as invalid npm ranges, and `edge` as a dist-tag that does not
// exist. A channel name is not a version, and the scaffold has to tell them apart or
// `npm install` fails on a fresh project. The version case is EXACT: the resolver speaks a
// protocol with this binary, so a range could pull one the binary does not expect.
func TestReleaseTag(t *testing.T) {
	saved := version
	t.Cleanup(func() { version = saved })
	for in, want := range map[string]string{
		"0.1.0":      "0.1.0",
		"0.1.0-rc.1": "0.1.0-rc.1",
		"edge":       "edge", // published from main, as both an image tag and an npm dist-tag
		// A local build takes `latest`, not `preview` (published only from a prerelease tag, so
		// it named an image that does not exist) and not `edge` (main, which is not what someone
		// scaffolding a project wants by default). `--version edge` is how to ask for main.
		"dev":    "latest",
		"":       "latest",
		"v0.1.0": "latest", // the tag, not the version — a `v` is not a number
	} {
		version = in
		if got := releaseTag(); got != want {
			t.Errorf("version %q gave release tag %q, want %q", in, got, want)
		}
	}
}

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

// EOF is a closed pipe or Ctrl-D: neither may hang or produce an empty folder name.
func TestInitPrompt_EOFTakesDefaults(t *testing.T) {
	p := newPrompter("")
	if got := p.ask("name", "fallback"); got != "fallback" {
		t.Errorf("EOF = %q, want the default", got)
	}
	if p.askYesNo("scripts", false) {
		t.Error("EOF selected eval-node")
	}
}

// Each case presses enter through every prompt, so only the flag can survive.
func TestInitOptions_AFlagIsNotReopenedByThePrompt(t *testing.T) {
	enter := "\n\n\n\n\n\n\n"
	if got := (options{postgres: true, setPostgres: true}).prompt(newPrompter(enter)); !got.postgres {
		t.Error("--postgres was overridden by the prompt's sqlite default")
	}
	withLogin := options{ui: true, auth: true, setUI: true, setAuth: true}
	if got := withLogin.prompt(newPrompter(enter)); !got.auth || !got.ui {
		t.Error("--auth was overridden by the prompt's default")
	}
	if got := (options{ui: true, setUI: true}).prompt(newPrompter(enter)); !got.ui {
		t.Error("--ui was overridden by the prompt's default")
	}
	if got := (options{evalNode: true, setEvalNode: true}).prompt(newPrompter(enter)); !got.evalNode {
		t.Error("--eval-node was overridden by the prompt's default")
	}
	// And a flag answering one question must not consume the answer meant for the next.
	got := withLogin.prompt(newPrompter("proj\ny\npostgres\n"))
	if got.dir != "proj" || !got.evalNode || !got.postgres {
		t.Errorf("--auth desynchronised the remaining prompts: %+v", got)
	}
}

// The folder included: -y writing into the current directory instead was a second default.
func TestInitOptions_AssumeYesAndEnterScaffoldTheSameProject(t *testing.T) {
	flags, _, _ := parseInitArgs([]string{"-y"})
	flags = flags.withDefaultDir()
	enter := options{}.prompt(newPrompter("\n\n\n\n\n\n")).withDefaultDir()
	if flags.dir != enter.dir || flags.evalNode != enter.evalNode || flags.postgres != enter.postgres ||
		flags.ui != enter.ui || flags.auth != enter.auth {
		t.Errorf("-y gave %+v, Enter gave %+v: parseInitArgs and prompt disagree on a default", flags, enter)
	}
	if named, _, _ := parseInitArgs([]string{".", "-y"}); named.withDefaultDir().dir != "." {
		t.Error(`an explicit "." must still mean the current directory`)
	}
	if flags.auth || flags.ui {
		t.Error("a UI or login is on by default; init scaffolds a laptop, so both are opt-in (--ui, --auth)")
	}
}

func TestInitOptions_AnswersReachTheDecision(t *testing.T) {
	for _, tc := range []struct {
		name, input string
		want        options
	}{
		// Five questions: the folder, script tasks, the database, the UI and the login — then
		// the email, but only when there is an account to name.
		{"all defaults", "\n\n\n\n\n\n", options{dir: "genroc-app"}},
		{"eval-node", "proj\ny\n\n\n\n", options{dir: "proj", evalNode: true}},
		{"postgres", "proj\nn\npostgres\n\n\n", options{dir: "proj", postgres: true}},
		{"the UI without a login", "proj\nn\n\ny\nn\n", options{dir: "proj", ui: true}},
		// Without the UI there is no login, so no email question follows.
		{"tokens without the UI", "proj\nn\n\nn\ny\nada@example.com\n",
			options{dir: "proj", auth: true}},
		{"an email of one's own", ".\ny\n\ny\ny\nada@example.com\n",
			options{dir: ".", evalNode: true, ui: true, auth: true, email: "ada@example.com"}},
		// Accepting the login is what adds the email question; declining never asks it.
		{"accepting the login", "proj\nn\n\ny\ny\n\n",
			options{dir: "proj", ui: true, auth: true, email: defaultEmail}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := options{}.prompt(newPrompter(tc.input))
			if got != tc.want {
				t.Errorf("answers %q gave %+v, want %+v", tc.input, got, tc.want)
			}
		})
	}
}

// A channel name is not a version: `^dev` or `^edge` is an invalid npm range, and `npm install`
// fails on a fresh project.
func TestReleaseTag(t *testing.T) {
	saved := version
	t.Cleanup(func() { version = saved })
	for in, want := range map[string]string{
		"0.1.0":      "0.1.0",
		"0.1.0-rc.1": "0.1.0-rc.1",
		"edge":       "edge", // published from main, as both an image tag and an npm dist-tag
		// Not `preview` (published only from a prerelease tag) nor `edge` (main, a poor default).
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

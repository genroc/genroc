package main

import (
	"bufio"
	"embed"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"text/template"
)

// `genctl init` — a project skeleton.
//
// Templates are EMBEDDED rather than fetched. A scaffolder that downloads its templates
// produces skew: genctl v0.3 writing a project written for v0.1. Embedding makes the skeleton
// a property of the binary that wrote it.

//go:embed all:templates
var templates embed.FS

func runInitCmd(args []string) {
	evalNode, assumeYes := false, false
	compose, postgres := true, false
	dir := "."
	for _, a := range args {
		switch {
		case a == "--eval-node":
			evalNode, assumeYes = true, true
		case a == "--no-compose":
			compose = false
		case a == "--postgres":
			postgres = true
		case a == "-y", a == "--yes":
			assumeYes = true
		case strings.HasPrefix(a, "-"):
			fatal("init: unknown option %q\n"+
				"usage: genctl init [dir] [--eval-node] [--postgres] [--no-compose] [-y]", a)
		default:
			dir = a
		}
	}

	// Prompt only when someone is there to answer. A pipe, a CI job or `| head` gets the
	// defaults instead of hanging on a read nobody will satisfy.
	choices := options{dir: dir, evalNode: evalNode, compose: compose, postgres: postgres}
	if !assumeYes && interactive() {
		choices = choices.prompt(prompter{in: bufio.NewReader(os.Stdin), out: os.Stderr})
	}
	dir, evalNode, compose, postgres = choices.dir, choices.evalNode, choices.compose, choices.postgres

	set := "base"
	if evalNode {
		set = "scripts"
	}
	data := struct {
		Dep, Image, WorkerImage string
		EvalNode, Postgres      bool
	}{
		Dep:         depRange(),
		Image:       "ghcr.io/genroc/genroc:" + imageTag(),
		WorkerImage: "ghcr.io/genroc/eval-node:" + imageTag(),
		EvalNode:    evalNode, Postgres: postgres,
	}

	root := "templates/" + set
	var written []string
	err := fs.WalkDir(templates, root, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return err
		}
		out, err := render(p, filepath.Join(dir, rel), data)
		if err != nil {
			return err
		}
		written = append(written, out)
		return nil
	})
	if err != nil {
		fatal("init: %v", err)
	}
	if compose {
		out, err := render("templates/compose.yaml", filepath.Join(dir, "compose.yaml"), data)
		if err != nil {
			fatal("init: %v", err)
		}
		written = append(written, out)
	}

	for _, w := range written {
		fmt.Println("created", w)
	}
	fmt.Println()
	fmt.Print("next:  ")
	if clean := filepath.Clean(dir); clean != "." {
		fmt.Printf("cd %s\n       ", clean)
	}
	if compose {
		fmt.Println("docker compose up -d")
		fmt.Print("       ")
	}
	if evalNode {
		fmt.Println("npm install")
		fmt.Println("       genctl apply")
	} else {
		fmt.Println("genctl apply")
	}
	fmt.Println("       genctl run hello --set who=you")
	if !evalNode {
		fmt.Println("\nTypeScript tasks?  genctl init --eval-node  (adds @genroc/eval-node)")
	}
}

// EXACT, not a range. The resolver and genctl speak a manifest protocol (specs/source-
// resolution.md), so the bundler that runs must be the one this genctl was released with — a
// caret would let npm pick a newer resolver than the binary invoking it.
//
// A CHANNEL is not a version. `edge` names an npm dist-tag published from main, so it pins the
// matching bundler; a plain `go build` has no counterpart and takes `latest`. Pinned by
// TestDepRange, because this was wrong twice.
func depRange() string {
	switch {
	case isSemver(version):
		return version
	case version == "edge":
		return "edge"
	default:
		return "latest"
	}
}

// Major.minor.patch, with an optional prerelease suffix. Deliberately not a full semver parse:
// what matters is telling a version from a channel name.
func isSemver(v string) bool {
	digits := 0
	for _, part := range strings.SplitN(strings.SplitN(v, "-", 2)[0], ".", 4) {
		if part == "" {
			return false
		}
		for _, r := range part {
			if r < '0' || r > '9' {
				return false
			}
		}
		digits++
	}
	return digits == 3
}

// `edge` and a release version are both real image tags; only a plain `go build` has none, and
// that scaffolds against the moving prerelease pointer.
func imageTag() string {
	if version == "dev" || version == "" {
		return "preview"
	}
	return version
}

// options is what init decides before it writes anything. Separated from the writing so the
// answers can be tested without a terminal -- a pty harness proved unreliable, and the wiring
// from an answer to a generated file is the part worth pinning.
type options struct {
	dir                         string
	evalNode, compose, postgres bool
}

func (o options) prompt(p prompter) options {
	// A DIRECTORY, said so plainly. The earlier wording ("project name") implied the answer
	// became something inside the files; it is the folder this writes into.
	if o.dir == "." {
		o.dir = p.ask("folder to create (. for the current directory)", "genroc-app")
	}
	o.evalNode = p.askYesNo("TypeScript script tasks (@genroc/eval-node)", false)
	o.compose = p.askYesNo("compose.yaml to run genroc locally", true)
	if o.compose {
		o.postgres = strings.HasPrefix(strings.ToLower(p.ask("database (sqlite/postgres)", "sqlite")), "p")
	}
	return o
}

// render writes one embedded template. It never overwrites: init runs in a directory someone
// may already care about, and a clobbered definition is not recoverable from here.
func render(tmpl, out string, data any) (string, error) {
	if _, err := os.Stat(out); err == nil {
		return "", fmt.Errorf("%s already exists", out)
	}
	body, err := templates.ReadFile(tmpl)
	if err != nil {
		return "", err
	}
	t, err := template.New(tmpl).Parse(string(body))
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Dir(out), 0o755); err != nil {
		return "", err
	}
	f, err := os.Create(out)
	if err != nil {
		return "", err
	}
	defer f.Close()
	if err := t.Execute(f, data); err != nil {
		return "", err
	}
	return out, nil
}

func interactive() bool {
	fi, err := os.Stdin.Stat()
	return err == nil && fi.Mode()&os.ModeCharDevice != 0
}

// prompter carries the input rather than reading a package-level reader, so the prompts are
// testable without a terminal.
type prompter struct {
	in  *bufio.Reader
	out io.Writer
}

func (p prompter) ask(label, def string) string {
	fmt.Fprintf(p.out, "%s [%s]: ", label, def)
	line, err := p.in.ReadString('\n')
	if err != nil && line == "" {
		fmt.Fprintln(p.out)
		return def
	}
	if line = strings.TrimSpace(line); line != "" {
		return line
	}
	return def
}

func (p prompter) askYesNo(label string, def bool) bool {
	hint := "y/N"
	if def {
		hint = "Y/n"
	}
	switch strings.ToLower(p.ask(label, hint)) {
	case "y", "yes":
		return true
	case "n", "no":
		return false
	default:
		return def
	}
}

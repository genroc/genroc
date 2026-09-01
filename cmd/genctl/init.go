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

	name := sanitizeName(filepath.Base(mustAbs(dir)))

	// Prompt only when someone is there to answer. A pipe, a CI job or `| head` gets the
	// defaults instead of hanging on a read nobody will satisfy.
	if !assumeYes && interactive() {
		p := prompter{in: bufio.NewReader(os.Stdin), out: os.Stderr}
		name = sanitizeName(p.ask("project name", name))
		evalNode = p.askYesNo("TypeScript script tasks (@genroc/eval-node)", false)
		compose = p.askYesNo("compose.yaml to run genroc locally", true)
		if compose {
			postgres = strings.HasPrefix(strings.ToLower(p.ask("database (sqlite/postgres)", "sqlite")), "p")
		}
	}

	set := "base"
	if evalNode {
		set = "scripts"
	}
	data := struct {
		Name, Dep, Image, WorkerImage string
		EvalNode, Postgres            bool
	}{
		Name: name, Dep: depRange(),
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
		out, err := render(p, filepath.Join(dir, filepath.Base(p)), data)
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
	if compose {
		fmt.Println("docker compose up -d")
		fmt.Print("       ")
	}
	if evalNode {
		fmt.Println("npm install")
		fmt.Println("       genctl apply -f hello.genroc.yaml     # bundles and typechecks greet.ts")
	} else {
		fmt.Println("genctl apply -f hello.genroc.yaml")
	}
	fmt.Printf("       genctl run %s --set who=you\n", data.Name)
	if !evalNode {
		fmt.Println("\nTypeScript tasks?  genctl init --eval-node  (adds @genroc/eval-node)")
	}
}

// A dev build has no released version to depend on, and "^dev" is not a semver range npm will
// accept — so an unreleased genctl scaffolds against the published latest instead.
func depRange() string {
	if version == "dev" || version == "" {
		return "latest"
	}
	return "^" + version
}

// An unreleased genctl has no matching image tag, so it scaffolds against the moving prerelease
// pointer rather than a version nobody published.
func imageTag() string {
	if version == "dev" || version == "" {
		return "preview"
	}
	return version
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

func mustAbs(p string) string {
	a, err := filepath.Abs(p)
	if err != nil {
		fatal("init: %v", err)
	}
	return a
}

// A process name is an identifier in expressions, so a directory like "my app" cannot become
// one verbatim.
func sanitizeName(s string) string {
	s = strings.ToLower(s)
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
		case r == '-' || r == '_':
			b.WriteRune('-')
		case r == ' ' || r == '.':
			b.WriteRune('-')
		}
	}
	out := strings.Trim(b.String(), "-")
	if out == "" {
		return "my-process"
	}
	return out
}

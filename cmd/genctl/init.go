package main

import (
	"bufio"
	"crypto/rand"
	"embed"
	"encoding/base64"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"text/template"

	"golang.org/x/crypto/bcrypt"
)

// `genctl init` — a project skeleton. Templates are EMBEDDED, never fetched: a downloaded
// skeleton skews against the binary that writes it.

//go:embed all:templates
var templates embed.FS

func runInitCmd(args []string) {
	// Under init, not top-level: the password it replaces is the one `init` printed.
	if len(args) > 0 && args[0] == "password" {
		runPasswordCmd(args[1:])
		return
	}
	if hasHelpArg(args) {
		helpFor("init")
		return
	}
	choices, tag, assumeYes := parseInitArgs(args)

	// Prompt only when someone can answer: a pipe or CI job gets the defaults, not a hang.
	if !assumeYes && interactive() {
		choices = choices.prompt(prompter{in: bufio.NewReader(os.Stdin), out: os.Stderr})
	}
	dir, evalNode, postgres, auth := choices.dir, choices.evalNode, choices.postgres, choices.auth

	set := "base"
	if evalNode {
		set = "scripts"
	}
	data := struct {
		Dep, Image, WorkerImage, UIImage string
		Email, Hash                      string
		EvalNode, Postgres, Auth         bool
	}{
		Dep:         tag,
		Image:       "ghcr.io/genroc/genroc:" + tag,
		WorkerImage: "ghcr.io/genroc/eval-node:" + tag,
		UIImage:     "ghcr.io/genroc/ui:" + tag,
		EvalNode:    evalNode, Postgres: postgres, Auth: auth,
	}

	// The login is generated BEFORE anything is written, so a failure to hash leaves no
	// half-built project holding a config that names a user nobody can sign in as.
	var login *login
	if auth {
		var err error
		if login, err = newLogin(choices.email); err != nil {
			fatal("init: %v", err)
		}
		data.Email, data.Hash = login.email, login.hash
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
	out, err := render("templates/compose.yaml", filepath.Join(dir, "compose.yaml"), data)
	if err != nil {
		fatal("init: %v", err)
	}
	written = append(written, out)

	if auth {
		out, err := render("templates/ui/ui.yaml", filepath.Join(dir, "ui.yaml"), data)
		if err != nil {
			fatal("init: %v", err)
		}
		written = append(written, out)
		secrets, err := writeSecrets(filepath.Join(dir, "data"), evalNode)
		if err != nil {
			fatal("init: %v", err)
		}
		written = append(written, secrets...)
	}
	// Unconditional: ./data holds the database even without a login. Appended, not rendered:
	// every template set already ships a .gitignore.
	if err := ignoreData(filepath.Join(dir, ".gitignore")); err != nil {
		fatal("init: %v", err)
	}

	for _, w := range written {
		fmt.Println("created", w)
	}
	// Credentials go in the steps, not above the file list, where they get scrolled past.
	const indent = "       "
	fmt.Println()
	fmt.Print("next:  ")
	if clean := filepath.Clean(dir); clean != "." {
		fmt.Printf("cd %s\n%s", clean, indent)
	}
	fmt.Println("docker compose up -d")
	if evalNode {
		fmt.Println(indent + "npm install")
	}
	if login != nil {
		// genctl needs its own token (genroc accepts only `Authorization`, not the session
		// cookie), and minting one takes this password -- so one step, not two.
		fmt.Println(indent + "open http://localhost:8448 and sign in")
		fmt.Printf("%s    email     %s\n", indent, login.email)
		fmt.Printf("%s    password  %s\n", indent, login.password)
		fmt.Println(indent + "mint a token in the tokens tab, then")
		fmt.Println(indent + "genctl config set token genroc_sk_...")
	}
	fmt.Println(indent + "genctl apply")
	fmt.Println(indent + "genctl run hello --set who=you")
	if auth {
		// Stored only as a bcrypt hash, so the replacement command is named here.
		fmt.Println("\nThe password is shown once; `genctl init password` mints a replacement.\n" +
			"`config set` keeps the token in ~/.config/genroc/config.yaml (0600) rather than in " +
			"the\nenvironment, where it is inherited by every process you start and shows up in " +
			"`ps`.\n\ngenroc itself is not published — genctl reaches it through genroc-ui, " +
			"which passes a\nrequest that already carries a token straight through.")
		fmt.Println("\ndata/ holds the signing key and is world-readable, which is why it is " +
			"gitignored.\nIt is a development default: anyone with an account on this machine " +
			"can read it,\nand whoever holds that key can mint any identity genroc will accept.")
	} else {
		fmt.Println("\nNO AUTHENTICATION: every caller is an operator, and `PUT /definitions` " +
			"stores code the\nengine runs. Right on a laptop, wrong on anything anyone else " +
			"can reach.\n\nA login in front?  genctl init --auth")
	}
	if !evalNode {
		fmt.Println("\nTypeScript tasks?  genctl init --eval-node  (adds @genroc/eval-node)")
	}
}

// parseInitArgs is split out so the flag surface is testable without writing a project.
func parseInitArgs(args []string) (opts options, tag string, assumeYes bool) {
	evalNode, postgres := false, false
	// No login by default: this scaffolds a laptop. prompt's default must agree.
	auth := false
	var setEvalNode, setPostgres, setAuth bool
	dir, tag := ".", ""
	for i := 0; i < len(args); i++ {
		a := args[i]
		// A flag answers ITS OWN question only: implying -y would let `--no-auth` silently pick
		// the folder and database too.
		switch {
		case a == "--eval-node":
			evalNode, setEvalNode = true, true
		case a == "--postgres":
			postgres, setPostgres = true, true
		case a == "--no-auth":
			auth, setAuth = false, true
		case a == "--auth":
			auth, setAuth = true, true
		case a == "-y", a == "--yes":
			assumeYes = true
		case a == "--version", a == "-version":
			i++
			if i >= len(args) {
				fatal("init: --version needs a value, e.g. --version edge")
			}
			tag = args[i]
		case strings.HasPrefix(a, "--version="):
			tag = strings.TrimPrefix(a, "--version=")
		case strings.HasPrefix(a, "-"):
			fatal("init: unknown option %q\n"+
				"usage: genctl init [dir] [--eval-node] [--auth] [--postgres] "+
				"[--version <tag>] [-y]", a)
		default:
			dir = a
		}
	}
	if tag == "" {
		tag = releaseTag()
	}
	return options{
		dir: dir, evalNode: evalNode, postgres: postgres, auth: auth,
		setEvalNode: setEvalNode, setPostgres: setPostgres, setAuth: setAuth,
	}, tag, assumeYes
}

// releaseTag is both the image tag and the npm dist-tag (release.yml publishes both). EXACT for
// a release, never a caret: genctl and the resolver speak a manifest protocol, so npm must not
// pick a newer resolver than this binary. Pinned by TestReleaseTag.
func releaseTag() string {
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

// options is decided before anything is written, so answers are testable without a terminal
// (a pty harness proved unreliable).
type options struct {
	dir, email               string
	evalNode, postgres, auth bool
	// set* marks command-line answers, which the prompt must neither re-ask nor override with its
	// own default.
	setEvalNode, setPostgres, setAuth bool
}

func (o options) prompt(p prompter) options {
	// A folder, not a "project name": the answer is where this writes, not something in the files.
	if o.dir == "." {
		o.dir = p.ask("folder to create (. for the current directory)", "genroc-app")
	}
	if !o.setEvalNode {
		o.evalNode = p.askYesNo("TypeScript script tasks (@genroc/eval-node)", false)
	}
	if !o.setPostgres {
		o.postgres = strings.HasPrefix(strings.ToLower(p.ask("database (sqlite/postgres)", "sqlite")), "p")
	}
	if !o.setAuth {
		// Follows parseInitArgs: the two defaults must agree, or -y and the prompt disagree.
		o.auth = p.askYesNo("a login in front of the UI", false)
	}
	if o.auth {
		o.email = p.ask("your sign-in email", defaultEmail)
	}
	return o
}

const defaultEmail = "admin@localhost"

// login's password is GENERATED, never fixed: every scaffold would share a known credential, and
// this one reaches the port the UI publishes.
type login struct{ email, password, hash string }

func newLogin(email string) (*login, error) {
	if email = strings.TrimSpace(email); email == "" {
		email = defaultEmail
	}
	pw, err := randomPassword()
	if err != nil {
		return nil, err
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(pw), bcrypt.DefaultCost)
	if err != nil {
		return nil, err
	}
	return &login{email: email, password: pw, hash: string(hash)}, nil
}

// The alphabet omits characters that are read wrong when a password is retyped from a terminal
// (0/O, 1/l/I). 16 characters of it carry ~92 bits, so the loss costs nothing.
const passwordAlphabet = "abcdefghijkmnpqrstuvwxyzABCDEFGHJKLMNPQRSTUVWXYZ23456789"

func randomPassword() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	// Rejection-free because the alphabet's length divides evenly enough that the bias is below
	// a bit; the entropy budget above already absorbs it.
	out := make([]byte, len(b))
	for i, v := range b {
		out[i] = passwordAlphabet[int(v)%len(passwordAlphabet)]
	}
	return string(out), nil
}

// writeSecrets writes 0644 so the images can read them as any uid: a development default, stated
// as one in init's output. The alternative is a root container or a uid pinned in compose.
func writeSecrets(dir string, evalNode bool) ([]string, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	key, err := randomSecret()
	if err != nil {
		return nil, err
	}
	files := []struct{ name, content string }{{"jwt-secret", key}}
	// No operator token: a person mints their own in the UI, leaving no standing admin secret in
	// a file (the server's jwt mode skips its bootstrap token likewise).
	if evalNode {
		worker, err := randomToken()
		if err != nil {
			return nil, err
		}
		// One secret in two shapes (genroc reads `label=perms=secret`, the worker the bare token),
		// written together so they cannot disagree.
		files = append(files,
			struct{ name, content string }{"worker-token", worker},
			struct{ name, content string }{"seed-tokens", "evaluator=worker=" + worker})
	}
	var written []string
	for _, f := range files {
		path := filepath.Join(dir, f.name)
		// Never overwritten, like every other file init writes -- and here it matters more:
		// a regenerated signing key invalidates every session signed with the old one.
		if _, err := os.Stat(path); err == nil {
			return nil, fmt.Errorf("%s already exists", path)
		}
		if err := os.WriteFile(path, []byte(f.content), 0o644); err != nil {
			return nil, err
		}
		written = append(written, path)
	}
	return written, nil
}

// The prefix is what makes a leaked credential greppable, and is the shape genroc validates.
func randomToken() (string, error) {
	b, err := randomSecret()
	return "genroc_sk_" + b, err
}

func ignoreData(path string) error {
	const entry = "data/"
	body, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	for _, line := range strings.Split(string(body), "\n") {
		if strings.TrimSpace(line) == entry {
			return nil
		}
	}
	add := "\n# Everything that persists: the database, and — with a login — the signing key and\n" +
		"# the worker's credential. Whoever holds that key can mint any identity genroc accepts.\n" +
		entry + "\n"
	if len(body) > 0 && !strings.HasSuffix(string(body), "\n") {
		add = "\n" + add
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.WriteString(add)
	return err
}

// 32 bytes, which is the floor both the server and genroc-ui refuse to start below.
func randomSecret() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
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

// `genctl init password` — a new entry for genroc-ui's `login.passwords`: the only way back from
// a lost password (init's is stored as a hash), and how a second person is added.
func runPasswordCmd(args []string) {
	email := ""
	for _, a := range args {
		if strings.HasPrefix(a, "-") {
			fatal("init password: unknown option %q\nusage: genctl init password [email]", a)
		}
		email = a
	}
	l, err := newLogin(email)
	if err != nil {
		fatal("password: %v", err)
	}
	fmt.Printf("password  %s\n\n", l.password)
	fmt.Printf("Put this under `login.passwords` in ui.yaml, replacing the entry for %s if it is\n"+
		"already there, then `docker compose restart genroc-ui` — sessions survive it:\n\n", l.email)
	fmt.Printf("    - email: %s\n      hash: \"%s\"\n      groups: [admins]\n", l.email, l.hash)
}

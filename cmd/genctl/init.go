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

// `genctl init` — a project skeleton.
//
// Templates are EMBEDDED rather than fetched. A scaffolder that downloads its templates
// produces skew: genctl v0.3 writing a project written for v0.1. Embedding makes the skeleton
// a property of the binary that wrote it.

//go:embed all:templates
var templates embed.FS

func runInitCmd(args []string) {
	// A subcommand rather than a top-level verb: it belongs to what `init` set up, and the
	// password it replaces is the one `init` printed.
	if len(args) > 0 && args[0] == "password" {
		runPasswordCmd(args[1:])
		return
	}
	choices, tag, assumeYes := parseInitArgs(args)

	// Prompt only when someone is there to answer. A pipe, a CI job or `| head` gets the
	// defaults instead of hanging on a read nobody will satisfy.
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
	// Unconditional: ./data holds the database whether or not there is a login, and a Postgres
	// cluster or a SQLite file in `git status` is the same mistake as a committed signing key.
	// Appended rather than written, because every template set already ships a .gitignore.
	if err := ignoreData(filepath.Join(dir, ".gitignore")); err != nil {
		fatal("init: %v", err)
	}

	for _, w := range written {
		fmt.Println("created", w)
	}
	// The steps, credentials included. The password used to be printed above this list, which
	// put the one thing that cannot be recovered in the place people scroll past.
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
		// genctl needs a credential of its own: a browser session is a cookie, and genroc
		// accepts only `Authorization`. Minting one is an admin action, which this password
		// grants -- so the sign-in and the token belong to one step, not two.
		fmt.Println(indent + "open http://localhost:8448 and sign in")
		fmt.Printf("%s    email     %s\n", indent, login.email)
		fmt.Printf("%s    password  %s\n", indent, login.password)
		fmt.Println(indent + "mint a token in the tokens tab, then")
		fmt.Println(indent + "genctl config set token genroc_sk_...")
	}
	fmt.Println(indent + "genctl apply")
	fmt.Println(indent + "genctl run hello --set who=you")
	if auth {
		// Stored only as a bcrypt hash, so the line above is the only time it exists in
		// readable form -- which is why the replacement command is named here rather than
		// left to be searched for.
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
			"can reach.")
	}
	if !evalNode {
		fmt.Println("\nTypeScript tasks?  genctl init --eval-node  (adds @genroc/eval-node)")
	}
}

// parseInitArgs turns the command line into the decisions init makes, so the flag surface --
// six flags with interactions -- is testable without writing a project.
func parseInitArgs(args []string) (opts options, tag string, assumeYes bool) {
	evalNode, postgres := false, false
	// A login is the default: the alternative is a port on which anyone can register a
	// definition, and `PUT /definitions` stores code the engine runs.
	auth := true
	var setEvalNode, setPostgres, setAuth bool
	dir, tag := ".", ""
	for i := 0; i < len(args); i++ {
		a := args[i]
		// A flag answers ITS OWN question and no others. Making one imply -y meant
		// `--no-auth` chose the folder, the database and the script tasks too, silently.
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
				"usage: genctl init [dir] [--eval-node] [--no-auth] [--postgres] "+
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

// releaseTag names what a generated project pulls: the image tag AND the npm dist-tag, which are
// published under the same names by release.yml. One rule for both, because two rules is what let
// them disagree -- the images said `preview` (published only from a prerelease tag, so it named
// nothing) while npm said `latest`.
//
// EXACT for a release, not a range. The resolver and genctl speak a manifest protocol
// (specs/source-resolution.md), so the bundler that runs must be the one this genctl shipped
// with -- a caret would let npm pick a newer resolver than the binary invoking it.
//
// A CHANNEL is not a version: `edge` is published from main and pins the matching bundler. A
// plain `go build` has no counterpart of its own and takes `latest`, which is the right default
// for anyone who is not developing genroc itself; `--version edge` is how they get main. Pinned
// by TestReleaseTag, because this was wrong twice.
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

// options is what init decides before it writes anything. Separated from the writing so the
// answers can be tested without a terminal -- a pty harness proved unreliable, and the wiring
// from an answer to a generated file is the part worth pinning.
type options struct {
	dir, email               string
	evalNode, postgres, auth bool
	// set* records which answers came from the command line, so the prompt does not ask a
	// question already answered -- and cannot then override it with its own default, which is
	// how `--postgres` used to come back as SQLite.
	setEvalNode, setPostgres, setAuth bool
}

func (o options) prompt(p prompter) options {
	// A DIRECTORY, said so plainly. The earlier wording ("project name") implied the answer
	// became something inside the files; it is the folder this writes into.
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
		// Defaulted yes: the alternative is a port on which anyone can register a definition,
		// and `PUT /definitions` stores code the engine runs.
		o.auth = p.askYesNo("a login in front of the UI", true)
	}
	if o.auth {
		o.email = p.ask("your sign-in email", defaultEmail)
	}
	return o
}

const defaultEmail = "admin@localhost"

// login is the one account `--ui` creates. A GENERATED password rather than a fixed one: a
// scaffold that ships a known credential is a scaffold whose every user shares it, and this one
// reaches the port the UI publishes.
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

// writeSecrets generates what the stack cannot commit. This is the whole reason `--ui` exists as
// a mode rather than one more template: the compose file it replaces ran a container as root
// purely to mint these and chown them.
//
// 0644, so the images can read them as whatever uid they run as. That is a development default
// and is stated as one in init's output -- the alternative is either a root container or a uid
// pinned into a committed compose file.
func writeSecrets(dir string, evalNode bool) ([]string, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	key, err := randomSecret()
	if err != nil {
		return nil, err
	}
	files := []struct{ name, content string }{{"jwt-secret", key}}
	// No operator token is generated. A person signs in with the password below and mints their
	// own in the UI, which is one credential rather than two and leaves no standing admin secret
	// in a file. The server agrees: in jwt mode it skips its bootstrap token for the same reason.
	if evalNode {
		worker, err := randomToken()
		if err != nil {
			return nil, err
		}
		// A worker cannot log in, so its credential has to exist before it starts. One secret in
		// two shapes, because two programs read it differently: genroc wants
		// `label=perms=secret`, the worker wants the bare token. Written together so they
		// cannot disagree.
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

// `genctl password` — a new login for genroc-ui's `login.passwords`.
//
// It exists because the one `init` prints is shown once and stored only as a hash, so a lost
// password has no other way back in. It is also how a second person is added, and it replaces
// ui.yaml's `htpasswd` instruction, which needs a tool the person may not have.
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

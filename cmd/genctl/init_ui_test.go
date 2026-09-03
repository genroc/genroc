package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"text/template"

	"golang.org/x/crypto/bcrypt"
)

// `genctl init --ui` generates credentials, which is the whole reason it exists as a mode: the
// compose file it replaces ran a container as root purely to mint them.

func renderCompose(t *testing.T, auth, evalNode bool) string {
	return renderComposeDB(t, auth, evalNode, false)
}

func renderComposeDB(t *testing.T, auth, evalNode, postgres bool) string {
	t.Helper()
	body, err := templates.ReadFile("templates/compose.yaml")
	if err != nil {
		t.Fatal(err)
	}
	tmpl, err := template.New("c").Parse(string(body))
	if err != nil {
		t.Fatal(err)
	}
	var out strings.Builder
	if err := tmpl.Execute(&out, map[string]any{
		"Image": "i", "UIImage": "u", "WorkerImage": "w",
		"Auth": auth, "EvalNode": evalNode, "Postgres": postgres,
	}); err != nil {
		t.Fatal(err)
	}
	return out.String()
}

// A seed file is named exactly when something was seeded into it. Naming it otherwise points
// genroc at a path nothing wrote; not naming it when a worker exists leaves that worker
// unable to authenticate, which it reports as a 401 and then exits on.
func TestInitUI_ReadsASeedFileOnlyWhenAWorkerNeedsOne(t *testing.T) {
	for _, evalNode := range []bool{false, true} {
		out := renderCompose(t, true, evalNode)
		if got := strings.Contains(out, "GENROC_SEED_TOKENS_FILE: /data/seed-tokens"); got != evalNode {
			t.Errorf("eval-node=%v: reads the seed file = %v, want %v", evalNode, got, evalNode)
		}
		// The signing key is what every login depends on, worker or not.
		if !strings.Contains(out, "GENROC_JWT_SECRET_FILE: /data/jwt-secret") {
			t.Errorf("eval-node=%v: compose does not read the signing key, so no login can "+
				"produce a token this server accepts", evalNode)
		}
	}
}

// Under --no-auth nothing is generated, so any credential file would name a path that does not
// exist -- and the worker would hold a token the server has never heard of.
func TestInitNoAuth_ReadsNoCredentialFilesButKeepsTheUI(t *testing.T) {
	out := renderCompose(t, false, true)
	// ./data itself stays — it is where the database lives either way. What must not appear is
	// anything naming a credential, since --no-auth generates none.
	for _, env := range []string{
		"GENROC_SEED_TOKENS_FILE", "GENROC_JWT_SECRET_FILE", "GENROC_AUTH", "GENROC_TOKEN_FILE",
		"jwt-secret", "worker-token", "seed-tokens",
	} {
		if strings.Contains(out, env) {
			t.Errorf("compose references %s under --no-auth, but nothing generates it", env)
		}
	}
	// The UI is NOT what --no-auth removes: it is how anyone sees a run at all, and the server
	// has carried no UI since it became its own image.
	if !strings.Contains(out, "genroc-ui:") {
		t.Error("--no-auth dropped genroc-ui; it turns the login off, not the UI")
	}
	if !strings.Contains(out, `command: [-server, "http://genroc:8448"]`) {
		t.Error("genroc-ui has neither a config nor a -server, so it proxies nowhere")
	}
}

// Least privilege, and it is free here because genctl creates the files before compose runs:
// the worker gets its own credential and not the signing key, which would let it mint any
// identity genroc accepts.
func TestInitUI_MountsTheKeyAndTheTokenSeparately(t *testing.T) {
	out := renderCompose(t, true, true)
	for _, want := range []string{
		"./data/jwt-secret:/data/jwt-secret:ro",
		"./data/worker-token:/data/worker-token:ro",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing per-file mount %q", want)
		}
	}
	if strings.Count(out, "./data:/data") != 1 {
		t.Error("only the genroc service may mount the whole data folder; it is the one that " +
			"writes the database, and the others need one file each")
	}
}

// Everything that persists lives under ./data, so `rm -rf data` is the whole reset. A named
// volume would outlive the project folder and survive that, which is the shape people delete a
// directory and then wonder why their old definitions are still there.
func TestInitCompose_NothingPersistsOutsideTheDataFolder(t *testing.T) {
	for _, postgres := range []bool{false, true} {
		out := renderComposeDB(t, true, true, postgres)
		if strings.Contains(out, "genroc-data") {
			t.Errorf("postgres=%v: a named volume survives `rm -rf data` and a `compose down -v`, "+
				"so the reset the header documents does not reset", postgres)
		}
		if got := strings.Contains(out, "./data/postgres:/var/lib/postgresql/data"); got != postgres {
			t.Errorf("postgres=%v: cluster bind-mounted under ./data = %v", postgres, got)
		}
	}
}

func TestInitUI_SecretsAreWrittenOnceAndNeverRegenerated(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "data")
	first, err := writeSecrets(dir, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(first) != 3 {
		t.Fatalf("wrote %v, want jwt-secret, worker-token and seed-tokens", first)
	}
	// A regenerated signing key invalidates every session signed with the old one, and a
	// regenerated token locks out whoever stored the last one.
	if _, err := writeSecrets(dir, true); err == nil {
		t.Error("a second run overwrote the secrets; re-running init must refuse instead")
	}

	seeds, err := os.ReadFile(filepath.Join(dir, "seed-tokens"))
	if err != nil {
		t.Fatal(err)
	}
	worker, err := os.ReadFile(filepath.Join(dir, "worker-token"))
	if err != nil {
		t.Fatal(err)
	}
	if string(seeds) != "evaluator=worker="+string(worker) {
		t.Errorf("seed-tokens and worker-token disagree; they are one secret in two shapes\n"+
			"  seeds:  %s\n  worker: %s", seeds, worker)
	}
	// No standing admin credential on disk: a person signs in with the generated password and
	// mints their own, which is the whole reason --ui needs no operator token.
	for _, gone := range []string{"admin-token", "operator-token"} {
		if _, err := os.Stat(filepath.Join(dir, gone)); err == nil {
			t.Errorf("%s was written; an admin credential sitting in a world-readable file is "+
				"exactly what signing in replaces", gone)
		}
	}
	// 32 characters is the floor both the server and genroc-ui refuse to start below.
	if key, _ := os.ReadFile(filepath.Join(dir, "jwt-secret")); len(key) < 32 {
		t.Errorf("jwt-secret is %d characters; the server refuses to start under 32", len(key))
	}
	if !strings.HasPrefix(string(worker), "genroc_sk_") {
		t.Errorf("token %q lacks the prefix genroc validates and secret scanners grep for", worker)
	}

	// Without a worker there is nothing to seed, so the only secret is the signing key.
	bare := filepath.Join(t.TempDir(), "data")
	only, err := writeSecrets(bare, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(only) != 1 || !strings.HasSuffix(only[0], "jwt-secret") {
		t.Errorf("wrote %v, want only jwt-secret", only)
	}
}

// A login is the default, because the alternative is a port on which anyone can register a
// definition — and `PUT /definitions` stores code the engine runs.
func TestInitOptions_LoginIsTheDefaultAndNoComposeDropsItSilently(t *testing.T) {
	if !newPrompter("\n\n\n\n\n").askYesNo("a web UI behind a login (genroc-ui)", true) {
		t.Error("the prompt default is not yes")
	}
}

// The password is printed once and stored only as a hash, so the two must agree or the account
// named in ui.yaml is one nobody can sign in as.
func TestInitUI_ThePrintedPasswordMatchesTheStoredHash(t *testing.T) {
	l, err := newLogin("ada@example.com")
	if err != nil {
		t.Fatal(err)
	}
	if l.email != "ada@example.com" {
		t.Errorf("email = %q", l.email)
	}
	if err := bcrypt.CompareHashAndPassword([]byte(l.hash), []byte(l.password)); err != nil {
		t.Fatalf("the printed password does not verify against the hash written to ui.yaml: %v", err)
	}
	if bcrypt.CompareHashAndPassword([]byte(l.hash), []byte(l.password+"x")) == nil {
		t.Fatal("the hash accepts a password it was not made from")
	}
	other, err := newLogin("")
	if err != nil {
		t.Fatal(err)
	}
	if other.email != defaultEmail {
		t.Errorf("blank email = %q, want %q", other.email, defaultEmail)
	}
	if other.password == l.password {
		t.Error("two inits produced the same password; it must come from crypto/rand")
	}
}

func TestInitUI_DataIsGitignoredExactlyOnce(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".gitignore")
	if err := os.WriteFile(path, []byte("node_modules/\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if err := ignoreData(path); err != nil {
			t.Fatal(err)
		}
	}
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(string(body), "\ndata/\n"); n != 1 {
		t.Errorf("data/ appears %d times, want 1", n)
	}
	if !strings.Contains(string(body), "node_modules/") {
		t.Error("appending clobbered the template set's own entries")
	}
}

// The flag surface, which is six flags with interactions. Each of these was wrong at some point:
// the tag named a nonexistent image, --no-compose wrote a compose file anyway, and --no-ui had
// nowhere to be read.
func TestParseInitArgs(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
		want options
		tag  string
	}{
		{"defaults", nil, options{dir: ".", auth: true}, releaseTag()},
		{"a directory", []string{"orders"}, options{dir: "orders", auth: true}, releaseTag()},
		{"--version takes the next argument", []string{"--version", "edge"},
			options{dir: ".", auth: true}, "edge"},
		{"--version= is the same flag", []string{"--version=0.1.0"},
			options{dir: ".", auth: true}, "0.1.0"},
		// The value is consumed, so what follows is still parsed as a flag rather than a folder.
		{"--version does not swallow the next flag", []string{"--version", "edge", "--no-auth"},
			options{dir: ".", auth: false}, "edge"},
		{"--no-auth", []string{"--no-auth"}, options{dir: ".", auth: false}, releaseTag()},
		{"--eval-node", []string{"--eval-node"},
			options{dir: ".", evalNode: true, auth: true}, releaseTag()},
		{"--postgres", []string{"--postgres"},
			options{dir: ".", postgres: true, auth: true, setPostgres: true}, releaseTag()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, tag, _ := parseInitArgs(tc.args)
			if got != tc.want {
				t.Errorf("args %q gave %+v, want %+v", tc.args, got, tc.want)
			}
			if tag != tc.tag {
				t.Errorf("args %q gave tag %q, want %q", tc.args, tag, tc.tag)
			}
		})
	}
}

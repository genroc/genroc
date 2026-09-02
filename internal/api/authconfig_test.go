package api

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The auth config file. specs/api-auth.md §2.4, §4; specs/auth-two-credentials.md.

func writeAuthConfig(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "auth.yaml")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// §2.4 refuses at LOAD, not at the first request: each of these is a way JWT deployments are
// broken, and a server that starts and then accepts forged tokens is the failure being avoided.
func TestLoadAuthConfig_RefusesAJWTConfigMissingAnyPin(t *testing.T) {
	const keys = "  jwks_file: /etc/genroc/jwks.json\n"

	cases := []struct {
		name string
		body string
		why  string
	}{
		{"no issuer", "mode: jwt\njwt:\n" + keys + "  audience: genroc\n  algorithms: [RS256]\n",
			"a second issuer with a valid JWKS would be accepted"},
		{"no audience", "mode: jwt\njwt:\n" + keys + "  issuer: https://i.test\n  algorithms: [RS256]\n",
			"a token minted for another app in the same tenant would verify here"},
		{"no algorithms", "mode: jwt\njwt:\n" + keys + "  issuer: https://i.test\n  audience: genroc\n",
			"`alg: none` and RS256/HS256 confusion would both be accepted"},
		{"algorithms containing none", "mode: jwt\njwt:\n" + keys +
			"  issuer: https://i.test\n  audience: genroc\n  algorithms: [RS256, none]\n",
			"`none` accepts unsigned tokens"},
		{"no key source", "mode: jwt\njwt:\n  issuer: https://i.test\n  audience: genroc\n  algorithms: [RS256]\n",
			"nothing could verify a signature"},
		{"both key sources", "mode: jwt\njwt:\n" + keys +
			"  jwks_url: https://i.test/jwks\n  issuer: https://i.test\n  audience: genroc\n  algorithms: [RS256]\n",
			"which one wins is not something an operator should have to guess"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if _, err := LoadAuthConfig(writeAuthConfig(t, c.body)); err == nil {
				t.Fatalf("loaded a jwt config with %s — %s", c.name, c.why)
			}
		})
	}

	cfg, err := LoadAuthConfig(writeAuthConfig(t, `mode: jwt
jwt:
  jwks_file: /etc/genroc/jwks.json
  issuer: https://accounts.example.com
  audience: genroc
  algorithms: [RS256]
  subject_claim: email
  roles_claim: groups
  leeway: 30s
roles:
  genroc-admins: [admin]
users:
  alice@example.com: [admin]
`))
	if err != nil {
		t.Fatalf("a complete jwt config was refused: %v", err)
	}
	if cfg.JWT.SubjectClaim != "email" || cfg.JWT.RolesClaim != "groups" || cfg.JWT.Leeway != "30s" {
		t.Fatalf("jwt block decoded as %+v", cfg.JWT)
	}
	if got := cfg.Users["alice@example.com"]; len(got) != 1 || got[0] != "admin" {
		t.Errorf("users decoded as %v; the YAML key must be `users`", cfg.Users)
	}
}

// A `mode: header` file is a real thing that exists on disk somewhere, so it must fail loudly
// and say what replaced it — not decode into an empty jwt block that authenticates nobody.
// specs/auth-two-credentials.md §1.
func TestLoadAuthConfig_RefusesTheRetiredHeaderMode(t *testing.T) {
	_, err := LoadAuthConfig(writeAuthConfig(t, `mode: header
header:
  subject: X-Auth-Request-Email
  trusted_proxies: [10.0.0.0/8]
`))
	if err == nil {
		t.Fatal("a header-mode config loaded; genroc reads no identity headers, so this file " +
			"would have started a server that silently authenticates nobody")
	}
	if !strings.Contains(err.Error(), "jwt") {
		t.Errorf("the error must name the replacement, got: %v", err)
	}
}

// An empty or absent mode is the same mistake by omission.
func TestLoadAuthConfig_RefusesAConfigWithNoMode(t *testing.T) {
	if _, err := LoadAuthConfig(writeAuthConfig(t, "roles:\n  admins: [admin]\n")); err == nil {
		t.Fatal("a config with no mode loaded")
	}
}

func TestGrantsFor_UnionsRolesSubjectAndTheWildcard(t *testing.T) {
	roles := map[string][]string{"genroc-admins": {"admin"}, "*": {"read"}}
	users := map[string][]string{"alice@example.com": {"deploy"}}

	got := grantsFor(roles, users, "alice@example.com", nil)
	if !hasPerm(got, PermDeploy) {
		t.Error("a subject entry did not grant; `users` is what serves a provider carrying no groups")
	}
	if !hasPerm(got, PermRead) {
		t.Error("the `*` rule did not apply to an authenticated caller")
	}
	if hasPerm(got, PermAdmin) {
		t.Error("a role the caller does not hold was granted")
	}

	withRole := grantsFor(roles, users, "bob@example.com", []string{"genroc-admins"})
	if !hasPerm(withRole, PermAdmin) {
		t.Error("a role the caller DOES hold was not granted")
	}
}

func hasPerm(gs []Grant, want Perm) bool {
	for _, g := range gs {
		if g.Perm == want {
			return true
		}
	}
	return false
}

package api

import (
	"net/http"
	"os"
	"path/filepath"
	"testing"
)

// Header mode. specs/api-auth.md §2.2, §4, §6.

func headerAuth(t *testing.T, cfg *AuthConfig) *HeaderAuth {
	t.Helper()
	h, err := NewHeaderAuth(cfg)
	if err != nil {
		t.Fatalf("NewHeaderAuth: %v", err)
	}
	return h
}

func forwarded(peer, subject, roles string) *http.Request {
	r := httptest("GET", "/api/instances")
	r.RemoteAddr = peer + ":40000"
	if subject != "" {
		r.Header.Set("X-Auth-Request-Email", subject)
	}
	if roles != "" {
		r.Header.Set("X-Auth-Request-Groups", roles)
	}
	return r
}

func httptest(method, path string) *http.Request {
	r, _ := http.NewRequest(method, "http://genroc.test"+path, nil)
	return r
}

func perms(p *Principal) []Perm {
	if p == nil {
		return nil
	}
	out := make([]Perm, 0, len(p.Grants))
	for _, g := range p.Grants {
		out = append(out, g.Perm)
	}
	return out
}

func has(p *Principal, want Perm) bool {
	for _, g := range perms(p) {
		if g == want {
			return true
		}
	}
	return false
}

func demoConfig() *AuthConfig {
	return &AuthConfig{
		Mode: "header",
		Header: HeaderModeConfig{
			Subject: "X-Auth-Request-Email", Roles: "X-Auth-Request-Groups",
			TrustedProxies: []string{"10.1.0.0/16", "192.168.5.7"},
		},
		Roles: map[string][]string{"genroc-admins": {"admin"}, "*": {"read"}},
		Users: map[string][]string{"alice@example.com": {"deploy"}},
	}
}

// The trust boundary. Everything else in this file assumes it holds.
func TestHeaderAuth_BelievesAForwardedIdentityOnlyFromATrustedPeer(t *testing.T) {
	h := headerAuth(t, demoConfig())
	for _, tc := range []struct {
		peer    string
		trusted bool
	}{
		{"10.1.2.3", true},
		{"192.168.5.7", true},  // bare address, accepted as /32
		{"192.168.5.8", false}, // one off it
		{"10.2.0.1", false},    // outside the CIDR
		{"127.0.0.1", false},   // loopback is not special
	} {
		got := h.PrincipalFrom(forwarded(tc.peer, "alice@example.com", "genroc-admins"))
		if tc.trusted && got == nil {
			t.Errorf("peer %s is inside trusted_proxies but its forwarded identity was ignored", tc.peer)
		}
		if !tc.trusted && got != nil {
			t.Errorf("peer %s is OUTSIDE trusted_proxies and was believed as %q with %v — "+
				"anyone who reaches this port can now assert any identity", tc.peer, got.Subject, perms(got))
		}
	}
}

// Why `users` exists: Dex's static-password connector, GitHub's OAuth2 provider and Google's ID
// token all forward an identity with no groups, so a role-only map can grant such a caller
// nothing beyond `*`.
func TestHeaderAuth_GrantsBySubjectWhenTheProxyForwardsNoRoles(t *testing.T) {
	h := headerAuth(t, demoConfig())

	p := h.PrincipalFrom(forwarded("10.1.2.3", "alice@example.com", ""))
	if p == nil {
		t.Fatal("a trusted peer forwarded a subject and got no principal")
	}
	if !has(p, PermDeploy) {
		t.Errorf("alice is named in `users` with deploy, got %v — an IdP that sends no groups "+
			"leaves the role map unable to grant anything", perms(p))
	}
	if !has(p, PermRead) {
		t.Errorf("`*` grants read to every authenticated caller, got %v", perms(p))
	}

	other := h.PrincipalFrom(forwarded("10.1.2.3", "mallory@example.com", ""))
	if has(other, PermDeploy) {
		t.Errorf("a subject absent from `users` picked up alice's grant: %v", perms(other))
	}
	if !has(other, PermRead) {
		t.Errorf("`*` should still apply to an unlisted subject, got %v", perms(other))
	}
}

// Roles and users are unioned, not alternatives — a listed operator who is ALSO in an admin
// group must not lose either half.
func TestHeaderAuth_UnionsRolesAndSubjectGrants(t *testing.T) {
	h := headerAuth(t, demoConfig())
	p := h.PrincipalFrom(forwarded("10.1.2.3", "alice@example.com", "genroc-admins"))
	if !has(p, PermAdmin) {
		t.Errorf("the genroc-admins role grants admin, got %v", perms(p))
	}
	if !has(p, PermDeploy) {
		t.Errorf("the users entry must survive alongside the role, got %v", perms(p))
	}
	seen := map[Perm]int{}
	for _, g := range perms(p) {
		seen[g]++
	}
	for g, n := range seen {
		if n > 1 {
			t.Errorf("%s granted %d times; grants must be deduplicated", g, n)
		}
	}
}

func TestHeaderAuth_ParsesRolesAndRefusesAnEmptySubject(t *testing.T) {
	h := headerAuth(t, demoConfig())

	p := h.PrincipalFrom(forwarded("10.1.2.3", "bob@example.com", " genroc-admins , , ops "))
	if len(p.Roles) != 2 || p.Roles[0] != "genroc-admins" || p.Roles[1] != "ops" {
		t.Errorf("roles = %q; want the comma list trimmed with blanks dropped", p.Roles)
	}

	if got := h.PrincipalFrom(forwarded("10.1.2.3", "", "genroc-admins")); got != nil {
		t.Errorf("a trusted peer forwarding roles but NO subject produced %v — a principal with "+
			"no identity would be granted admin by the role map alone", perms(got))
	}
	if got := h.PrincipalFrom(forwarded("10.1.2.3", "   ", "")); got != nil {
		t.Error("a whitespace-only subject must not authenticate")
	}
}

// The one guard that cannot be forgotten: header mode without trusted_proxies is a total bypass,
// so it must fail at load rather than at the first request.
func TestLoadAuthConfig_RefusesHeaderModeWithoutATrustBoundary(t *testing.T) {
	write := func(t *testing.T, body string) string {
		t.Helper()
		p := filepath.Join(t.TempDir(), "auth.yaml")
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}

	if _, err := LoadAuthConfig(write(t, "mode: header\nheader:\n  subject: X-User\n")); err == nil {
		t.Fatal("header mode loaded with no trusted_proxies — every caller can assert any identity")
	}
	if _, err := LoadAuthConfig(write(t, "mode: header\nheader:\n  trusted_proxies: [10.0.0.0/8]\n")); err == nil {
		t.Fatal("header mode loaded with no subject header, so nothing identifies the caller")
	}

	cfg, err := LoadAuthConfig(write(t, `mode: header
header:
  subject: X-Auth-Request-Email
  trusted_proxies: [10.0.0.0/8]
users:
  alice@example.com: [admin]
`))
	if err != nil {
		t.Fatalf("a complete header config was refused: %v", err)
	}
	if got := cfg.Users["alice@example.com"]; len(got) != 1 || got[0] != "admin" {
		t.Errorf("users decoded as %v; the YAML key must be `users`", cfg.Users)
	}
}

// §2.4 refuses at LOAD, not at the first request: each of these is a way JWT deployments are
// broken, and a server that starts and then accepts forged tokens is the failure being avoided.
func TestLoadAuthConfig_RefusesAJWTConfigMissingAnyPin(t *testing.T) {
	write := func(body string) string {
		t.Helper()
		p := filepath.Join(t.TempDir(), "auth.yaml")
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
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
		{"roles header with no trust boundary", "mode: jwt\njwt:\n" + keys +
			"  issuer: https://i.test\n  audience: genroc\n  algorithms: [RS256]\nheader:\n  roles: X-Groups\n",
			"a role list from an unbounded peer is a grant from one"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if _, err := LoadAuthConfig(write(c.body)); err == nil {
				t.Fatalf("loaded a jwt config with %s — %s", c.name, c.why)
			}
		})
	}

	cfg, err := LoadAuthConfig(write(`mode: jwt
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
`))
	if err != nil {
		t.Fatalf("a complete jwt config was refused: %v", err)
	}
	if cfg.JWT.SubjectClaim != "email" || cfg.JWT.RolesClaim != "groups" || cfg.JWT.Leeway != "30s" {
		t.Fatalf("jwt block decoded as %+v", cfg.JWT)
	}
}

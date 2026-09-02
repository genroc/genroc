package api

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// How jwt mode is configured. specs/api-auth.md §2.4; specs/ui-issued-tokens.md.

const testSecret = "a-shared-secret-long-enough-to-be-taken-seriously"

// Each of these is refused at STARTUP, not at the first request: a server that comes up and then
// accepts forged tokens is the failure being avoided.
func TestJWTConfig_RefusesAnythingUnusable(t *testing.T) {
	cases := []struct {
		name string
		cfg  JWTModeConfig
		why  string
	}{
		{"no secret at all", JWTModeConfig{Issuer: "genroc-ui", Audience: "genroc"},
			"nothing could verify a signature"},
		{"both secret and secret_file", JWTModeConfig{Secret: testSecret, SecretFile: "/etc/x"},
			"which one wins is not something an operator should have to guess"},
		{"a short secret", JWTModeConfig{Secret: "hunter2"},
			"HMAC is only as strong as its key, and forging one mints any identity"},
		{"an unreadable secret file", JWTModeConfig{SecretFile: "/nonexistent/key"},
			"a missing key must fail loudly, not start with none"},
		{"a bad leeway", JWTModeConfig{Secret: testSecret, Leeway: "soon"},
			"an unparseable duration is a typo, not a default"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cfg := c.cfg
			if err := cfg.Validate(); err == nil {
				t.Fatalf("accepted a config with %s — %s", c.name, c.why)
			}
		})
	}
}

// Both pins default to what genroc-ui uses, so a deployment running the pair as shipped
// configures neither — while still pinning them, which §2.4 is what requires.
func TestJWTConfig_DefaultsToTheIssuerAndAudienceGenrocUIUses(t *testing.T) {
	cfg := JWTModeConfig{Secret: testSecret}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("a secret alone should be enough: %v", err)
	}
	if cfg.Issuer != DefaultJWTIssuer || cfg.Audience != DefaultJWTAudience {
		t.Fatalf("defaults = %q/%q, want %q/%q", cfg.Issuer, cfg.Audience,
			DefaultJWTIssuer, DefaultJWTAudience)
	}

	explicit := JWTModeConfig{Secret: testSecret, Issuer: "mine", Audience: "yours"}
	if err := explicit.Validate(); err != nil {
		t.Fatal(err)
	}
	if explicit.Issuer != "mine" || explicit.Audience != "yours" {
		t.Error("an explicit value was overwritten by a default")
	}
}

// A secret delivered as a file — the k8s and /data shape — almost always arrives with a
// trailing newline. A mismatch on an invisible byte is the worst kind to debug.
func TestJWTConfig_ReadsASecretFromAFileAndTrimsIt(t *testing.T) {
	sf := filepath.Join(t.TempDir(), "secret")
	if err := os.WriteFile(sf, []byte(testSecret+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := JWTModeConfig{SecretFile: sf}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("secret_file was refused: %v", err)
	}
	got, err := cfg.resolveSecret()
	if err != nil {
		t.Fatal(err)
	}
	if got != testSecret {
		t.Fatalf("secret = %q; a trailing newline must not become part of the key", got)
	}
}

// The server holds no role map: a token's permissions are whatever its issuer put in it, and
// there is no configuration here that could add to them.
func TestJWTAuth_GrantsOnlyWhatTheTokenCarries(t *testing.T) {
	a, err := NewJWTAuth(JWTModeConfig{Secret: testSecret})
	if err != nil {
		t.Fatal(err)
	}
	tok := mintTestToken(t, testSecret, DefaultJWTIssuer, DefaultJWTAudience,
		"ada@example.com", nil, time.Hour)

	p, err := a.Authenticate(context.Background(), tok)
	if err != nil || p == nil {
		t.Fatalf("p=%v err=%v", p, err)
	}
	if len(p.Grants) != 0 {
		t.Fatalf("granted %v from a token carrying no perms; the map moved to genroc-ui and "+
			"nothing here can add to what a token says", p.Grants)
	}
	if !strings.HasPrefix(p.Actor(), "jwt:") {
		t.Errorf("Actor() = %q", p.Actor())
	}
}

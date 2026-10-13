package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeConfig(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	secret := filepath.Join(dir, "secret")
	if err := os.WriteFile(secret, []byte(strings.Repeat("k", 40)), 0o600); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "ui.yaml")
	body = "server: http://genroc:8448\ntoken:\n  secret_file: " + secret + "\n" + body
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// The entry guesses no issuer, and asks for `groups`: IdPs that have groups emit them only when
// asked, and their silent absence reads as a broken role map.
func TestProvider_NeedsAnIssuerAndAsksForGroups(t *testing.T) {
	_, err := LoadConfig(writeConfig(t, `
login:
  providers:
    - id: dex
      client_id: abc
      client_secret: shh
`))
	if err == nil || !strings.Contains(err.Error(), "needs an issuer") {
		t.Errorf("a provider with no issuer was accepted: %v", err)
	}
	if got := strings.Join((Provider{ID: "dex"}).scopes(), " "); got != "openid email profile groups" {
		t.Errorf("default scopes = %q", got)
	}
	if got := (Provider{Scopes: []string{"openid", "email"}}).scopes(); len(got) != 2 {
		t.Errorf("explicit scopes were replaced: %v; Google refuses `groups`, so they must win", got)
	}
}

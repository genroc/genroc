package main

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// A `type` encodes what an IdP needs that a generic entry cannot guess, so an operator does not
// have to know that Google publishes no `groups` scope and no groups claim.

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

func TestProviderType_GoogleSuppliesWhatGoogleNeeds(t *testing.T) {
	cfg, err := LoadConfig(writeConfig(t, `
login:
  providers:
    - id: google
      type: google
      client_id: abc
      client_secret: shh
`))
	if err != nil {
		t.Fatal(err)
	}
	p := cfg.Login.Providers[0]
	if p.Issuer != "https://accounts.google.com" {
		t.Errorf("issuer = %q; the type exists so it need not be typed out", p.Issuer)
	}
	if p.SubjectClaim != "email" {
		t.Errorf("subject_claim = %q, want email", p.SubjectClaim)
	}
	got := p.scopes()
	// The bare `groups` scope breaks the login outright: Google's discovery document does not
	// publish it, and it refuses a scope it does not publish.
	for _, s := range got {
		if s == "groups" {
			t.Errorf("scopes = %v; Google refuses `groups`, which the generic default asks for", got)
		}
	}
	// And the one that makes membership reachable at all, since the ID token carries none.
	if !slices.Contains(got, googleGroupsScope) {
		t.Errorf("scopes = %v; without %s there is no way to read Workspace groups", got, googleGroupsScope)
	}
}

// A type is a set of defaults, not a cage.
func TestProviderType_ExplicitValuesWin(t *testing.T) {
	cfg, err := LoadConfig(writeConfig(t, `
login:
  providers:
    - id: google
      type: google
      issuer: https://accounts.google.com
      subject_claim: sub
      scopes: [openid, email]
      client_id: abc
      client_secret: shh
`))
	if err != nil {
		t.Fatal(err)
	}
	p := cfg.Login.Providers[0]
	if p.SubjectClaim != "sub" || strings.Join(p.scopes(), " ") != "openid email" {
		t.Errorf("the type overrode what was written down: %+v / %v", p.SubjectClaim, p.scopes())
	}
}

// Refused at load, because it can never match at runtime: the ID token has no such claim, so the
// symptom would be every Google user landing on the `*` rule and wondering why.
func TestProviderType_GoogleRefusesAGroupsClaim(t *testing.T) {
	_, err := LoadConfig(writeConfig(t, `
login:
  providers:
    - id: google
      type: google
      groups_claim: groups
      client_id: abc
      client_secret: shh
`))
	if err == nil {
		t.Fatal("a groups_claim on Google was accepted; it can never match")
	}
	if !strings.Contains(err.Error(), "never arrives") {
		t.Errorf("error does not say why: %v", err)
	}
}

func TestProviderType_UnknownTypeIsRefusedWithTheKnownOnes(t *testing.T) {
	_, err := LoadConfig(writeConfig(t, `
login:
  providers:
    - id: x
      type: gogle
      client_id: abc
      client_secret: shh
`))
	if err == nil || !strings.Contains(err.Error(), "google") {
		t.Errorf("a typo'd type must name the ones that exist; got %v", err)
	}
}

// The generic entry is unchanged: it guesses no issuer, and still asks for `groups`, which is
// the scope IdPs that HAVE groups only honour when asked.
func TestProviderType_GenericStillNeedsAnIssuerAndAsksForGroups(t *testing.T) {
	_, err := LoadConfig(writeConfig(t, `
login:
  providers:
    - id: dex
      client_id: abc
      client_secret: shh
`))
	if err == nil || !strings.Contains(err.Error(), "needs an issuer") {
		t.Errorf("a generic provider with no issuer was accepted: %v", err)
	}
	p := Provider{ID: "dex"}
	if strings.Join(p.scopes(), " ") != "openid email profile groups" {
		t.Errorf("generic scopes = %v", p.scopes())
	}
}

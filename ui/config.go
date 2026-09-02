package main

import (
	"fmt"
	"os"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// genroc-ui's configuration. specs/ui-issued-tokens.md §5.
//
// A file rather than flags because the shape is not flat: several providers, a role map, and a
// user list are all lists and maps, and an env var per field stops being expressible at the
// second provider.

type Config struct {
	// Server is the genroc API to proxy to.
	Server string `yaml:"server"`
	Listen string `yaml:"listen"`
	// RedirectURL pins the callback a provider sends the browser back to. Optional: it is
	// derived from the request otherwise, so a deployment states its address once — at the
	// provider, where it has to be registered anyway. Set it behind a proxy that rewrites Host.
	RedirectURL string `yaml:"redirect_url"`
	// SecureCookie forces the Secure attribute on or off. Optional: it follows the request's
	// scheme otherwise, which is right in both directions and cannot be set wrong.
	SecureCookie *bool  `yaml:"secure_cookie"`
	SessionTTL   string `yaml:"session_ttl"`

	Login Login `yaml:"login"`
	Token Token `yaml:"token"`

	// Roles maps a group asserted by a provider to permissions; `*` applies to anyone who
	// logged in. Users maps a subject, for providers that carry no groups at all.
	//
	// This is the map that used to live in the genroc server. It is here because the token this
	// component mints carries PERMISSIONS, so the resolution has to happen before signing.
	// specs/ui-issued-tokens.md §1.
	Roles map[string][]string `yaml:"roles"`
	Users map[string][]string `yaml:"users"`
}

type Login struct {
	Providers []Provider `yaml:"providers"`
	Passwords []Password `yaml:"passwords"`
}

type Provider struct {
	ID     string `yaml:"id"`   // stable, used in URLs and the state cookie
	Name   string `yaml:"name"` // what the button says
	Issuer string `yaml:"issuer"`
	// DiscoveryURL, TokenURL and JWKSURL override the discovered endpoints, for an issuer whose
	// URL resolves differently from the browser than from this process -- an IdP in Docker, or
	// split-horizon DNS. Issuer stays the front-channel value, because that is what `iss` says.
	DiscoveryURL string   `yaml:"discovery_url"`
	TokenURL     string   `yaml:"token_url"`
	JWKSURL      string   `yaml:"jwks_url"`
	ClientID     string   `yaml:"client_id"`
	ClientSecret string   `yaml:"client_secret"`
	Scopes       []string `yaml:"scopes"`
	// SubjectClaim and GroupsClaim default to `email` and `groups`.
	SubjectClaim string `yaml:"subject_claim"`
	GroupsClaim  string `yaml:"groups_claim"`
}

// Password is a local login. It is the `staticPasswords` trade -- one file, no directory, no
// registration, no reset -- and it must not grow past that. specs/ui-issued-tokens.md §5.
type Password struct {
	Email  string   `yaml:"email"`
	Hash   string   `yaml:"hash"` // bcrypt, never plaintext
	Groups []string `yaml:"groups"`
}

// Token is what this component mints and the genroc server verifies. Issuer and Audience must
// match the server's auth config exactly, and Secret must be the same secret.
type Token struct {
	Issuer     string `yaml:"issuer"`
	Audience   string `yaml:"audience"`
	Secret     string `yaml:"secret"`
	SecretFile string `yaml:"secret_file"`
	TTL        string `yaml:"ttl"`
}

const (
	// The genroc server defaults to these too (api.DefaultJWTIssuer / DefaultJWTAudience), which
	// is what lets the pair run as shipped with no issuer or audience configured anywhere. They
	// are duplicated rather than imported because this module deliberately does not depend on
	// the server's. specs/ui-component.md §1.
	defaultTokenIssuer   = "genroc-ui"
	defaultTokenAudience = "genroc"

	defaultSessionTTL = 12 * time.Hour
	defaultTokenTTL   = time.Minute
	// minSecretBytes mirrors the server's floor. HMAC is only as strong as its key, and forging
	// a token here mints any identity with any permissions.
	minSecretBytes = 32
)

// LoadConfig reads and validates. Everything that can be refused here is refused here: a
// misconfigured login that starts and then fails at the callback is the outcome being avoided.
func LoadConfig(path string) (*Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var c Config
	if err := yaml.Unmarshal(raw, &c); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if c.Server == "" {
		return nil, fmt.Errorf("%s: server is required — the genroc API to proxy to", path)
	}
	if c.Listen == "" {
		c.Listen = ":8448"
	}
	if !c.loginConfigured() {
		// A UI with no way to log in is legitimate: it proxies as requests arrive, which is the
		// laptop shape against a server running -auth none or a pasted token.
		return &c, nil
	}

	seen := map[string]bool{}
	for i, p := range c.Login.Providers {
		switch {
		case p.ID == "":
			return nil, fmt.Errorf("%s: login.providers[%d] needs an id", path, i)
		case seen[p.ID]:
			return nil, fmt.Errorf("%s: two providers share the id %q; it addresses them in URLs "+
				"and in the state cookie, so it has to be unique", path, p.ID)
		case p.Issuer == "":
			return nil, fmt.Errorf("%s: provider %q needs an issuer", path, p.ID)
		case p.ClientID == "" || p.ClientSecret == "":
			return nil, fmt.Errorf("%s: provider %q needs client_id and client_secret", path, p.ID)
		}
		seen[p.ID] = true
	}
	for i, u := range c.Login.Passwords {
		if u.Email == "" || u.Hash == "" {
			return nil, fmt.Errorf("%s: login.passwords[%d] needs email and hash", path, i)
		}
		if !strings.HasPrefix(u.Hash, "$2") {
			return nil, fmt.Errorf("%s: login.passwords[%d] hash is not bcrypt; a plaintext "+
				"password in a config file is not a password", path, i)
		}
	}
	if _, err := c.Token.resolveSecret(path); err != nil {
		return nil, err
	}
	// Defaults matching the server's, so the pair agrees with neither side configuring them.
	// Set one and you must set it on both: they are what the server pins.
	if c.Token.Issuer == "" {
		c.Token.Issuer = defaultTokenIssuer
	}
	if c.Token.Audience == "" {
		c.Token.Audience = defaultTokenAudience
	}
	return &c, nil
}

func (c *Config) loginConfigured() bool {
	return len(c.Login.Providers) > 0 || len(c.Login.Passwords) > 0
}

func (t Token) resolveSecret(path string) (string, error) {
	switch {
	case t.Secret != "" && t.SecretFile != "":
		return "", fmt.Errorf("%s: token takes secret or secret_file, not both", path)
	case t.Secret == "" && t.SecretFile == "":
		return "", fmt.Errorf("%s: token.secret or token.secret_file is required — the key the "+
			"genroc server verifies against", path)
	}
	secret := t.Secret
	if t.SecretFile != "" {
		raw, err := os.ReadFile(t.SecretFile)
		if err != nil {
			return "", fmt.Errorf("%s: token.secret_file: %w", path, err)
		}
		// Trimmed for the same reason the server trims: a secret delivered as a file arrives
		// with a trailing newline, and a mismatch on an invisible byte is the worst to debug.
		secret = strings.TrimSpace(string(raw))
	}
	if len(secret) < minSecretBytes {
		return "", fmt.Errorf("%s: the token secret must be at least %d characters; got %d",
			path, minSecretBytes, len(secret))
	}
	return secret, nil
}

func (c *Config) sessionTTL() (time.Duration, error) { return dur(c.SessionTTL, defaultSessionTTL) }
func (c *Config) tokenTTL() (time.Duration, error)   { return dur(c.Token.TTL, defaultTokenTTL) }

func dur(s string, def time.Duration) (time.Duration, error) {
	if s == "" {
		return def, nil
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return 0, err
	}
	if d <= 0 {
		return 0, fmt.Errorf("must be positive; got %q", s)
	}
	return d, nil
}

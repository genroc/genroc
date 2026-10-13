package main

import (
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// genroc-ui's configuration (specs/ui-issued-tokens.md §5): a file, not flags, because
// providers, roles and users are lists and maps.

// The `description`, `default` and `required` tags are the published reference (configSchema):
// write them for whoever edits ui.yaml.
type Config struct {
	Server       string `yaml:"server" required:"true" description:"The genroc API to proxy to, e.g. http://genroc:8448."`
	Listen       string `yaml:"listen" default:":8448" description:"Address to listen on. --http overrides it."`
	RedirectURL  string `yaml:"redirect_url" description:"The full URL of /auth/callback that providers redirect back to. Derived from the request when omitted; set it behind a proxy that rewrites Host."`
	SecureCookie *bool  `yaml:"secure_cookie" description:"Forces the Secure attribute of the session cookie on or off. Omitted, it follows the request's scheme."`
	SessionTTL   string `yaml:"session_ttl" default:"12h" description:"How long a sign-in lasts, as a duration (\"30m\", \"12h\")."`

	Login Login `yaml:"login" description:"How people sign in. With neither providers nor passwords there is no login, and requests are proxied as they arrive."`
	Token Token `yaml:"token" description:"The token minted for genroc. Required once login is configured."`

	// Resolved here, before signing, because the token carries PERMISSIONS
	// (specs/ui-issued-tokens.md §1).
	Roles map[string][]string `yaml:"roles" description:"Group -> permissions. The key \"*\" matches anyone signed in."`
	Users map[string][]string `yaml:"users" description:"Subject -> permissions, for a provider whose tokens carry no groups. Unioned with roles."`
}

type Login struct {
	Providers []Provider `yaml:"providers" description:"OpenID Connect providers, one sign-in button each."`
	Passwords []Password `yaml:"passwords" description:"Email and password logins kept in this file. genctl init password prints an entry."`
}

type Provider struct {
	ID   string `yaml:"id" required:"true" description:"Stable and unique; it appears in URLs and in the state cookie."`
	Name string `yaml:"name" description:"What the sign-in button says."`
	// A type encodes VERIFIED quirks of a named IdP rather than documenting them (providerTypes).
	Type   string `yaml:"type" default:"oidc" description:"google, or oidc for a provider configured by hand. A type fills in issuer, scopes and subject_claim; anything set explicitly wins."`
	Issuer string `yaml:"issuer" description:"The OIDC issuer URL, exactly as the provider's tokens name it. Required unless type supplies it."`
	// Issuer stays the front-channel value under these overrides, because that is what `iss` says.
	DiscoveryURL string   `yaml:"discovery_url" description:"Overrides where the discovery document is fetched, for an issuer this process reaches at a different address than the browser does (an IdP in Docker, split-horizon DNS)."`
	TokenURL     string   `yaml:"token_url" description:"Overrides the discovered token endpoint, as discovery_url."`
	JWKSURL      string   `yaml:"jwks_url" description:"Overrides the discovered key set, as discovery_url."`
	ClientID     string   `yaml:"client_id" required:"true" description:"The OAuth client registered with the provider."`
	ClientSecret string   `yaml:"client_secret" required:"true" description:"That client's secret."`
	Scopes       []string `yaml:"scopes" description:"Scopes requested at login. Defaults to the type's, else openid, email, profile and groups."`
	SubjectClaim string   `yaml:"subject_claim" default:"email" description:"ID-token claim naming the person: the token's subject, and the key of users."`
	GroupsClaim  string   `yaml:"groups_claim" default:"groups" description:"ID-token claim listing the person's groups, matched against roles. Refused for google, whose groups are read from its API."`
}

// Password is a local login. It is the `staticPasswords` trade -- one file, no directory, no
// registration, no reset -- and it must not grow past that. specs/ui-issued-tokens.md §5.
type Password struct {
	Email  string   `yaml:"email" required:"true" description:"The sign-in email, and the token's subject."`
	Hash   string   `yaml:"hash" required:"true" description:"bcrypt hash of the password; plaintext is refused."`
	Groups []string `yaml:"groups" description:"Groups, matched against roles."`
}

type Token struct {
	Issuer     string `yaml:"issuer" default:"genroc-ui" description:"The iss of minted tokens. Must match genroc's --jwt-issuer."`
	Audience   string `yaml:"audience" default:"genroc" description:"The aud of minted tokens. Must match genroc's --jwt-audience."`
	Secret     string `yaml:"secret" description:"The signing secret shared with genroc, at least 32 characters. Give this or secret_file."`
	SecretFile string `yaml:"secret_file" description:"A file holding the secret, which must be the one genroc reads with --jwt-secret-file. Surrounding whitespace is trimmed."`
	TTL        string `yaml:"ttl" default:"1m" description:"Lifetime of a token minted for one request to genroc."`
}

const (
	// Match the server's defaults (api.DefaultJWTIssuer / DefaultJWTAudience) so the pair runs
	// unconfigured; duplicated, not imported, since this module must not depend on the server's.
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
		// laptop shape against a server without --auth, or a pasted token.
		return &c, nil
	}

	seen := map[string]bool{}
	for i := range c.Login.Providers {
		if err := c.Login.Providers[i].applyType(path); err != nil {
			return nil, err
		}
		p := c.Login.Providers[i]
		switch {
		case p.ID == "":
			return nil, fmt.Errorf("%s: login.providers[%d] needs an id", path, i)
		case seen[p.ID]:
			return nil, fmt.Errorf("%s: two providers share the id %q; it addresses them in URLs "+
				"and in the state cookie, so it has to be unique", path, p.ID)
		case p.Issuer == "":
			return nil, fmt.Errorf("%s: provider %q needs an issuer, or a `type` that supplies "+
				"one (%s)", path, p.ID, knownProviderTypes())
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

// A `type` holds only VERIFIED quirks of a named IdP. Google publishes no groups claim and
// refuses a `groups` scope, so its membership is fetched afterwards (google.go).
type providerType struct {
	issuer       string
	scopes       []string
	subjectClaim string
	// groupsFetched: the ID token carries no groups and the type fetches them, so `groups_claim`
	// is refused on such a provider.
	groupsFetched bool
}

var providerTypes = map[string]providerType{
	"google": {
		issuer: "https://accounts.google.com",
		// The groups scope is requested at login because the membership call is made with the
		// person's OWN access token -- no service account, no stored Google credential.
		scopes:        []string{"openid", "email", "profile", googleGroupsScope},
		subjectClaim:  "email",
		groupsFetched: true,
	},
}

// `groups` is in the generic set because IdPs with groups emit them only when asked, and their
// silent absence reads as a broken role map.
var defaultScopes = []string{"openid", "email", "profile", "groups"}

// applyType fills in what the type knows and refuses what it knows cannot work. Anything set
// explicitly wins, so a type is a set of defaults rather than a cage.
func (p *Provider) applyType(path string) error {
	if p.Type == "" || p.Type == "oidc" {
		return nil
	}
	t, ok := providerTypes[p.Type]
	if !ok {
		return fmt.Errorf("%s: provider %q has unknown type %q; known types are %s (omit it, or "+
			"use `oidc`, for a provider configured by hand)", path, p.ID, p.Type, knownProviderTypes())
	}
	if t.groupsFetched && p.GroupsClaim != "" {
		return fmt.Errorf("%s: provider %q is %s, whose ID token carries no groups — genroc-ui "+
			"reads them from the provider's API instead, so `groups_claim` names a claim that "+
			"never arrives", path, p.ID, p.Type)
	}
	if p.Issuer == "" {
		p.Issuer = t.issuer
	}
	if p.SubjectClaim == "" {
		p.SubjectClaim = t.subjectClaim
	}
	return nil
}

// scopes is what this provider is asked for: its own, else its type's, else the generic set.
func (p Provider) scopes() []string {
	if len(p.Scopes) > 0 {
		return p.Scopes
	}
	if t, ok := providerTypes[p.Type]; ok {
		return t.scopes
	}
	return defaultScopes
}

func knownProviderTypes() string {
	names := make([]string, 0, len(providerTypes))
	for n := range providerTypes {
		names = append(names, n)
	}
	sort.Strings(names)
	return strings.Join(names, ", ")
}

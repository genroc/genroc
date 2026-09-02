package oidc

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"genroc/ui/jwks"
)

// The relying-party half of OIDC: discovery, the authorization-code exchange, and verifying an
// ID token. This lives under ui/ and not in internal/ because the genroc SERVER has no use for
// it -- it verifies tokens, it never obtains them. specs/ui-component.md.
//
// It RELAYS and never issues: there is no signing key here and no token minted. A provider that
// cannot produce an ID token needs a broker in front (ui-component.md §5.1).

// Provider is a discovered OIDC issuer plus the client credentials to talk to it.
type Provider struct {
	Issuer   string
	AuthURL  string
	TokenURL string
	keys     *jwks.KeySet

	ClientID     string
	ClientSecret string
	Scopes       []string
}

type discovery struct {
	Issuer   string `json:"issuer"`
	AuthURL  string `json:"authorization_endpoint"`
	TokenURL string `json:"token_endpoint"`
	JWKSURI  string `json:"jwks_uri"`
}

// Config is what a deployment tells this component about its provider.
//
// Issuer is the only required endpoint field, and the three overrides exist for one real shape:
// an issuer whose URL resolves differently from the browser than from this process. That is the
// normal case for an IdP in Docker or behind split-horizon DNS -- the browser must reach a
// front-channel address for the login form, while token exchange and key fetching happen from
// here. Discovery publishes ONE set of URLs and cannot satisfy both, so the back-channel ones
// are nameable. Issuer stays the front-channel value, because that is what `iss` carries and
// what every token is validated against.
type Config struct {
	Issuer       string // pinned; what `iss` must say
	DiscoveryURL string // where to FETCH the document; defaults to Issuer + /.well-known/...
	TokenURL     string // override the discovered token endpoint
	JWKSURL      string // override the discovered JWKS endpoint
	ClientID     string
	ClientSecret string
	Scopes       []string
}

// Discover reads the issuer's well-known document.
//
// The `issuer` it advertises is checked against the one configured, because discovery is fetched
// over the network and a document that renames its own issuer would quietly move the value every
// later token is validated against. That check is why DiscoveryURL is separate rather than the
// issuer simply being rewritten: the document is fetched from wherever it lives, and still has
// to name the issuer we pinned.
func Discover(ctx context.Context, cfg Config) (*Provider, error) {
	issuer, clientID, clientSecret, scopes := cfg.Issuer, cfg.ClientID, cfg.ClientSecret, cfg.Scopes
	u := cfg.DiscoveryURL
	if u == "" {
		u = strings.TrimSuffix(issuer, "/") + "/.well-known/openid-configuration"
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	resp, err := (&http.Client{Timeout: jwks.FetchTimeout}).Do(req)
	if err != nil {
		return nil, fmt.Errorf("discover %s: %w", issuer, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("discover %s: %s", issuer, resp.Status)
	}
	var d discovery
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&d); err != nil {
		return nil, fmt.Errorf("discover %s: %w", issuer, err)
	}
	if d.Issuer != issuer {
		return nil, fmt.Errorf("discover %s: document declares issuer %q; the two must match or "+
			"tokens are validated against a value the network chose", issuer, d.Issuer)
	}
	if d.AuthURL == "" || d.TokenURL == "" || d.JWKSURI == "" {
		return nil, fmt.Errorf("discover %s: incomplete document", issuer)
	}
	tokenURL, jwksURI := d.TokenURL, d.JWKSURI
	if cfg.TokenURL != "" {
		tokenURL = cfg.TokenURL
	}
	if cfg.JWKSURL != "" {
		jwksURI = cfg.JWKSURL
	}
	return &Provider{
		Issuer: d.Issuer, AuthURL: d.AuthURL, TokenURL: tokenURL,
		keys:         jwks.NewKeySet(jwksURI, ""),
		ClientID:     clientID,
		ClientSecret: clientSecret,
		Scopes:       scopes,
	}, nil
}

// AuthCodeURL is where the browser is sent to log in.
func (p *Provider) AuthCodeURL(redirectURI, state, nonce string) string {
	q := url.Values{
		"client_id":     {p.ClientID},
		"redirect_uri":  {redirectURI},
		"response_type": {"code"},
		"scope":         {strings.Join(p.Scopes, " ")},
		"state":         {state},
		"nonce":         {nonce},
	}
	sep := "?"
	if strings.Contains(p.AuthURL, "?") {
		sep = "&"
	}
	return p.AuthURL + sep + q.Encode()
}

// Exchange trades an authorization code for the ID token, and verifies it before returning.
//
// Verified HERE rather than trusted because it was fetched over TLS: the token is about to be
// stored in a cookie and replayed on every later request, and a nonce that is not checked at
// exactly this moment can never be checked at all.
func (p *Provider) Exchange(ctx context.Context, code, redirectURI, nonce string) (string, error) {
	form := url.Values{
		"grant_type":   {"authorization_code"},
		"code":         {code},
		"redirect_uri": {redirectURI},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.TokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	// Client secret in the Authorization header rather than the body: a confidential client is
	// what genroc-ui gets to be by virtue of being a server, and basic auth is the form every
	// provider accepts.
	req.SetBasicAuth(url.QueryEscape(p.ClientID), url.QueryEscape(p.ClientSecret))

	resp, err := (&http.Client{Timeout: jwks.FetchTimeout}).Do(req)
	if err != nil {
		return "", fmt.Errorf("token exchange: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("token exchange: %s: %s", resp.Status, strings.TrimSpace(string(body)))
	}
	var out struct {
		IDToken string `json:"id_token"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return "", fmt.Errorf("token exchange: %w", err)
	}
	if out.IDToken == "" {
		return "", fmt.Errorf("token exchange: no id_token in the response — is this an OIDC " +
			"provider, or OAuth2 only?")
	}
	if _, err := p.verify(ctx, out.IDToken, nonce); err != nil {
		return "", err
	}
	return out.IDToken, nil
}

// Claims is the little genroc-ui needs from a verified token: who they are and which groups the
// provider says they are in. That is the whole contract with an upstream provider -- everything
// after it (groups to permissions, minting) is ours. specs/ui-issued-tokens.md §0.
type Claims struct {
	Subject string
	Groups  []string
	Expiry  time.Time
}

// Claims verifies a token and reads the identity out of it, using the claim names this provider
// was configured with. Defaults are `email` and `groups`: an OIDC `sub` is an opaque, provider-
// specific id, which is the wrong thing to put in a role map or an audit trail.
func (p *Provider) Claims(ctx context.Context, raw, nonce, subjectClaim, groupsClaim string) (*Claims, error) {
	c, err := p.verify(ctx, raw, nonce)
	if err != nil {
		return nil, err
	}
	if subjectClaim == "" {
		subjectClaim = "email"
	}
	if groupsClaim == "" {
		groupsClaim = "groups"
	}
	sub, _ := c.raw[subjectClaim].(string)
	if sub = strings.TrimSpace(sub); sub == "" {
		return nil, fmt.Errorf("the token carries no %q claim to identify the caller by", subjectClaim)
	}
	return &Claims{Subject: sub, Groups: claimStrings(c.raw[groupsClaim]), Expiry: c.Expiry}, nil
}

// claimStrings accepts both shapes providers emit: a JSON array, and a single space- or
// comma-separated string.
func claimStrings(v any) []string {
	switch t := v.(type) {
	case []any:
		out := make([]string, 0, len(t))
		for _, x := range t {
			if s, ok := x.(string); ok {
				if s = strings.TrimSpace(s); s != "" {
					out = append(out, s)
				}
			}
		}
		return out
	case string:
		var out []string
		for _, f := range strings.FieldsFunc(t, func(r rune) bool { return r == ',' || r == ' ' }) {
			if f = strings.TrimSpace(f); f != "" {
				out = append(out, f)
			}
		}
		return out
	}
	return nil
}

type verified struct {
	Expiry time.Time
	raw    jwt.MapClaims
}

// Verify checks the signature, issuer, audience and expiry of an ID token. When nonce is
// non-empty it must match the claim, which is what binds a token to the login that requested it.
//
// genroc-ui verifies even though the genroc server will verify again. The duplication is not
// belt-and-braces: without it a tampered cookie claiming a future expiry would be attached to
// every request, refused by the server, and drive the browser into a redirect loop that looks
// like a broken login rather than a bad cookie.
func (p *Provider) verify(ctx context.Context, raw, nonce string) (*verified, error) {
	claims := jwt.MapClaims{}
	parser := jwt.NewParser(
		jwt.WithIssuer(p.Issuer),
		jwt.WithAudience(p.ClientID),
		jwt.WithExpirationRequired(),
		jwt.WithLeeway(30*time.Second),
	)
	if _, err := parser.ParseWithClaims(raw, claims, func(t *jwt.Token) (any, error) {
		kid, _ := t.Header["kid"].(string)
		return p.keys.KeyFor(ctx, kid)
	}); err != nil {
		return nil, err
	}
	if nonce != "" {
		if got, _ := claims["nonce"].(string); got != nonce {
			return nil, fmt.Errorf("nonce mismatch: this token belongs to a different login")
		}
	}

	exp, err := claims.GetExpirationTime()
	if err != nil || exp == nil {
		return nil, fmt.Errorf("token has no usable expiry")
	}
	return &verified{Expiry: exp.Time, raw: claims}, nil
}

// RandomState returns an unguessable value for `state` and `nonce`.
func RandomState() (string, error) {
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

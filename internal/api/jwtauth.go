package api

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// JWTAuth is `mode: jwt` -- a signed token the deployment's IdP or proxy minted, verified
// against a configured JWKS. specs/api-auth.md §2.1.
//
// This is the mode that removes §6's bypass hazard rather than documenting it: the guarantee
// lives in the request, so a caller that reaches genroc past the proxy still has to produce a
// signature it cannot forge. `header` mode's trust rests on a network fact genroc cannot see.
type JWTAuth struct {
	keys   *keySet
	cfg    JWTModeConfig
	roles  map[string][]string
	users  map[string][]string
	parser *jwt.Parser
	leeway time.Duration
}

func NewJWTAuth(cfg *AuthConfig) (*JWTAuth, error) {
	j := cfg.JWT
	leeway, err := parseLeeway(j.Leeway)
	if err != nil {
		return nil, err
	}
	// Every one of these is a §2.4 validation, and they are options on the parser rather than
	// checks written here so that there is no path which parses without them.
	opts := []jwt.ParserOption{
		jwt.WithValidMethods(j.Algorithms),
		jwt.WithIssuer(j.Issuer),
		jwt.WithAudience(j.Audience),
		jwt.WithLeeway(leeway),
		jwt.WithExpirationRequired(),
	}
	return &JWTAuth{
		keys:   newKeySet(j.JWKSURL, j.JWKSFile),
		cfg:    j,
		roles:  cfg.Roles,
		users:  cfg.Users,
		parser: jwt.NewParser(opts...),
		leeway: leeway,
	}, nil
}

// Authenticate verifies a bearer token and resolves it to a Principal.
//
// A credential that is not a JWT at all returns (nil, nil) rather than an error: a deployment
// runs `jwt` beside `token` (§2), and a `genroc_sk_*` presented here is simply not this mode's
// to answer. A token that IS a JWT but fails verification also returns (nil, nil) -- it did not
// authenticate, and authorize turns that into 401. Only a failure to DECIDE is an error.
func (a *JWTAuth) Authenticate(ctx context.Context, credential string) (*Principal, error) {
	if credential == "" || strings.HasPrefix(credential, "genroc_sk_") {
		return nil, nil
	}
	// Three dots-separated segments is the cheapest way to not treat an opaque credential as a
	// malformed JWT, which would otherwise log noise for every machine caller.
	if strings.Count(credential, ".") != 2 {
		return nil, nil
	}

	claims := jwt.MapClaims{}
	_, err := a.parser.ParseWithClaims(credential, claims, func(t *jwt.Token) (any, error) {
		kid, _ := t.Header["kid"].(string)
		return a.keys.keyFor(ctx, kid)
	})
	if err != nil {
		return nil, nil
	}

	subject := claimString(claims, a.cfg.SubjectClaim)
	if subject == "" {
		// Verified but unusable: without a subject there is nothing to map to permissions and
		// nothing to record as the actor.
		return nil, nil
	}
	roles := claimStrings(claims, a.cfg.RolesClaim)
	return &Principal{
		Subject: subject,
		Roles:   roles,
		Grants:  grantsFor(a.roles, a.users, subject, roles),
		Source:  "jwt",
	}, nil
}

// SubjectClaim / RolesClaim default to `sub` and `groups` when unset.
func claimString(c jwt.MapClaims, key string) string {
	if key == "" {
		key = "sub"
	}
	s, _ := c[key].(string)
	return strings.TrimSpace(s)
}

// claimStrings reads a roles claim, accepting both shapes IdPs actually emit: a JSON array, and
// a single space- or comma-separated string. Refusing the second would fail against real
// issuers for no gain.
func claimStrings(c jwt.MapClaims, key string) []string {
	if key == "" {
		key = "groups"
	}
	switch v := c[key].(type) {
	case []any:
		out := make([]string, 0, len(v))
		for _, x := range v {
			if s, ok := x.(string); ok {
				if s = strings.TrimSpace(s); s != "" {
					out = append(out, s)
				}
			}
		}
		return out
	case []string:
		return v
	case string:
		return splitList(v)
	}
	return nil
}

func splitList(s string) []string {
	var out []string
	for _, f := range strings.FieldsFunc(s, func(r rune) bool { return r == ',' || r == ' ' }) {
		if f = strings.TrimSpace(f); f != "" {
			out = append(out, f)
		}
	}
	return out
}

func parseLeeway(s string) (time.Duration, error) {
	if s == "" {
		// Not zero: a fixed zero fails on real clusters, where clocks disagree by seconds.
		// specs/api-auth.md §2.4.
		return 30 * time.Second, nil
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return 0, fmt.Errorf("jwt.leeway %q: %w", s, err)
	}
	if d < 0 {
		return 0, fmt.Errorf("jwt.leeway must not be negative; got %q", s)
	}
	return d, nil
}

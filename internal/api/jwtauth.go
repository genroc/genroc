package api

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// JWTAuth is `mode: jwt`: it verifies a token signed with a shared secret and RESOLVES
// NOTHING -- the token carries its issuer's permissions. specs/ui-issued-tokens.md §1.
type JWTAuth struct {
	cfg    JWTModeConfig
	secret []byte
	parser *jwt.Parser
}

// permsClaim carries the resolved permission set. Scoped by the pinned issuer and audience
// rather than by a namespaced name -- a token from anywhere else fails before this is read.
const permsClaim = "perms"

func NewJWTAuth(cfg JWTModeConfig) (*JWTAuth, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	secret, _ := cfg.resolveSecret()
	leeway, _ := parseLeeway(cfg.Leeway)
	// §2.4's validations, as parser OPTIONS so no path verifies without them. HS256 only.
	return &JWTAuth{
		cfg:    cfg,
		secret: []byte(secret),
		parser: jwt.NewParser(
			jwt.WithValidMethods([]string{jwt.SigningMethodHS256.Alg()}),
			jwt.WithIssuer(cfg.Issuer),
			jwt.WithAudience(cfg.Audience),
			jwt.WithLeeway(leeway),
			jwt.WithExpirationRequired(),
		),
	}, nil
}

// Authenticate returns (nil, nil), not an error, for a non-JWT and for a JWT that fails
// verification, since it runs beside `token` mode. Only a failure to DECIDE is an error.
func (a *JWTAuth) Authenticate(ctx context.Context, credential string) (*Principal, error) {
	if credential == "" || strings.HasPrefix(credential, "genroc_sk_") {
		return nil, nil
	}
	if strings.Count(credential, ".") != 2 {
		return nil, nil
	}

	claims := jwt.MapClaims{}
	if _, err := a.parser.ParseWithClaims(credential, claims, func(*jwt.Token) (any, error) {
		return a.secret, nil
	}); err != nil {
		return nil, nil
	}

	subject, _ := claims["sub"].(string)
	if subject = strings.TrimSpace(subject); subject == "" {
		// Verified but unusable: nothing to record as the actor.
		return nil, nil
	}
	grants := permGrants(claims[permsClaim])
	if len(grants) == 0 {
		// Granting nothing is 403, not 401: the issuer authenticated this person.
		return &Principal{Subject: subject, Source: "jwt"}, nil
	}
	return &Principal{Subject: subject, Grants: grants, Source: "jwt"}, nil
}

// permGrants keeps an unrecognised permission: Allows never matches it, so a newer issuer
// degrades instead of failing.
func permGrants(v any) []Grant {
	list, ok := v.([]any)
	if !ok {
		return nil
	}
	seen := map[Perm]bool{}
	out := make([]Grant, 0, len(list))
	for _, x := range list {
		s, ok := x.(string)
		if !ok {
			continue
		}
		if p := Perm(strings.TrimSpace(s)); p != "" && !seen[p] {
			seen[p] = true
			out = append(out, Grant{Perm: p})
		}
	}
	return out
}

func parseLeeway(s string) (time.Duration, error) {
	if s == "" {
		// Not zero: a fixed zero fails on real clusters, where clocks disagree by seconds.
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

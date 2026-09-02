package api

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// JWTAuth is `mode: jwt` -- a token minted by genroc-ui (or anything else able to produce a
// conforming one) and signed with a secret shared with this server.
//
// It VERIFIES and it RESOLVES NOTHING. The token already carries the permissions its issuer
// computed, so there is no role map here, no group claim, and no per-provider quirk. What this
// server owns is which endpoint needs which permission (`actionDef.Allow`), and that never
// left. specs/ui-issued-tokens.md §1.
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
	// Every one of these is a §2.4 validation, and they are parser OPTIONS rather than checks
	// written below, so that no path verifies without them. HS256 is the only accepted method:
	// with one symmetric key there is no algorithm set to misconfigure.
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

// Authenticate verifies a bearer token and reads the permissions off it.
//
// A credential that is not a JWT returns (nil, nil) rather than an error: a deployment runs this
// beside `token` mode, and a `genroc_sk_*` presented here is simply not this mode's to answer. A
// token that IS a JWT but fails verification also returns (nil, nil) -- it did not authenticate,
// and authorize turns that into 401. Only a failure to DECIDE is an error.
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
		// A verified token granting nothing is not an authentication failure -- the issuer
		// authenticated this person and decided they may do nothing here. 403, not 401, which
		// authorize produces from a principal holding no grants.
		return &Principal{Subject: subject, Source: "jwt"}, nil
	}
	return &Principal{Subject: subject, Grants: grants, Source: "jwt"}, nil
}

// permGrants reads the `perms` claim. An unrecognised string is kept rather than filtered: it
// grants nothing, because Allows only ever compares against permissions this server declares, so
// a newer issuer naming a permission this build predates degrades instead of failing.
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

package main

import (
	"fmt"
	"strings"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// Two HS256 tokens, one secret (specs/ui-issued-tokens.md §2, §4): SESSION {sub, groups}, hours,
// in an HttpOnly cookie and never sent to the server; ACCESS {sub, perms}, a minute, on each
// proxied request. The provider's ID token is used once, at login, then discarded.

// sessionAudience keeps a 12-hour session cookie from being replayed as an access token: both
// are `iss: genroc-ui` HS256 under one key.
const sessionAudience = "genroc-ui-session"

type signer struct {
	secret     []byte
	issuer     string
	audience   string
	tokenTTL   time.Duration
	sessionTTL time.Duration
}

// identity is what a login establishes, whichever way it happened. OIDC and a config password
// converge here, and everything after this point is identical.
type identity struct {
	Subject string
	Groups  []string
}

func (s *signer) mintSession(id identity) (string, time.Time, error) {
	exp := time.Now().Add(s.sessionTTL)
	groups := make([]any, len(id.Groups))
	for i, g := range id.Groups {
		groups[i] = g
	}
	tok, err := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.MapClaims{
		"iss": s.issuer, "aud": sessionAudience, "sub": id.Subject,
		"groups": groups, "iat": time.Now().Unix(), "exp": exp.Unix(),
	}).SignedString(s.secret)
	return tok, exp, err
}

func (s *signer) readSession(raw string) (identity, error) {
	claims := jwt.MapClaims{}
	if _, err := jwt.NewParser(
		jwt.WithValidMethods([]string{jwt.SigningMethodHS256.Alg()}),
		jwt.WithIssuer(s.issuer),
		jwt.WithAudience(sessionAudience),
		jwt.WithExpirationRequired(),
		jwt.WithLeeway(30*time.Second),
	).ParseWithClaims(raw, claims, func(*jwt.Token) (any, error) {
		return s.secret, nil
	}); err != nil {
		return identity{}, err
	}
	sub, _ := claims["sub"].(string)
	if sub = strings.TrimSpace(sub); sub == "" {
		return identity{}, fmt.Errorf("session has no subject")
	}
	return identity{Subject: sub, Groups: stringList(claims["groups"])}, nil
}

// mintAccess carries resolved PERMISSIONS and a short expiry, so an escaped token is worth
// little (specs/ui-issued-tokens.md §2).
func (s *signer) mintAccess(id identity, perms []string) (string, error) {
	list := make([]any, len(perms))
	for i, p := range perms {
		list[i] = p
	}
	return jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.MapClaims{
		"iss": s.issuer, "aud": s.audience, "sub": id.Subject,
		"perms": list, "iat": time.Now().Unix(),
		"exp": time.Now().Add(s.tokenTTL).Unix(),
	}).SignedString(s.secret)
}

// resolve maps groups to permissions. `*` applies to anyone signed in; a subject's entry is
// unioned on top, for providers with no groups.
func resolve(roles, users map[string][]string, id identity) []string {
	seen := map[string]bool{}
	var out []string
	add := func(perms []string) {
		for _, p := range perms {
			if p = strings.TrimSpace(p); p != "" && !seen[p] {
				seen[p] = true
				out = append(out, p)
			}
		}
	}
	for _, g := range id.Groups {
		add(roles[g])
	}
	add(users[id.Subject])
	add(roles["*"])
	return out
}

func stringList(v any) []string {
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
	case []string:
		return t
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

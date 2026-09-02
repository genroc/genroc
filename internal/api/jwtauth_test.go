package api

import (
	"context"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// specs/ui-issued-tokens.md §2 — the token is a CONTRACT, so these are the tests a third-party
// UI would be checked against. Each rejection below is a way JWT deployments are broken, and
// `jwks_file` used to be what made them reachable; with a shared secret they are simply cheap.

// mintTestToken produces what genroc-ui is specified to produce. A test that built the token any
// other way would be testing itself.
func mintTestToken(t *testing.T, secret, iss, aud, sub string, perms []string, ttl time.Duration) string {
	t.Helper()
	claims := jwt.MapClaims{
		"iss": iss, "aud": aud, "sub": sub,
		"iat": time.Now().Unix(), "exp": time.Now().Add(ttl).Unix(),
	}
	if perms != nil {
		list := make([]any, len(perms))
		for i, p := range perms {
			list[i] = p
		}
		claims["perms"] = list
	}
	s, err := jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString([]byte(secret))
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func testAuth(t *testing.T) *JWTAuth {
	t.Helper()
	a, err := NewJWTAuth(JWTModeConfig{Issuer: "genroc-ui", Audience: "genroc", Secret: testSecret})
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func TestJWT_ReadsPermissionsStraightOffTheToken(t *testing.T) {
	a := testAuth(t)
	tok := mintTestToken(t, testSecret, "genroc-ui", "genroc", "ada@example.com",
		[]string{"deploy", "read"}, time.Hour)

	p, err := a.Authenticate(context.Background(), tok)
	if err != nil || p == nil {
		t.Fatalf("p=%v err=%v", p, err)
	}
	if !p.Allows([]Perm{PermDeploy}) || !p.Allows([]Perm{PermRead}) {
		t.Fatalf("grants = %v; the issuer already resolved these and this server only reads them", p.Grants)
	}
	if p.Allows([]Perm{PermAdmin}) {
		t.Error("granted a permission the token did not carry")
	}
	if p.Actor() != "jwt:ada@example.com" {
		t.Errorf("Actor() = %q", p.Actor())
	}
}

func TestJWT_RefusesEveryWayTheContractCanBeViolated(t *testing.T) {
	a := testAuth(t)
	other := "a-different-secret-also-long-enough-to-pass"

	cases := []struct {
		name  string
		token func() string
		why   string
	}{
		{"signed with another secret", func() string {
			return mintTestToken(t, other, "genroc-ui", "genroc", "ada", []string{"admin"}, time.Hour)
		}, "the signature is the whole guarantee"},
		{"another issuer", func() string {
			return mintTestToken(t, testSecret, "someone-else", "genroc", "ada", []string{"admin"}, time.Hour)
		}, "an unpinned issuer holding the secret would be accepted"},
		{"another audience", func() string {
			return mintTestToken(t, testSecret, "genroc-ui", "other-app", "ada", []string{"admin"}, time.Hour)
		}, "a token minted for a different application would verify here"},
		{"expired", func() string {
			return mintTestToken(t, testSecret, "genroc-ui", "genroc", "ada", []string{"admin"}, -2*time.Hour)
		}, "exp is what bounds replay"},
		{"no subject", func() string {
			return mintTestToken(t, testSecret, "genroc-ui", "genroc", "", []string{"admin"}, time.Hour)
		}, "there would be nothing to record as the actor"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p, err := a.Authenticate(context.Background(), c.token())
			if err != nil {
				t.Fatalf("a rejection must be (nil, nil), not an error: %v", err)
			}
			if p != nil {
				t.Fatalf("ACCEPTED a token %s — %s", c.name, c.why)
			}
		})
	}
}

// `alg: none` and RS256/HS256 confusion are closed by there being ONE accepted method. Signing
// with a method the config does not allow must fail even when the key would otherwise work.
func TestJWT_RefusesAnyAlgorithmButHS256(t *testing.T) {
	a := testAuth(t)
	tok := jwt.NewWithClaims(jwt.SigningMethodNone, jwt.MapClaims{
		"iss": "genroc-ui", "aud": "genroc", "sub": "mallory",
		"perms": []any{"admin"}, "exp": time.Now().Add(time.Hour).Unix(),
	})
	raw, err := tok.SignedString(jwt.UnsafeAllowNoneSignatureType)
	if err != nil {
		t.Fatal(err)
	}
	if p, _ := a.Authenticate(context.Background(), raw); p != nil {
		t.Fatal("an unsigned token authenticated")
	}

	// HS512 with the right secret: the signature is genuine and only the algorithm is wrong, so
	// nothing but the pinned method can reject it.
	hs512, err := jwt.NewWithClaims(jwt.SigningMethodHS512, jwt.MapClaims{
		"iss": "genroc-ui", "aud": "genroc", "sub": "ada",
		"perms": []any{"admin"}, "exp": time.Now().Add(time.Hour).Unix(),
	}).SignedString([]byte(testSecret))
	if err != nil {
		t.Fatal(err)
	}
	if p, _ := a.Authenticate(context.Background(), hs512); p != nil {
		t.Fatal("HS512 was accepted by an HS256-only verifier")
	}
}

// A verified caller the issuer decided may do nothing is authenticated but not permitted: 403,
// not 401. Collapsing them would make the most common support question undiagnosable (§8).
func TestJWT_AVerifiedTokenWithNoPermissionsIsForbiddenNotUnauthenticated(t *testing.T) {
	a := testAuth(t)
	tok := mintTestToken(t, testSecret, "genroc-ui", "genroc", "bob@example.com", []string{}, time.Hour)

	p, err := a.Authenticate(context.Background(), tok)
	if err != nil || p == nil {
		t.Fatalf("a verified token must produce a principal: p=%v err=%v", p, err)
	}
	if got := authorize(actionDef{Name: "list_definitions", Allow: []Perm{PermRead}}, p); got == nil {
		t.Fatal("a principal with no grants was allowed")
	} else if got.Code != CodeForbidden {
		t.Fatalf("code = %q, want forbidden — the caller IS authenticated, they may just not", got.Code)
	}
}

// Unknown permissions degrade rather than fail: a newer issuer naming one this build predates
// must not invalidate the whole token.
func TestJWT_AnUnknownPermissionGrantsNothingAndBreaksNothing(t *testing.T) {
	a := testAuth(t)
	tok := mintTestToken(t, testSecret, "genroc-ui", "genroc", "ada",
		[]string{"read", "teleport"}, time.Hour)

	p, err := a.Authenticate(context.Background(), tok)
	if err != nil || p == nil {
		t.Fatalf("p=%v err=%v", p, err)
	}
	if !p.Allows([]Perm{PermRead}) {
		t.Error("a known permission was lost because an unknown one sat beside it")
	}
	if p.Allows([]Perm{PermAdmin}) {
		t.Error("an unknown permission granted something")
	}
}

func TestJWT_DeclinesAGenrocTokenSoTheChainCanContinue(t *testing.T) {
	a := testAuth(t)
	for _, cred := range []string{"genroc_sk_abc", "not-a-jwt", "", "a.b"} {
		p, err := a.Authenticate(context.Background(), cred)
		if err != nil || p != nil {
			t.Fatalf("credential %q: p=%v err=%v — a non-JWT must be declined quietly so the "+
				"next mode in the chain sees it", cred, p, err)
		}
	}
}

package api

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"math/big"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// specs/api-auth.md §2.4. These exist BECAUSE `jwks_file` does: each case below is a known way
// JWT deployments are broken, and none of them could be reached from an end-to-end test that
// needed a real identity provider to mint the bad token.

const (
	testIssuer = "https://accounts.example.test"
	testAud    = "genroc"
	testKid    = "k1"
)

type signer struct {
	key      *rsa.PrivateKey
	jwksPath string
}

func newSigner(t *testing.T) *signer {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	doc := map[string]any{"keys": []map[string]any{{
		"kty": "RSA",
		"kid": testKid,
		"use": "sig",
		"n":   base64.RawURLEncoding.EncodeToString(key.N.Bytes()),
		"e":   base64.RawURLEncoding.EncodeToString(big.NewInt(int64(key.E)).Bytes()),
	}}}
	raw, err := json.Marshal(doc)
	if err != nil {
		t.Fatalf("marshal jwks: %v", err)
	}
	path := filepath.Join(t.TempDir(), "jwks.json")
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatalf("write jwks: %v", err)
	}
	return &signer{key: key, jwksPath: path}
}

// sign mints an RS256 token, letting a test override any registered claim.
func (s *signer) sign(t *testing.T, over map[string]any) string {
	t.Helper()
	claims := jwt.MapClaims{
		"iss":    testIssuer,
		"aud":    testAud,
		"sub":    "ada@example.test",
		"groups": []any{"genroc-admins"},
		"exp":    time.Now().Add(time.Hour).Unix(),
		"iat":    time.Now().Unix(),
	}
	for k, v := range over {
		if v == nil {
			delete(claims, k)
			continue
		}
		claims[k] = v
	}
	tok := jwt.NewWithClaims(jwt.SigningMethodRS256, claims)
	tok.Header["kid"] = testKid
	out, err := tok.SignedString(s.key)
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	return out
}

func (s *signer) auth(t *testing.T, mutate func(*AuthConfig)) *JWTAuth {
	t.Helper()
	cfg := &AuthConfig{
		Mode: "jwt",
		JWT: JWTModeConfig{
			JWKSFile: s.jwksPath, Issuer: testIssuer, Audience: testAud,
			Algorithms: []string{"RS256"}, SubjectClaim: "sub", RolesClaim: "groups",
		},
		Roles: map[string][]string{"genroc-admins": {"admin"}},
	}
	if mutate != nil {
		mutate(cfg)
	}
	a, err := NewJWTAuth(cfg)
	if err != nil {
		t.Fatalf("NewJWTAuth: %v", err)
	}
	return a
}

func TestJWT_VerifiesAndResolvesRolesThroughTheMap(t *testing.T) {
	s := newSigner(t)
	p, err := s.auth(t, nil).Authenticate(context.Background(), s.sign(t, nil))
	if err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	if p == nil {
		t.Fatal("a validly signed token did not authenticate")
	}
	if p.Subject != "ada@example.test" || p.Source != "jwt" {
		t.Fatalf("Principal = %+v", p)
	}
	if !p.Allows([]Perm{PermDeploy}) {
		t.Fatal("the role map did not resolve `genroc-admins` to admin; §2.3 puts the " +
			"role→permission decision in genroc, so a verified token with a mapped role must land here")
	}
	if p.Actor() != "jwt:ada@example.test" {
		t.Fatalf("Actor() = %q, want jwt:ada@example.test", p.Actor())
	}
}

// Each of these is §2.4. A rejection is (nil, nil) — not authenticated — never an error.
func TestJWT_RefusesTheThreeClassicMisconfigurations(t *testing.T) {
	s := newSigner(t)
	other := newSigner(t)

	cases := []struct {
		name  string
		token func() string
		why   string
	}{
		{
			"a token minted for another audience",
			func() string { return s.sign(t, map[string]any{"aud": "some-other-app"}) },
			"without an audience check, every other app in the same tenant is a genroc credential",
		},
		{
			"a token from an unpinned issuer",
			func() string { return s.sign(t, map[string]any{"iss": "https://evil.test"}) },
			"an attacker-chosen issuer with a valid JWKS would be accepted",
		},
		{
			"a token signed by a different key entirely",
			func() string { return other.sign(t, nil) },
			"the signature is the whole guarantee; §2.1 rests on it",
		},
		{
			"an expired token",
			func() string {
				return s.sign(t, map[string]any{"exp": time.Now().Add(-2 * time.Hour).Unix()})
			},
			"exp is what bounds replay, which a plain header cannot express at all",
		},
		{
			"a token with no expiry",
			func() string { return s.sign(t, map[string]any{"exp": nil}) },
			"a token that never expires is a permanent credential nobody can revoke",
		},
		{
			"a token not yet valid",
			func() string {
				return s.sign(t, map[string]any{"nbf": time.Now().Add(time.Hour).Unix()})
			},
			"nbf is checked with the same parser options as exp",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p, err := s.auth(t, nil).Authenticate(context.Background(), c.token())
			if err != nil {
				t.Fatalf("a rejection must be (nil, nil), not an error: %v", err)
			}
			if p != nil {
				t.Fatalf("ACCEPTED %s — %s", c.name, c.why)
			}
		})
	}
}

// The algorithm PIN specifically. The two confusion cases below are also refused by golang-jwt's
// key typing, so removing WithValidMethods does not fail them — this one signs with the right
// key and only the wrong algorithm, so nothing but the pin can reject it.
func TestJWT_RefusesAnAlgorithmOutsideTheConfiguredSet(t *testing.T) {
	s := newSigner(t)
	tok := jwt.NewWithClaims(jwt.SigningMethodRS512, jwt.MapClaims{
		"iss": testIssuer, "aud": testAud, "sub": "ada@example.test",
		"exp": time.Now().Add(time.Hour).Unix(),
	})
	tok.Header["kid"] = testKid
	raw, err := tok.SignedString(s.key)
	if err != nil {
		t.Fatalf("sign rs512: %v", err)
	}
	// The config allows RS256 only. The signature is genuine and the key is the right one, so
	// this verifies unless the algorithm set is enforced.
	p, err := s.auth(t, nil).Authenticate(context.Background(), raw)
	if err != nil || p != nil {
		t.Fatalf("an RS512 token was accepted by an RS256-only configuration (p=%v, err=%v) — "+
			"§2.4 pins the algorithm set to what the issuer actually uses", p, err)
	}
}

// alg confusion, both directions, kept apart from the table because each is forged differently.
func TestJWT_RefusesAlgorithmConfusion(t *testing.T) {
	s := newSigner(t)
	a := s.auth(t, nil)

	t.Run("alg: none", func(t *testing.T) {
		tok := jwt.NewWithClaims(jwt.SigningMethodNone, jwt.MapClaims{
			"iss": testIssuer, "aud": testAud, "sub": "mallory@evil.test",
			"exp": time.Now().Add(time.Hour).Unix(),
		})
		tok.Header["kid"] = testKid
		raw, err := tok.SignedString(jwt.UnsafeAllowNoneSignatureType)
		if err != nil {
			t.Fatalf("sign none: %v", err)
		}
		p, err := a.Authenticate(context.Background(), raw)
		if err != nil || p != nil {
			t.Fatalf("an unsigned token authenticated (p=%v, err=%v) — `alg: none` is a live "+
				"vulnerability class and pinning the algorithm set is what closes it", p, err)
		}
	})

	t.Run("RS256 verifier handed an HS256 token keyed on the public key", func(t *testing.T) {
		// The classic confusion: the attacker knows the PUBLIC key (it is published in the
		// JWKS) and signs an HMAC token with it, hoping the verifier picks HMAC.
		pub, err := x509.MarshalPKIXPublicKey(&s.key.PublicKey)
		if err != nil {
			t.Fatalf("marshal pub: %v", err)
		}
		tok := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.MapClaims{
			"iss": testIssuer, "aud": testAud, "sub": "mallory@evil.test",
			"exp": time.Now().Add(time.Hour).Unix(),
		})
		tok.Header["kid"] = testKid
		raw, err := tok.SignedString(pub)
		if err != nil {
			t.Fatalf("sign hs256: %v", err)
		}
		p, err := a.Authenticate(context.Background(), raw)
		if err != nil || p != nil {
			t.Fatalf("an HS256 token verified against an RS256 configuration (p=%v, err=%v)", p, err)
		}
	})
}

func TestJWT_LeewayAbsorbsClockSkewButNotAReallyExpiredToken(t *testing.T) {
	s := newSigner(t)
	a := s.auth(t, func(c *AuthConfig) { c.JWT.Leeway = "60s" })

	justExpired := s.sign(t, map[string]any{"exp": time.Now().Add(-10 * time.Second).Unix()})
	p, err := a.Authenticate(context.Background(), justExpired)
	if err != nil || p == nil {
		t.Fatalf("a token 10s past expiry was refused under 60s leeway (err=%v) — a fixed zero "+
			"skew fails on real clusters, which is why leeway is configurable", err)
	}

	longGone := s.sign(t, map[string]any{"exp": time.Now().Add(-10 * time.Minute).Unix()})
	if p, _ := a.Authenticate(context.Background(), longGone); p != nil {
		t.Fatal("leeway swallowed a token expired ten minutes ago; it absorbs skew, not expiry")
	}
}

// The chain (§2): jwt and token both read `Authorization: Bearer`, so each must decline the
// other's credential rather than erroring on it.
func TestJWT_DeclinesAGenrocTokenSoTheChainCanContinue(t *testing.T) {
	s := newSigner(t)
	a := s.auth(t, nil)
	for _, cred := range []string{"genroc_sk_" + "a", "not-a-jwt", "", "a.b"} {
		p, err := a.Authenticate(context.Background(), cred)
		if err != nil || p != nil {
			t.Fatalf("credential %q: p=%v err=%v — a non-JWT must be declined quietly so the "+
				"next mode in the chain sees it", cred, p, err)
		}
	}
}

func TestChain_TakesTheFirstModeThatRecognisesTheCredential(t *testing.T) {
	s := newSigner(t)
	jwtAuth := s.auth(t, nil)
	stub := stubAuth{principal: &Principal{Subject: "ci", Source: "token",
		Grants: []Grant{{Perm: PermWorker}}}}

	c := Chain(jwtAuth, stub)

	p, err := c.Authenticate(context.Background(), s.sign(t, nil))
	if err != nil || p == nil || p.Source != "jwt" {
		t.Fatalf("a JWT should be answered by jwt mode: p=%v err=%v", p, err)
	}
	p, err = c.Authenticate(context.Background(), "genroc_sk_whatever")
	if err != nil || p == nil || p.Source != "token" {
		t.Fatalf("a genroc token should fall through to token mode: p=%v err=%v", p, err)
	}
}

func TestChain_StopsOnAModeThatCannotDecide(t *testing.T) {
	failing := stubAuth{err: context.DeadlineExceeded}
	reached := &stubAuth{principal: &Principal{Subject: "x", Source: "token"}}
	if _, err := Chain(failing, reached).Authenticate(context.Background(), "c"); err == nil {
		t.Fatal("a mode that could not decide was silently downgraded to `not authenticated` by " +
			"the next link — that turns an outage into a 401 the operator cannot diagnose")
	}
}

type stubAuth struct {
	principal *Principal
	err       error
}

func (s stubAuth) Authenticate(context.Context, string) (*Principal, error) {
	return s.principal, s.err
}

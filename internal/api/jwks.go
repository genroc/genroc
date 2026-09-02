package api

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"os"
	"sync"
	"time"
)

// JWKS handling. specs/api-auth.md §2.1, §2.4.
//
// Parsed here with the standard library rather than by a second dependency: turning a JWK into
// a *rsa.PublicKey is base64 and big.Int, not cryptography -- the cryptography is the signature
// check, which is golang-jwt's. genroc's dep list is small and deliberate, and §2.1 budgeted one
// addition for this mode.

// jwk is the subset of RFC 7517 a verifier needs. `alg` is deliberately NOT read: the accepted
// algorithms come from configuration (§2.4), so a key that names its own would let the token's
// own metadata widen what is accepted.
type jwk struct {
	Kty string `json:"kty"`
	Kid string `json:"kid"`
	Use string `json:"use"`
	N   string `json:"n"` // RSA modulus
	E   string `json:"e"` // RSA exponent
	Crv string `json:"crv"`
	X   string `json:"x"`
	Y   string `json:"y"`
}

func parseJWKS(raw []byte) (map[string]crypto.PublicKey, error) {
	var doc struct {
		Keys []jwk `json:"keys"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("parse JWKS: %w", err)
	}
	out := make(map[string]crypto.PublicKey, len(doc.Keys))
	for _, k := range doc.Keys {
		// A key published for encryption is not a signing key; accepting one would verify
		// signatures against material the issuer never intended for that purpose.
		if k.Use != "" && k.Use != "sig" {
			continue
		}
		key, err := k.publicKey()
		if err != nil {
			// One unusable key must not sink the whole set: an issuer rotating in a type we
			// do not implement would otherwise take down every verification.
			continue
		}
		out[k.Kid] = key
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("JWKS contains no usable signing key")
	}
	return out, nil
}

func (k jwk) publicKey() (crypto.PublicKey, error) {
	switch k.Kty {
	case "RSA":
		n, err := b64uint(k.N)
		if err != nil {
			return nil, err
		}
		e, err := b64uint(k.E)
		if err != nil {
			return nil, err
		}
		if !e.IsInt64() || e.Int64() < 3 {
			return nil, fmt.Errorf("implausible RSA exponent")
		}
		return &rsa.PublicKey{N: n, E: int(e.Int64())}, nil
	case "EC":
		var curve elliptic.Curve
		switch k.Crv {
		case "P-256":
			curve = elliptic.P256()
		case "P-384":
			curve = elliptic.P384()
		case "P-521":
			curve = elliptic.P521()
		default:
			return nil, fmt.Errorf("unsupported curve %q", k.Crv)
		}
		x, err := b64uint(k.X)
		if err != nil {
			return nil, err
		}
		y, err := b64uint(k.Y)
		if err != nil {
			return nil, err
		}
		if !curve.IsOnCurve(x, y) {
			return nil, fmt.Errorf("EC key is not on curve %s", k.Crv)
		}
		return &ecdsa.PublicKey{Curve: curve, X: x, Y: y}, nil
	default:
		return nil, fmt.Errorf("unsupported key type %q", k.Kty)
	}
}

func b64uint(s string) (*big.Int, error) {
	b, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return nil, fmt.Errorf("bad base64url in JWK: %w", err)
	}
	return new(big.Int).SetBytes(b), nil
}

// keySet serves verification keys by `kid`, from a file or a URL.
//
// A file is re-read on every miss and a URL re-fetched at most once per refreshInterval, which
// is what makes rotation work without a restart: an issuer publishes the new key before signing
// with it, so a `kid` genroc has not seen is the signal to look again. Rate-limiting that is
// what stops a stream of garbage `kid`s from turning into a stream of outbound requests.
type keySet struct {
	url    string
	file   string
	client *http.Client

	// Per-server state with a lifetime, not a lookup table: it belongs to the struct whose
	// life it shares rather than at package level. See the root CLAUDE.md.
	mu          sync.RWMutex
	keys        map[string]crypto.PublicKey
	lastRefresh time.Time
	now         func() time.Time
}

// refreshInterval bounds how often an unknown `kid` may trigger a fetch.
const refreshInterval = 5 * time.Minute

// jwksFetchTimeout bounds one JWKS fetch. The shared transport carries no Client.Timeout
// (internal/transport/CLAUDE.md), so this one is set here and applies per request.
const jwksFetchTimeout = 10 * time.Second

func newKeySet(url, file string) *keySet {
	return &keySet{
		url:    url,
		file:   file,
		client: &http.Client{Timeout: jwksFetchTimeout},
		now:    time.Now,
	}
}

// keyFor returns the verification key for a kid, refreshing at most once per refreshInterval.
//
// An empty kid is accepted only when the set holds exactly one key: a JWKS with several keys and
// a token that names none is ambiguous, and picking one arbitrarily would mean a token verified
// against a key the issuer did not sign it with, or not, depending on map iteration order.
func (k *keySet) keyFor(ctx context.Context, kid string) (crypto.PublicKey, error) {
	if key, ok := k.lookup(kid); ok {
		return key, nil
	}
	if err := k.refresh(ctx); err != nil {
		return nil, err
	}
	if key, ok := k.lookup(kid); ok {
		return key, nil
	}
	return nil, fmt.Errorf("no key %q in JWKS", kid)
}

func (k *keySet) lookup(kid string) (crypto.PublicKey, bool) {
	k.mu.RLock()
	defer k.mu.RUnlock()
	if kid == "" {
		if len(k.keys) == 1 {
			for _, v := range k.keys {
				return v, true
			}
		}
		return nil, false
	}
	key, ok := k.keys[kid]
	return key, ok
}

func (k *keySet) refresh(ctx context.Context) error {
	k.mu.Lock()
	defer k.mu.Unlock()
	if !k.lastRefresh.IsZero() && k.now().Sub(k.lastRefresh) < refreshInterval {
		// Already refreshed recently: report the miss rather than fetching again, so an
		// unknown kid costs one lookup instead of one request.
		return fmt.Errorf("JWKS was refreshed recently and holds no such key")
	}
	raw, err := k.load(ctx)
	if err != nil {
		return err
	}
	keys, err := parseJWKS(raw)
	if err != nil {
		return err
	}
	k.keys = keys
	k.lastRefresh = k.now()
	return nil
}

func (k *keySet) load(ctx context.Context) ([]byte, error) {
	if k.file != "" {
		return os.ReadFile(k.file)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, k.url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := k.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch JWKS: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("fetch JWKS: %s", resp.Status)
	}
	// Capped: an endpoint answering with something enormous must not be read into memory
	// unbounded. A real JWKS is a few kilobytes.
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("read JWKS: %w", err)
	}
	return raw, nil
}

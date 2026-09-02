package api

import (
	"fmt"
	"os"
	"strings"
)

// How the server accepts JWTs. specs/ui-issued-tokens.md.
//
// FLAGS, not a config file. api-auth.md §4 argued for a file because the auth config held the
// ROLE MAP — policy, which must not be editable through the API it governs. The role map moved
// to genroc-ui, and what is left is four scalars describing which tokens to accept. A file for
// that is a parser, a schema and a mount for no benefit; it is also how the server ended up
// depending on a YAML library.
type JWTModeConfig struct {
	// Issuer and Audience are pinned. Unpinned, a token minted for a different application by
	// the same issuer verifies here too, which is the most common real-world JWT bug.
	// specs/api-auth.md §2.4. Both default to what genroc-ui uses, so a deployment that runs
	// the pair as shipped configures neither.
	Issuer   string
	Audience string
	// Secret or SecretFile. The algorithm is HS256 and is not configurable: with a single issuer
	// and a symmetric key there is no set to get wrong, which closes `alg: none` and RS256/HS256
	// confusion by construction rather than by pinning.
	Secret     string
	SecretFile string
	Leeway     string
}

const (
	// minSecretBytes is the floor for the shared secret. HMAC's strength is the key's entropy,
	// so a short one is a forgeable one — and forging a token here mints any identity with any
	// permissions. Refused at startup rather than warned about.
	minSecretBytes = 32

	DefaultJWTIssuer   = "genroc-ui"
	DefaultJWTAudience = "genroc"
)

// Validate fills the defaults and refuses anything unusable, at startup rather than at the first
// request: a server that starts and then accepts forged tokens is the failure being avoided.
func (j *JWTModeConfig) Validate() error {
	if j.Issuer == "" {
		j.Issuer = DefaultJWTIssuer
	}
	if j.Audience == "" {
		j.Audience = DefaultJWTAudience
	}
	if _, err := j.resolveSecret(); err != nil {
		return err
	}
	if _, err := parseLeeway(j.Leeway); err != nil {
		return err
	}
	return nil
}

func (j JWTModeConfig) resolveSecret() (string, error) {
	if j.Secret != "" && j.SecretFile != "" {
		return "", fmt.Errorf("-jwt-secret and -jwt-secret-file are exclusive")
	}
	secret := j.Secret
	if j.SecretFile != "" {
		raw, err := os.ReadFile(j.SecretFile)
		if err != nil {
			return "", fmt.Errorf("-jwt-secret-file: %w", err)
		}
		// Trimmed: a secret delivered as a file almost always arrives with a trailing newline,
		// and a mismatch on an invisible byte is the worst kind to debug.
		secret = strings.TrimSpace(string(raw))
	}
	if secret == "" {
		return "", fmt.Errorf("a signing secret is required — the key genroc-ui signs with")
	}
	if len(secret) < minSecretBytes {
		return "", fmt.Errorf("the signing secret must be at least %d characters; got %d — "+
			"HMAC is only as strong as its key, and forging one here mints any identity",
			minSecretBytes, len(secret))
	}
	return secret, nil
}

package api

import (
	"fmt"
	"os"
	"strings"
)

// How the server accepts JWTs, as flags: four scalars need no config file now that the role
// map lives in genroc-ui. specs/ui-issued-tokens.md.
type JWTModeConfig struct {
	// Pinned, or a token the same issuer minted for another application verifies here too.
	// Both default to genroc-ui's. specs/api-auth.md §2.4.
	Issuer   string
	Audience string
	// Secret or SecretFile. HS256 only, not configurable: that closes `alg: none` and
	// RS256/HS256 confusion by construction.
	Secret     string
	SecretFile string
	Leeway     string
}

const (
	// minSecretBytes: a short HMAC key is forgeable, and a forged token mints any identity, so
	// a shorter secret is refused at startup, not warned about.
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
		return "", fmt.Errorf("$GENROC_JWT_SECRET and --jwt-secret-file are exclusive")
	}
	secret := j.Secret
	if j.SecretFile != "" {
		raw, err := os.ReadFile(j.SecretFile)
		if err != nil {
			return "", fmt.Errorf("--jwt-secret-file: %w", err)
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

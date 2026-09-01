package db

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	dbgen "genroc/internal/db/gen"
	"genroc/internal/idgen"
)

// Machine credentials. specs/api-auth.md §5.

// TokenPrefix makes a leaked credential greppable in a log and detectable by a secret scanner.
// It is part of the token, not decoration: HashToken hashes the whole string.
const TokenPrefix = "genroc_sk_"

// APIToken is a token as an operator sees it. Secret is set ONLY by MintToken, on the one
// occasion the plaintext exists; every later read leaves it empty because the row cannot
// produce it.
type APIToken struct {
	ID         string
	Label      string
	Perms      []string
	Secret     string
	CreatedAt  int64
	LastUsedAt int64
	RevokedAt  int64
	// 0 = never, which is what a machine credential wants. §5.
	ExpiresAt int64
}

// NewTokenSecret returns a fresh credential. 32 bytes of crypto/rand, base64url without
// padding — no ambiguity about where the token ends when it is pasted into a shell or a header.
func NewTokenSecret() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generate token: %w", err)
	}
	return TokenPrefix + base64.RawURLEncoding.EncodeToString(b), nil
}

// HashToken is what the database stores. SHA-256 rather than a password KDF on purpose: the
// input is 256 bits of uniform randomness, so there is nothing to slow an attacker down about
// — a KDF would only add latency to every request.
func HashToken(secret string) string {
	sum := sha256.Sum256([]byte(secret))
	return hex.EncodeToString(sum[:])
}

// nullMillis renders 0 as SQL NULL, which is how "never expires" is stored: a sentinel zero in
// the column would compare as long past and expire every token immediately.
func nullMillis(ms int64) sql.NullInt64 {
	return sql.NullInt64{Int64: ms, Valid: ms != 0}
}

// minSecretBody is the shortest credential body accepted from an operator. NewTokenSecret
// produces 43 base64url characters (256 bits); this floor allows a different generator while
// refusing anything a person could have typed.
const minSecretBody = 32

// ValidateTokenSecret refuses a secret genroc could never authenticate.
//
// This exists because a supplied secret is NOT symmetric with a generated one: LookupToken
// requires the prefix, so storing a prefix-less value creates a row that can never be used —
// while still counting as a live admin token, which permanently satisfies the bootstrap
// condition. The result is a silent lockout with no way back except `genroc token create`.
// Refusing at the boundary is the only place that cannot be forgotten.
func ValidateTokenSecret(secret string) error {
	if !strings.HasPrefix(secret, TokenPrefix) {
		return fmt.Errorf("a token must start with %q (generate one with `genctl token generate`)", TokenPrefix)
	}
	if body := strings.TrimPrefix(secret, TokenPrefix); len(body) < minSecretBody {
		return fmt.Errorf("a token must carry at least %d characters after %q; got %d — "+
			"a guessable admin credential is worse than none", minSecretBody, TokenPrefix, len(body))
	}
	return nil
}

// MintToken returns the token with its plaintext — the only time it exists anywhere.
//
// expiresAt is millis, or 0 for never. Required rather than optional because a machine
// credential and a browser session want opposite answers.
func (db *DB) MintToken(ctx context.Context, label string, perms []string, expiresAt int64) (APIToken, error) {
	secret, err := NewTokenSecret()
	if err != nil {
		return APIToken{}, err
	}
	encoded, err := json.Marshal(perms)
	if err != nil {
		return APIToken{}, fmt.Errorf("encode perms: %w", err)
	}
	tok := APIToken{
		ID: idgen.New(), Label: label, Perms: perms,
		Secret: secret, CreatedAt: nowMillis(), ExpiresAt: expiresAt,
	}
	err = db.q.InsertAPIToken(ctx, dbgen.InsertAPITokenParams{
		ID: tok.ID, Hash: HashToken(secret), Label: label,
		Perms: string(encoded), CreatedAt: tok.CreatedAt,
		ExpiresAt: nullMillis(expiresAt),
	})
	if err != nil {
		return APIToken{}, fmt.Errorf("insert token: %w", err)
	}
	return tok, nil
}

// LookupToken resolves a presented secret to the permissions it grants. ok=false covers both
// "no such token" and "revoked" without saying which: a caller learning that a token EXISTED
// but was revoked learns something about the deployment it has not authenticated to.
//
// The constant-time compare is belt-and-braces over an indexed equality lookup on a hash — the
// query cannot leak a timing signal about the secret, and this stops one being introduced by a
// later change to how the row is found.
func (db *DB) LookupToken(ctx context.Context, secret string) (APIToken, bool, error) {
	if !strings.HasPrefix(secret, TokenPrefix) {
		return APIToken{}, false, nil
	}
	hash := HashToken(secret)
	row, err := db.q.GetAPITokenByHash(ctx, dbgen.GetAPITokenByHashParams{
		Hash: hash, Now: sql.NullInt64{Int64: nowMillis(), Valid: true},
	})
	if errors.Is(err, sql.ErrNoRows) {
		return APIToken{}, false, nil
	}
	if err != nil {
		return APIToken{}, false, fmt.Errorf("lookup token: %w", err)
	}
	if subtle.ConstantTimeCompare([]byte(hash), []byte(HashToken(secret))) != 1 {
		return APIToken{}, false, nil
	}
	var perms []string
	if err := json.Unmarshal([]byte(row.Perms), &perms); err != nil {
		return APIToken{}, false, fmt.Errorf("token %s: decode perms: %w", row.ID, err)
	}
	return APIToken{ID: row.ID, Label: row.Label, Perms: perms}, true, nil
}

// TouchToken records that a token was used. Best-effort and throttled by the caller: it is a
// write on the read path, and losing one is worth less than slowing every request.
func (db *DB) TouchToken(ctx context.Context, id string, at int64) error {
	return db.q.TouchAPIToken(ctx, dbgen.TouchAPITokenParams{ID: id, LastUsedAt: sql.NullInt64{Int64: at, Valid: true}})
}

func (db *DB) ListTokens(ctx context.Context) ([]APIToken, error) {
	rows, err := db.q.ListAPITokens(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]APIToken, 0, len(rows))
	for _, r := range rows {
		var perms []string
		_ = json.Unmarshal([]byte(r.Perms), &perms)
		out = append(out, APIToken{
			ID: r.ID, Label: r.Label, Perms: perms, CreatedAt: r.CreatedAt,
			LastUsedAt: r.LastUsedAt.Int64, RevokedAt: r.RevokedAt.Int64,
			ExpiresAt: r.ExpiresAt.Int64,
		})
	}
	return out, nil
}

// RevokeToken marks a token dead. Reports ErrNotFound when nothing changed, so revoking twice
// is distinguishable from revoking an id that never existed — an operator running the wrong
// command should not be told it worked.
func (db *DB) RevokeToken(ctx context.Context, id string) error {
	n, err := db.q.RevokeAPIToken(ctx, dbgen.RevokeAPITokenParams{
		ID: id, RevokedAt: sql.NullInt64{Int64: nowMillis(), Valid: true},
	})
	if err != nil {
		return err
	}
	if n == 0 {
		return fmt.Errorf("token %q is not live: %w", id, ErrNotFound)
	}
	return nil
}

// EnsureBootstrapToken mints an admin token when the deployment has no live one, and does
// nothing otherwise. specs/api-auth.md §5.3.
//
// The condition is "no live ADMIN token", not "no tokens": a deployment holding only worker
// tokens has locked its operators out and still needs a way back in.
//
// **A transaction is not enough, and SERIALIZABLE is the mechanism.** Genroc runs as a fleet
// against one database, so N replicas start together and all count zero. Wrapping the count and
// the insert in an ordinary transaction does NOT stop that: under Postgres's default READ
// COMMITTED a COUNT takes no lock on rows that do not exist yet, so both transactions see zero
// and both insert. Measured, not reasoned about — 8 replicas minted 8 admin tokens with the
// plain transaction in place, and 1 with this. SQLite's single writer hides the whole problem,
// which is why TestTokens_BootstrapRaceMintsExactlyOne is meaningless without POSTGRES_DSN.
//
// A loser therefore fails at COMMIT with a serialization error rather than returning cleanly,
// and retrying is how it learns it lost: the next pass counts the winner's row and reports
// created=false. A replica must not exit because it lost this race.
//
// created reports whether this call was the one that minted, so only the winner prints a
// credential.
func (db *DB) EnsureBootstrapToken(ctx context.Context, label string, secret string) (APIToken, bool, error) {
	// Bounded: a loser needs one more pass to see the winner's row. More attempts than
	// replicas would be a busy-wait on a contended row for no gain.
	// Validated once, before the retry loop: a malformed secret is not transient, and retrying
	// it five times only delays the same refusal.
	if secret != "" {
		if err := ValidateTokenSecret(secret); err != nil {
			return APIToken{}, false, err
		}
	}
	const attempts = 5
	var err error
	for i := 0; i < attempts; i++ {
		var tok APIToken
		var created bool
		tok, created, err = db.tryBootstrapToken(ctx, label, secret)
		if err == nil {
			return tok, created, nil
		}
	}
	return APIToken{}, false, fmt.Errorf("bootstrap token after %d attempts: %w", attempts, err)
}

func (db *DB) tryBootstrapToken(ctx context.Context, label string, secret string) (tok APIToken, created bool, err error) {
	tx, qtx, _, err := db.beginTx(ctx, &sql.TxOptions{Isolation: sql.LevelSerializable})
	if err != nil {
		return APIToken{}, false, err
	}
	defer tx.Rollback()

	live, err := qtx.CountLiveAdminTokens(ctx, sql.NullInt64{Int64: nowMillis(), Valid: true})
	if err != nil {
		return APIToken{}, false, fmt.Errorf("count admin tokens: %w", err)
	}
	if live > 0 {
		return APIToken{}, false, nil
	}
	if secret == "" {
		if secret, err = NewTokenSecret(); err != nil {
			return APIToken{}, false, err
		}
	} else if err := ValidateTokenSecret(secret); err != nil {
		return APIToken{}, false, err
	}
	perms, _ := json.Marshal([]string{"admin"})
	tok = APIToken{
		ID: idgen.New(), Label: label, Perms: []string{"admin"},
		Secret: secret, CreatedAt: nowMillis(),
	}
	if err := qtx.InsertAPIToken(ctx, dbgen.InsertAPITokenParams{
		ID: tok.ID, Hash: HashToken(secret), Label: label,
		Perms: string(perms), CreatedAt: tok.CreatedAt,
	}); err != nil {
		return APIToken{}, false, fmt.Errorf("insert bootstrap token: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return APIToken{}, false, err
	}
	return tok, true, nil
}

// SeedToken ensures a token with this exact secret exists, granting perms under label. It is
// how an operator supplies credentials they generated themselves — the secret never originates
// inside genroc, so it never reaches its logs or rests in its container.
//
// Idempotent by SECRET, not by label: re-running with the same value is a no-op, and changing
// the value mints a second token rather than mutating the first. Rotation is therefore additive
// — the old credential keeps working until it is revoked, which is what lets a fleet roll
// without a window where half the workers are refused.
//
// created reports whether this call inserted, so a caller can log the difference between
// provisioning and a restart.
func (db *DB) SeedToken(ctx context.Context, label string, perms []string, secret string) (created bool, err error) {
	if err := ValidateTokenSecret(secret); err != nil {
		return false, fmt.Errorf("seed token %q: %w", label, err)
	}
	if _, ok, err := db.LookupToken(ctx, secret); err != nil {
		return false, err
	} else if ok {
		return false, nil
	}
	encoded, err := json.Marshal(perms)
	if err != nil {
		return false, err
	}
	err = db.q.InsertAPIToken(ctx, dbgen.InsertAPITokenParams{
		ID: idgen.New(), Hash: HashToken(secret), Label: label,
		Perms: string(encoded), CreatedAt: nowMillis(),
	})
	if err != nil {
		// A concurrent replica seeding the same secret loses the UNIQUE(hash) race, which is
		// success rather than failure: the row it wanted exists.
		if _, ok, lookupErr := db.LookupToken(ctx, secret); lookupErr == nil && ok {
			return false, nil
		}
		return false, fmt.Errorf("seed token %q: %w", label, err)
	}
	return true, nil
}

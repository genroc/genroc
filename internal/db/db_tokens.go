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
)

// Machine credentials. specs/api-auth.md §5.

// TokenPrefix makes a leaked credential greppable in a log and detectable by a secret scanner.
// It is part of the token, not decoration: HashToken hashes the whole string.
const TokenPrefix = "genroc_sk_"

// APIToken is a token as an operator sees it. Secret is set ONLY by MintToken; no later read can
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
	// Actor minted it, RevokedBy killed it. Two events, so two columns: neither supersedes the
	// other the way a channel's last mover supersedes its first. §7, migration 043.
	Actor     string
	RevokedBy string
}

// Actors for the mint paths outside any request, recording HOW a credential entered (§5.3's
// root-of-trust ranking). An API mint carries the caller's actor instead.
const (
	ActorSeedTokens     = "startup:seed-tokens"
	ActorBootstrapToken = "startup:bootstrap-token"
	ActorAutoMint       = "startup:auto-mint"
	ActorTokenCreate    = "cli:token-create"
	ActorTokenRevoke    = "cli:token-revoke"
)

// NewTokenSecret returns a fresh credential. 32 bytes of crypto/rand, base64url without
// padding — no ambiguity about where the token ends when it is pasted into a shell or a header.
func NewTokenSecret() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generate token: %w", err)
	}
	return TokenPrefix + base64.RawURLEncoding.EncodeToString(b), nil
}

// HashToken is what the database stores. SHA-256, not a password KDF: the input is 256 random
// bits, so a KDF would only add latency to every request.
func HashToken(secret string) string {
	sum := sha256.Sum256([]byte(secret))
	return hex.EncodeToString(sum[:])
}

// nullMillis renders 0 as SQL NULL, which is how "never expires" is stored: a sentinel zero in
// the column would compare as long past and expire every token immediately.
func nullMillis(ms int64) sql.NullInt64 {
	return sql.NullInt64{Int64: ms, Valid: ms != 0}
}

// minSecretBody (NewTokenSecret makes 43) allows another generator while refusing anything a
// person could have typed.
const minSecretBody = 32

// ValidateTokenSecret refuses a secret genroc could never authenticate: a prefix-less row is never
// usable yet counts as a live admin token, a silent lockout that permanently satisfies the
// bootstrap condition.
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

// MintToken returns the token with its plaintext, the only time it exists. expiresAt is millis,
// or 0 for never; required because machine credentials and browser sessions want opposite answers.
func (db *DB) MintToken(ctx context.Context, label string, perms []string, expiresAt int64, actor string) (APIToken, error) {
	secret, err := NewTokenSecret()
	if err != nil {
		return APIToken{}, err
	}
	encoded, err := json.Marshal(perms)
	if err != nil {
		return APIToken{}, fmt.Errorf("encode perms: %w", err)
	}
	tok := APIToken{
		ID: db.nextTokenID(), Label: label, Perms: perms,
		Secret: secret, CreatedAt: nowMillis(), ExpiresAt: expiresAt, Actor: actor,
	}
	err = db.q.InsertAPIToken(ctx, dbgen.InsertAPITokenParams{
		ID: tok.ID, Hash: HashToken(secret), Label: label,
		Perms: string(encoded), CreatedAt: tok.CreatedAt,
		ExpiresAt: nullMillis(expiresAt), Actor: actor,
	})
	if err != nil {
		return APIToken{}, fmt.Errorf("insert token: %w", err)
	}
	return tok, nil
}

// LookupToken resolves a presented secret to the permissions it grants. ok=false covers both "no
// such token" and "revoked" without saying which. The constant-time compare guards against a later
// change to how the row is found.
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
			ExpiresAt: r.ExpiresAt.Int64, Actor: r.Actor, RevokedBy: r.RevokedBy,
		})
	}
	return out, nil
}

// RevokeToken marks a token dead, or reports ErrNotFound when nothing changed: an operator running
// the wrong command must not be told it worked.
func (db *DB) RevokeToken(ctx context.Context, id string, actor string) error {
	n, err := db.q.RevokeAPIToken(ctx, dbgen.RevokeAPITokenParams{
		ID: id, RevokedAt: sql.NullInt64{Int64: nowMillis(), Valid: true}, RevokedBy: actor,
	})
	if err != nil {
		return err
	}
	if n == 0 {
		return fmt.Errorf("token %q is not live: %w", id, ErrNotFound)
	}
	return nil
}

// EnsureBootstrapToken mints an admin token when no live ADMIN one exists; created reports whether
// this call minted, so only the winner prints it. SERIALIZABLE, not READ COMMITTED: a COUNT locks
// no unborn rows, so N replicas would all insert. A loser retries. specs/api-auth.md §5.3.
func (db *DB) EnsureBootstrapToken(ctx context.Context, label string, secret string) (APIToken, bool, error) {
	// Validated once, outside the retry loop: a malformed secret is not transient.
	if secret != "" {
		if err := ValidateTokenSecret(secret); err != nil {
			return APIToken{}, false, err
		}
	}
	// A loser needs one more pass to see the winner's row; more would only busy-wait.
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
	// The two bootstrap paths differ only in who produced the secret, which is exactly the
	// difference in root of trust worth recording (§5.3).
	actor := ActorBootstrapToken
	if secret == "" {
		actor = ActorAutoMint
		if secret, err = NewTokenSecret(); err != nil {
			return APIToken{}, false, err
		}
	} else if err := ValidateTokenSecret(secret); err != nil {
		return APIToken{}, false, err
	}
	perms, _ := json.Marshal([]string{"admin"})
	tok = APIToken{
		ID: db.nextTokenID(), Label: label, Perms: []string{"admin"},
		Secret: secret, CreatedAt: nowMillis(), Actor: actor,
	}
	if err := qtx.InsertAPIToken(ctx, dbgen.InsertAPITokenParams{
		ID: tok.ID, Hash: HashToken(secret), Label: label,
		Perms: string(perms), CreatedAt: tok.CreatedAt, Actor: actor,
	}); err != nil {
		return APIToken{}, false, fmt.Errorf("insert bootstrap token: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return APIToken{}, false, err
	}
	return tok, true, nil
}

// SeedToken ensures a token with this exact secret exists, granting perms under label. Idempotent
// by SECRET, not label: a new value mints a second token, so rotation is additive and a fleet
// rolls without refusals. created reports whether this call inserted.
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
		ID: db.nextTokenID(), Hash: HashToken(secret), Label: label,
		Perms: string(encoded), CreatedAt: nowMillis(), Actor: ActorSeedTokens,
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

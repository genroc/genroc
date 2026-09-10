package dbtest

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	dbpkg "genroc/internal/db"
)

// specs/api-auth.md §5. Machine credentials, and §5.3's bootstrap — which is the part with a
// concurrency hazard rather than just CRUD, and the reason these are Go tests.

func TestTokens_MintThenLookup(t *testing.T) {
	for _, b := range testBackends(t) {
		t.Run(b.name, func(t *testing.T) {
			ctx := context.Background()
			tok, err := b.db.MintToken(ctx, "ci", []string{"deploy", "read"}, 0, dbpkg.ActorTokenCreate)
			if err != nil {
				t.Fatalf("mint: %v", err)
			}
			if !strings.HasPrefix(tok.Secret, dbpkg.TokenPrefix) {
				t.Errorf("secret %q lacks the %q prefix that makes a leak greppable", tok.Secret, dbpkg.TokenPrefix)
			}

			got, ok, err := b.db.LookupToken(ctx, tok.Secret)
			if err != nil || !ok {
				t.Fatalf("lookup: ok=%v err=%v", ok, err)
			}
			if got.ID != tok.ID {
				t.Errorf("id = %q, want %q", got.ID, tok.ID)
			}
			if strings.Join(got.Perms, ",") != "deploy,read" {
				t.Errorf("perms = %v, want [deploy read]", got.Perms)
			}
		})
	}
}

// The plaintext must not be recoverable from the database. A dump is then useless as a
// credential store, which is the whole reason the column holds a hash.
func TestTokens_PlaintextIsNotStored(t *testing.T) {
	for _, b := range testBackends(t) {
		t.Run(b.name, func(t *testing.T) {
			ctx := context.Background()
			tok, err := b.db.MintToken(ctx, "ci", []string{"read"}, 0, dbpkg.ActorTokenCreate)
			if err != nil {
				t.Fatal(err)
			}
			listed, err := b.db.ListTokens(ctx)
			if err != nil {
				t.Fatal(err)
			}
			for _, row := range listed {
				if row.Secret != "" {
					t.Errorf("ListTokens returned a secret for %s; only MintToken may", row.ID)
				}
			}
			if _, ok, _ := b.db.LookupToken(ctx, tok.Secret+"x"); ok {
				t.Error("a token with an extra character authenticated")
			}
			if _, ok, _ := b.db.LookupToken(ctx, "not-even-prefixed"); ok {
				t.Error("an unprefixed string authenticated")
			}
		})
	}
}

func TestTokens_RevokedStopsAuthenticating(t *testing.T) {
	for _, b := range testBackends(t) {
		t.Run(b.name, func(t *testing.T) {
			ctx := context.Background()
			tok, err := b.db.MintToken(ctx, "leaked", []string{"admin"}, 0, dbpkg.ActorTokenCreate)
			if err != nil {
				t.Fatal(err)
			}
			if err := b.db.RevokeToken(ctx, tok.ID, dbpkg.ActorTokenRevoke); err != nil {
				t.Fatalf("revoke: %v", err)
			}
			if _, ok, _ := b.db.LookupToken(ctx, tok.Secret); ok {
				t.Error("a revoked token still authenticates — revocation is the reason the token is opaque")
			}
			// Revoking twice is not the same as revoking something that never existed, and an
			// operator running the wrong command must not be told it worked.
			if err := b.db.RevokeToken(ctx, tok.ID, dbpkg.ActorTokenRevoke); !errors.Is(err, dbpkg.ErrNotFound) {
				t.Errorf("second revoke err = %v, want ErrNotFound", err)
			}
		})
	}
}

func TestTokens_BootstrapIsIdempotent(t *testing.T) {
	for _, b := range testBackends(t) {
		t.Run(b.name, func(t *testing.T) {
			ctx := context.Background()
			first, created, err := b.db.EnsureBootstrapToken(ctx, "bootstrap", "")
			if err != nil || !created {
				t.Fatalf("first bootstrap: created=%v err=%v", created, err)
			}
			_, created, err = b.db.EnsureBootstrapToken(ctx, "bootstrap", "")
			if err != nil {
				t.Fatal(err)
			}
			if created {
				t.Error("a restart minted a second admin token; bootstrap must be a no-op once one is live")
			}
			if _, ok, _ := b.db.LookupToken(ctx, first.Secret); !ok {
				t.Error("the first token stopped working")
			}
		})
	}
}

// The condition is "no live ADMIN token", not "no tokens at all": a deployment holding only
// worker credentials has locked its operators out and still needs a way back in.
func TestTokens_BootstrapIgnoresNonAdminTokens(t *testing.T) {
	for _, b := range testBackends(t) {
		t.Run(b.name, func(t *testing.T) {
			ctx := context.Background()
			if _, err := b.db.MintToken(ctx, "a-worker", []string{"worker"}, 0, dbpkg.ActorTokenCreate); err != nil {
				t.Fatal(err)
			}
			if _, created, err := b.db.EnsureBootstrapToken(ctx, "bootstrap", ""); err != nil || !created {
				t.Fatalf("a worker-only deployment must still bootstrap an admin: created=%v err=%v", created, err)
			}
		})
	}
}

// The fleet race: N replicas starting together each see an empty table and each mint an admin
// token, N-1 of them orphaned and unrevoked. Two conditions to know -- it bites on POSTGRES ONLY
// (SQLite's single writer serialises the two statements) and it needs the pool WARM, which in
// practice means the whole package running: READ COMMITTED was caught in 4 of 4 full runs and 0 of
// 1 `-run` runs. Reproduce a suspected regression with the full package, not with -run.
func TestTokens_BootstrapRaceMintsExactlyOne(t *testing.T) {
	for _, b := range testBackends(t) {
		t.Run(b.name, func(t *testing.T) {
			ctx := context.Background()
			const replicas = 8

			var wg sync.WaitGroup
			var mu sync.Mutex
			var minted []dbpkg.APIToken
			start := make(chan struct{})

			for i := 0; i < replicas; i++ {
				wg.Add(1)
				go func() {
					defer wg.Done()
					// Warm this goroutine's path before the barrier. It does not fully solve
					// the cold-pool problem (database/sql returns the connection straight
					// away), which is why the doc comment above bounds what this proves.
					_, _ = b.db.ListTokens(ctx)
					<-start
					tok, created, err := b.db.EnsureBootstrapToken(ctx, "bootstrap", "")
					if err != nil {
						return // a loser may collide; what matters is how many were CREATED
					}
					if created {
						mu.Lock()
						minted = append(minted, tok)
						mu.Unlock()
					}
				}()
			}
			close(start)
			wg.Wait()

			if len(minted) != 1 {
				t.Fatalf("%d replicas minted %d admin tokens, want exactly 1 — a plain transaction "+
					"is not enough here, the count needs SERIALIZABLE", replicas, len(minted))
			}
			live, err := b.db.ListTokens(ctx)
			if err != nil {
				t.Fatal(err)
			}
			admins := 0
			for _, row := range live {
				if row.RevokedAt == 0 && strings.Join(row.Perms, ",") == "admin" {
					admins++
				}
			}
			if admins != 1 {
				t.Errorf("%d live admin rows, want 1", admins)
			}
		})
	}
}

// A supplied secret is used verbatim, which is what makes -bootstrap-token declarative
// recovery: set the value, restart, and the credential you already hold works.
func TestTokens_BootstrapUsesTheSuppliedSecret(t *testing.T) {
	for _, b := range testBackends(t) {
		t.Run(b.name, func(t *testing.T) {
			ctx := context.Background()
			secret := dbpkg.TokenPrefix + "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
			if _, created, err := b.db.EnsureBootstrapToken(ctx, "from-secret", secret); err != nil || !created {
				t.Fatalf("created=%v err=%v", created, err)
			}
			got, ok, err := b.db.LookupToken(ctx, secret)
			if err != nil || !ok {
				t.Fatalf("the supplied secret does not authenticate: ok=%v err=%v", ok, err)
			}
			if got.Label != "from-secret" {
				t.Errorf("label = %q", got.Label)
			}
		})
	}
}

// A supplied secret is NOT symmetric with a generated one, and the asymmetry bricks a
// deployment: LookupToken requires the prefix, so a prefix-less value is stored as a live
// admin row that can never authenticate — while permanently satisfying the bootstrap
// condition. No usable credential, and no second chance to mint one.
func TestTokens_BootstrapRefusesAnUnusableSecret(t *testing.T) {
	for _, b := range testBackends(t) {
		t.Run(b.name, func(t *testing.T) {
			ctx := context.Background()
			for _, bad := range []string{
				"hunter2",         // no prefix: could never authenticate
				"genroc_sk_short", // prefixed but guessable
				dbpkg.TokenPrefix, // prefix alone
			} {
				if _, created, err := b.db.EnsureBootstrapToken(ctx, "bootstrap", bad); err == nil || created {
					t.Errorf("%q was accepted (created=%v); an unusable admin row is a silent lockout", bad, created)
				}
			}
			// Nothing may be left behind: a row would satisfy the bootstrap condition forever.
			rows, err := b.db.ListTokens(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if len(rows) != 0 {
				t.Fatalf("%d rows written by refused bootstraps; the table must be untouched", len(rows))
			}
			// A well-formed one still works, and a generated secret satisfies the same rule.
			good, err := dbpkg.NewTokenSecret()
			if err != nil {
				t.Fatal(err)
			}
			if err := dbpkg.ValidateTokenSecret(good); err != nil {
				t.Fatalf("a generated secret must pass its own validator: %v", err)
			}
			if _, created, err := b.db.EnsureBootstrapToken(ctx, "bootstrap", good); err != nil || !created {
				t.Fatalf("created=%v err=%v", created, err)
			}
			if _, ok, _ := b.db.LookupToken(ctx, good); !ok {
				t.Error("the supplied secret does not authenticate")
			}
		})
	}
}

// Expiry. specs/api-auth.md §5 -- the browser exchange mints on every page load, because it
// cannot return a token it issued before (only the hash is stored), so a session token that
// never expired left a permanent live credential behind each time.

func TestTokens_ExpiredStopsAuthenticating(t *testing.T) {
	for _, b := range testBackends(t) {
		t.Run(b.name, func(t *testing.T) {
			ctx := context.Background()
			ttl := time.Hour
			tok, err := b.db.MintToken(ctx, "session:alice", []string{"read"},
				dbpkg.Now().Add(ttl).UnixMilli(), dbpkg.ActorTokenCreate)
			if err != nil {
				t.Fatalf("mint: %v", err)
			}
			if _, ok, err := b.db.LookupToken(ctx, tok.Secret); err != nil || !ok {
				t.Fatalf("a token inside its window must authenticate: ok=%v err=%v", ok, err)
			}

			dbpkg.AdvanceClock(ttl + time.Minute)
			got, ok, err := b.db.LookupToken(ctx, tok.Secret)
			if err != nil {
				t.Fatalf("lookup after expiry: %v", err)
			}
			if ok {
				t.Errorf("an expired token still authenticated as %q with %v — every page load "+
					"mints one, so this is an unbounded pile of live credentials", got.Label, got.Perms)
			}
		})
	}
}

// 0 must store NULL, not a zero timestamp. A sentinel zero compares as 1970 and would expire
// every machine credential the instant it was minted.
func TestTokens_ZeroExpiryNeverExpires(t *testing.T) {
	for _, b := range testBackends(t) {
		t.Run(b.name, func(t *testing.T) {
			ctx := context.Background()
			tok, err := b.db.MintToken(ctx, "worker", []string{"worker"}, 0, dbpkg.ActorTokenCreate)
			if err != nil {
				t.Fatalf("mint: %v", err)
			}
			dbpkg.AdvanceClock(365 * 24 * time.Hour)
			if _, ok, err := b.db.LookupToken(ctx, tok.Secret); err != nil || !ok {
				t.Errorf("a machine token with no expiry stopped working after a year: ok=%v err=%v", ok, err)
			}
		})
	}
}

// An expired admin token cannot authenticate, so it must not count as "a way in still exists" --
// otherwise a deployment whose only admin credential lapsed can never bootstrap another.
func TestTokens_BootstrapIgnoresAnExpiredAdmin(t *testing.T) {
	for _, b := range testBackends(t) {
		t.Run(b.name, func(t *testing.T) {
			ctx := context.Background()
			if _, err := b.db.MintToken(ctx, "old-admin", []string{"admin"},
				dbpkg.Now().Add(time.Hour).UnixMilli(), dbpkg.ActorTokenCreate); err != nil {
				t.Fatalf("mint: %v", err)
			}
			if _, created, err := b.db.EnsureBootstrapToken(ctx, "bootstrap", ""); err != nil || created {
				t.Fatalf("a LIVE admin token must suppress bootstrap: created=%v err=%v", created, err)
			}

			dbpkg.AdvanceClock(2 * time.Hour)
			tok, created, err := b.db.EnsureBootstrapToken(ctx, "bootstrap", "")
			if err != nil {
				t.Fatalf("bootstrap: %v", err)
			}
			if !created {
				t.Fatal("the only admin token has expired and bootstrap still refused to mint — " +
					"the deployment is locked out with no way back except the database")
			}
			if _, ok, err := b.db.LookupToken(ctx, tok.Secret); err != nil || !ok {
				t.Errorf("the bootstrap token it minted does not authenticate: ok=%v err=%v", ok, err)
			}
		})
	}
}

// Every path that mints names itself, so a listing answers "where did this credential come
// from" -- §5.3's root-of-trust ranking, made readable off the row. The bootstrap pair is the
// half worth pinning: both land in EnsureBootstrapToken and only the supplied secret tells them
// apart, so getting it wrong attributes an auto-minted credential to the operator.
func TestTokens_EveryMintingPathRecordsItsOwnActor(t *testing.T) {
	for _, b := range testBackends(t) {
		t.Run(b.name, func(t *testing.T) {
			ctx := context.Background()
			seed, err := dbpkg.NewTokenSecret()
			if err != nil {
				t.Fatal(err)
			}
			supplied, err := dbpkg.NewTokenSecret()
			if err != nil {
				t.Fatal(err)
			}
			if _, err := b.db.MintToken(ctx, "by-cli", []string{"read"}, 0, dbpkg.ActorTokenCreate); err != nil {
				t.Fatalf("mint: %v", err)
			}
			if _, err := b.db.SeedToken(ctx, "by-seed", []string{"worker"}, seed); err != nil {
				t.Fatalf("seed: %v", err)
			}
			// Neither of the two above is an admin, so the bootstrap condition still holds.
			flagged, created, err := b.db.EnsureBootstrapToken(ctx, "by-flag", supplied)
			if err != nil || !created {
				t.Fatalf("bootstrap from a supplied secret: created=%v err=%v", created, err)
			}
			// Revoked so the next call sees no live admin and takes the auto-mint path instead.
			if err := b.db.RevokeToken(ctx, flagged.ID, dbpkg.ActorTokenRevoke); err != nil {
				t.Fatalf("revoke: %v", err)
			}
			if _, created, err := b.db.EnsureBootstrapToken(ctx, "by-auto", ""); err != nil || !created {
				t.Fatalf("auto-mint: created=%v err=%v", created, err)
			}

			want := map[string]string{
				"by-cli":  dbpkg.ActorTokenCreate,
				"by-seed": dbpkg.ActorSeedTokens,
				"by-flag": dbpkg.ActorBootstrapToken,
				"by-auto": dbpkg.ActorAutoMint,
			}
			rows, err := b.db.ListTokens(ctx)
			if err != nil {
				t.Fatalf("list: %v", err)
			}
			for _, r := range rows {
				if w, ok := want[r.Label]; ok {
					if r.Actor != w {
						t.Errorf("%s: actor = %q, want %q -- a credential nobody can trace to "+
							"how it entered the system is one nobody can decide to trust", r.Label, r.Actor, w)
					}
					delete(want, r.Label)
				}
				if r.Label == "by-flag" && r.RevokedBy != dbpkg.ActorTokenRevoke {
					t.Errorf("revoked_by = %q, want %q -- revoked_at moved and the actor beside "+
						"it did not, which is the seam §7 already paid for once", r.RevokedBy, dbpkg.ActorTokenRevoke)
				}
			}
			for label := range want {
				t.Errorf("no token labelled %q was listed, so its actor was never checked", label)
			}
		})
	}
}

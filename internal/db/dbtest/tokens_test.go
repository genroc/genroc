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

// specs/api-auth.md §5. Machine credentials, and §5.3's seeding — which is the part with a
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

// The startup warning keys on this: a deployment holding only worker credentials, or an admin
// that lapsed or was revoked, has locked its operators out.
func TestTokens_OnlyALiveAdminCountsAsAWayIn(t *testing.T) {
	for _, b := range testBackends(t) {
		t.Run(b.name, func(t *testing.T) {
			ctx := context.Background()
			wayIn := func() bool {
				ok, err := b.db.HasLiveAdminToken(ctx)
				if err != nil {
					t.Fatal(err)
				}
				return ok
			}
			if _, err := b.db.MintToken(ctx, "a-worker", []string{"worker"}, 0, dbpkg.ActorTokenCreate); err != nil {
				t.Fatal(err)
			}
			if wayIn() {
				t.Error("a worker token counted as an admin; a worker-only deployment would get no warning")
			}
			revoked, err := b.db.MintToken(ctx, "revoked", []string{"admin"}, 0, dbpkg.ActorTokenCreate)
			if err != nil {
				t.Fatal(err)
			}
			if err := b.db.RevokeToken(ctx, revoked.ID, dbpkg.ActorTokenRevoke); err != nil {
				t.Fatal(err)
			}
			if wayIn() {
				t.Error("a revoked admin token counted as a way in")
			}
			if _, err := b.db.MintToken(ctx, "lapsing", []string{"read", "admin"},
				dbpkg.Now().Add(time.Hour).UnixMilli(), dbpkg.ActorTokenCreate); err != nil {
				t.Fatal(err)
			}
			if !wayIn() {
				t.Fatal("a live admin token (among other perms) was not counted")
			}
			dbpkg.AdvanceClock(2 * time.Hour)
			if wayIn() {
				t.Error("an expired admin token still counted; a lapsed deployment would get no warning")
			}
		})
	}
}

// Replicas starting together read the same seed file. Losing the UNIQUE(hash) race must read as
// "already present", not fail startup, and must leave one row.
func TestTokens_ConcurrentSeedsOfOneSecretStoreOneRow(t *testing.T) {
	for _, b := range testBackends(t) {
		t.Run(b.name, func(t *testing.T) {
			ctx := context.Background()
			secret, err := dbpkg.NewTokenSecret()
			if err != nil {
				t.Fatal(err)
			}
			const replicas = 8
			var wg sync.WaitGroup
			errs := make(chan error, replicas)
			start := make(chan struct{})
			for i := 0; i < replicas; i++ {
				wg.Add(1)
				go func() {
					defer wg.Done()
					_, _ = b.db.ListTokens(ctx) // warm this goroutine's connection before the barrier
					<-start
					if _, err := b.db.SeedToken(ctx, "admin", []string{"admin"}, secret); err != nil {
						errs <- err
					}
				}()
			}
			close(start)
			wg.Wait()
			close(errs)
			for err := range errs {
				t.Errorf("a replica failed to seed (%v); it would exit at startup", err)
			}
			if rows, _ := b.db.ListTokens(ctx); len(rows) != 1 {
				t.Errorf("%d rows after %d concurrent seeds of one secret, want 1", len(rows), replicas)
			}
		})
	}
}

// A seeded secret is used verbatim, which is what makes the seed file declarative: the credential
// you put in it is the one that works.
func TestTokens_SeedUsesTheSuppliedSecret(t *testing.T) {
	for _, b := range testBackends(t) {
		t.Run(b.name, func(t *testing.T) {
			ctx := context.Background()
			secret := dbpkg.TokenPrefix + "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
			if outcome, err := b.db.SeedToken(ctx, "from-file", []string{"admin"}, secret); err != nil || outcome != dbpkg.SeedCreated {
				t.Fatalf("outcome=%v err=%v", outcome, err)
			}
			got, ok, err := b.db.LookupToken(ctx, secret)
			if err != nil || !ok {
				t.Fatalf("the supplied secret does not authenticate: ok=%v err=%v", ok, err)
			}
			if got.Label != "from-file" {
				t.Errorf("label = %q", got.Label)
			}
			if outcome, err := b.db.SeedToken(ctx, "from-file", []string{"admin"}, secret); err != nil || outcome != dbpkg.SeedPresent {
				t.Errorf("a restart re-seeding the same secret: outcome=%v err=%v, want SeedPresent", outcome, err)
			}
		})
	}
}

// A prefix-less secret would be a live admin row that can never authenticate yet silences the
// no-admin warning: no credential, and nothing saying so.
func TestTokens_SeedRefusesAnUnusableSecret(t *testing.T) {
	for _, b := range testBackends(t) {
		t.Run(b.name, func(t *testing.T) {
			ctx := context.Background()
			for _, bad := range []string{
				"hunter2",         // no prefix: could never authenticate
				"genroc_sk_short", // prefixed but guessable
				dbpkg.TokenPrefix, // prefix alone
			} {
				if _, err := b.db.SeedToken(ctx, "admin", []string{"admin"}, bad); err == nil {
					t.Errorf("%q was accepted; an unusable admin row is a silent lockout", bad)
				}
			}
			if rows, err := b.db.ListTokens(ctx); err != nil || len(rows) != 0 {
				t.Fatalf("%d rows written by refused seeds (err=%v); the table must be untouched", len(rows), err)
			}
			good, err := dbpkg.NewTokenSecret()
			if err != nil {
				t.Fatal(err)
			}
			if err := dbpkg.ValidateTokenSecret(good); err != nil {
				t.Fatalf("a generated secret must pass its own validator: %v", err)
			}
		})
	}
}

// Expiry. specs/api-auth.md §5.

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

func TestTokens_EveryMintingPathRecordsItsOwnActor(t *testing.T) {
	for _, b := range testBackends(t) {
		t.Run(b.name, func(t *testing.T) {
			ctx := context.Background()
			seed, err := dbpkg.NewTokenSecret()
			if err != nil {
				t.Fatal(err)
			}
			if _, err := b.db.MintToken(ctx, "by-cli", []string{"read"}, 0, dbpkg.ActorTokenCreate); err != nil {
				t.Fatalf("mint: %v", err)
			}
			if _, err := b.db.SeedToken(ctx, "by-seed", []string{"worker"}, seed); err != nil {
				t.Fatalf("seed: %v", err)
			}
			cli, err := b.db.MintToken(ctx, "revoked", []string{"read"}, 0, dbpkg.ActorTokenCreate)
			if err != nil {
				t.Fatalf("mint: %v", err)
			}
			if err := b.db.RevokeToken(ctx, cli.ID, dbpkg.ActorTokenRevoke); err != nil {
				t.Fatalf("revoke: %v", err)
			}

			want := map[string]string{
				"by-cli":  dbpkg.ActorTokenCreate,
				"by-seed": dbpkg.ActorSeedTokens,
				"revoked": dbpkg.ActorTokenCreate,
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
				if r.Label == "revoked" && r.RevokedBy != dbpkg.ActorTokenRevoke {
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

func TestTokens_SeedLeavesARevokedSecretRevoked(t *testing.T) {
	for _, b := range testBackends(t) {
		t.Run(b.name, func(t *testing.T) {
			ctx := context.Background()
			secret, err := dbpkg.NewTokenSecret()
			if err != nil {
				t.Fatal(err)
			}
			if _, err := b.db.SeedToken(ctx, "ci", []string{"deploy"}, secret); err != nil {
				t.Fatalf("first seed: %v", err)
			}
			rows, err := b.db.ListTokens(ctx)
			if err != nil || len(rows) != 1 {
				t.Fatalf("list: %d rows, err=%v", len(rows), err)
			}
			if err := b.db.RevokeToken(ctx, rows[0].ID, dbpkg.ActorTokenRevoke); err != nil {
				t.Fatalf("revoke: %v", err)
			}

			outcome, err := b.db.SeedToken(ctx, "ci", []string{"deploy"}, secret)
			if err != nil {
				t.Fatalf("re-seeding a revoked secret failed (%v); a restart with the seed still "+
					"configured must not crash the server", err)
			}
			if outcome != dbpkg.SeedDead {
				t.Errorf("outcome = %v, want SeedDead -- startup warns by it, naming the label", outcome)
			}
			if _, ok, _ := b.db.LookupToken(ctx, secret); ok {
				t.Error("a revoked seed authenticates again after a restart; revocation must survive it")
			}
			if rows, _ := b.db.ListTokens(ctx); len(rows) != 1 {
				t.Errorf("%d rows after re-seeding, want the one revoked row", len(rows))
			}
		})
	}
}

package dbtest

import (
	"testing"
	"time"

	dbpkg "genroc/internal/db"
)

// Content must survive a sweep that runs while a writer is resurrecting it: an object released,
// its grace lapsed, and the same content written again before the sweep reaches it.
//
// No amount of concurrency testing pins this -- TestObjects_ResurrectionAgainstALiveSweeper stayed
// green with both of the store's defences dismantled -- because the window is the microseconds
// between two adjacent statements inside one transaction. So the writer is hand-driven (being the
// transaction is the only way to look) while the SWEEP is production code. Postgres only, since
// SQLite's single writer serializes the two whatever the store does. specs/object-store.md.
func TestObjects_ContentSurvivesASweepRacingItsResurrection(t *testing.T) {
	if sharedPgRaw == nil || sharedPgDB == nil {
		t.Skip("needs POSTGRES_DSN: SQLite's single writer hides this race")
	}
	const hash = "0bjectl0ck0bjectl0ck0bjectl0ck00"
	ctx := t.Context()

	clean := func() {
		sharedPgRaw.ExecContext(ctx, `DELETE FROM object_refs WHERE hash = $1`, hash)
		sharedPgRaw.ExecContext(ctx, `DELETE FROM objects WHERE hash = $1`, hash)
	}
	clean()
	t.Cleanup(clean)

	// The state a released object passes through on its way to being collected: present, claimed
	// by nobody, and already marked long enough ago that the sweep wants it.
	if _, err := sharedPgRaw.ExecContext(ctx,
		`INSERT INTO objects (hash, content, size, created_at, released_at) VALUES ($1, 'BODY', 4, 0, 0)`, hash); err != nil {
		t.Fatalf("seed object: %v", err)
	}

	writer, err := sharedPgRaw.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin writer: %v", err)
	}
	defer writer.Rollback()

	// Step one of the write, mirroring PutObject. Setting size to the value it already has is
	// not a no-op at the storage layer: it is the write that takes the row lock.
	if _, err := writer.ExecContext(ctx,
		`INSERT INTO objects (hash, content, size, created_at) VALUES ($1, 'BODY', 4, 0)
		 ON CONFLICT (hash) DO UPDATE SET size = excluded.size`, hash); err != nil {
		t.Fatalf("writer upsert: %v", err)
	}

	type sweepResult struct {
		n   int64
		err error
	}
	swept := make(chan sweepResult, 1)
	go func() {
		n, err := sharedPgDB.CollectObjects(dbpkg.Now().UnixMilli())
		swept <- sweepResult{n, err}
	}()

	// The sweep must WAIT. Without the lock it would not even pause before deleting content the
	// writer is about to claim.
	select {
	case r := <-swept:
		t.Fatalf("the sweep ran to completion while the writer held the row (deleted %d, err=%v): the content upsert took no lock", r.n, r.err)
	case <-time.After(300 * time.Millisecond):
	}

	// Step two: the claim, committed while the sweep waits.
	if _, err := writer.ExecContext(ctx,
		`INSERT INTO object_refs (hash, owner_kind, owner_id, created_at)
		 VALUES ($1, 'instance', 'lock-test', 0)`, hash); err != nil {
		t.Fatalf("writer claim: %v", err)
	}
	if err := writer.Commit(); err != nil {
		t.Fatalf("commit writer: %v", err)
	}

	select {
	case r := <-swept:
		if r.err != nil {
			t.Fatalf("sweep: %v", r.err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the sweep never unblocked after the writer committed")
	}

	// Waiting is only half of it. A sweep that wakes and deletes anyway -- because its predicate
	// was decided by a snapshot older than the claim -- leaves object_refs pointing at nothing.
	var objects, claims int
	if err := sharedPgRaw.QueryRowContext(ctx, `SELECT count(*) FROM objects WHERE hash = $1`, hash).Scan(&objects); err != nil {
		t.Fatalf("count objects: %v", err)
	}
	if err := sharedPgRaw.QueryRowContext(ctx, `SELECT count(*) FROM object_refs WHERE hash = $1`, hash).Scan(&claims); err != nil {
		t.Fatalf("count claims: %v", err)
	}
	if claims != 1 {
		t.Fatalf("setup: %d claims, want the writer's 1", claims)
	}
	if objects != 1 {
		t.Fatalf("dangling reference: the claim survived and its content did not (objects=%d, claims=%d). The sweep waited on the row lock and then deleted on a snapshot taken before the claim committed", objects, claims)
	}
}

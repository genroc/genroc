package dbtest

import (
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	dbpkg "genroc/internal/db"
	"genroc/internal/model"
)

// A log's object and its claim must be written in ONE transaction, so no observer -- and no
// sweep -- can ever see the object unclaimed.
//
// Unlike the resurrection race this needs no luck to hit. The log path wrote its content and its
// claim as two autocommit statements, leaving the object committed and held by nobody in
// between; a sweep landing there deletes content the claim about to commit will point at, and
// SQLite's single writer does not help, because two autocommit statements are two transactions
// on any engine. Polling for "an object nobody claims" caught the gap 452 times across 200 log
// writes -- more than twice per write.
//
// Postgres only for the instrument, not the bug: the observer needs a second connection, and
// dbtest only keeps a raw one for Postgres. The fix (db.withTx) is engine-agnostic.
// specs/object-store.md.
func TestLogObjects_ContentAndClaimAreWrittenAtomically(t *testing.T) {
	if sharedPgDB == nil || sharedPgRaw == nil {
		t.Skip("needs POSTGRES_DSN for the second connection the observer needs")
	}
	ctx := t.Context()
	clean := func() {
		sharedPgRaw.ExecContext(ctx, `DELETE FROM object_refs`)
		sharedPgRaw.ExecContext(ctx, `DELETE FROM objects`)
	}
	clean()
	t.Cleanup(clean)

	var unclaimed atomic.Int64
	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			var n int
			if err := sharedPgRaw.QueryRowContext(ctx, `SELECT count(*) FROM objects o
				WHERE NOT EXISTS (SELECT 1 FROM object_refs r WHERE r.hash = o.hash)`).Scan(&n); err == nil && n > 0 {
				unclaimed.Add(1)
			}
		}
	}()

	const writes = 200
	for i := range writes {
		// Distinct content per write, each over the target so it cuts to its own object.
		payload := map[string]any{"code": strings.Repeat(fmt.Sprintf("L%03d", i), 1200)}
		entry := &model.LogEntry{InstanceID: fmt.Sprintf("inst-%d", i), Level: model.LogInfo, Event: "probe"}
		if err := sharedPgDB.AppendLogValue(entry, payload, 128); err != nil {
			t.Fatalf("AppendLogValue: %v", err)
		}
	}
	close(stop)
	wg.Wait()

	if n := unclaimed.Load(); n != 0 {
		t.Errorf("saw an object with no claim %d times across %d log writes: the content write and its claim are not one transaction, so a sweep landing between them deletes content the claim will point at", n, writes)
	}
}

// A log's object lives exactly as long as its log ROW, and then gets the grace window.
//
// The claim's owner IS the row (`(hash, 'log', <log id>)`), so nothing about the object's life is
// expressed in time: prune the row and the sweep notices the owner is gone, releases the claim
// and stamps grace, which is the same release an instance value gets. Before this the owner was
// the INSTANCE, so a row's deletion said nothing about whether the claim was still wanted, and
// the object's life had to be guessed with a retention horizon instead.
// specs/object-store.md.
func TestLogObjects_LiveAsLongAsTheirRowThenGetGrace(t *testing.T) {
	for _, b := range testBackends(t) {
		t.Run(b.name, func(t *testing.T) {
			b.db.SetObjectGrace(24 * time.Hour)

			entry := &model.LogEntry{InstanceID: "inst-grace", Level: model.LogInfo, Event: "probe"}
			if err := b.db.AppendLogValue(entry, map[string]any{"code": strings.Repeat("G", 4096)}, 128); err != nil {
				t.Fatalf("AppendLogValue: %v", err)
			}
			logs, _, err := b.db.ListLogs("inst-grace", dbpkg.LogQuery{})
			if err != nil || len(logs) != 1 {
				t.Fatalf("ListLogs: %d rows, err=%v", len(logs), err)
			}
			var env model.Envelope
			if err := json.Unmarshal([]byte(logs[0].Data), &env); err != nil {
				t.Fatalf("decode log envelope: %v", err)
			}
			if len(env.Refs) != 1 {
				t.Fatalf("expected the payload to externalize one leaf, got %d", len(env.Refs))
			}
			ref := env.Refs[0]

			// While the row lives the object is untouchable, however long that is -- no horizon
			// to run out, so "keep logs forever" needs no sentinel.
			if _, err := b.db.CollectObjects(nowPlusHours(100000)); err != nil {
				t.Fatalf("CollectObjects: %v", err)
			}
			if _, _, err := b.db.GetObjectContent(ref.Ref); err != nil {
				t.Fatalf("the object was collected while its log row still referenced it: %v", err)
			}

			// Prune the row: the next sweep finds the claim's owner gone and releases it, and
			// grace keeps a reference already handed out resolvable.
			if _, err := b.db.PruneLogs(nowPlusHours(1)); err != nil {
				t.Fatalf("PruneLogs: %v", err)
			}
			if _, err := b.db.CollectObjects(nowPlusHours(2)); err != nil {
				t.Fatalf("CollectObjects: %v", err)
			}
			if _, _, err := b.db.GetObjectContent(ref.Ref); err != nil {
				t.Fatalf("the object went with its row, giving a reference already handed out no window at all: %v", err)
			}

			// Past the window it goes, or a pruned log's objects accumulate forever.
			// +2h stamped grace to +26h, and the sweep drops a claim only once it is strictly
			// past, so the window closes after that.
			if _, err := b.db.CollectObjects(nowPlusHours(27)); err != nil {
				t.Fatalf("CollectObjects: %v", err)
			}
			if _, _, err := b.db.GetObjectContent(ref.Ref); err == nil {
				t.Fatal("the object outlived its row plus the grace window")
			}
		})
	}
}

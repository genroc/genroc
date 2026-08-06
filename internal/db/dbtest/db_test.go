package dbtest

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"os"
	"testing"
	"time"

	dbpkg "genroc/internal/db"
	"genroc/internal/model"
)

// sharedPgDB is opened once in TestMain and reused across all tests.
// nil when POSTGRES_DSN is not set. sharedPgRaw is a plain connection to the
// same database, used only to wipe tables between tests (the db package keeps
// its connection unexported, so the black-box tests open their own).
var (
	sharedPgDB  *dbpkg.DB
	sharedPgRaw *sql.DB
)

func TestMain(m *testing.M) {
	if dsn := os.Getenv("POSTGRES_DSN"); dsn != "" {
		pg, err := dbpkg.OpenPostgres(dsn, 0)
		if err != nil {
			log.Fatalf("open postgres for tests: %v", err)
		}
		sharedPgDB = pg
		defer pg.Close()

		// The stress tests use sharedPgDB directly and only wipe process_instances, but their
		// fixtures reference process "test" v1 — and the `stress` CI job runs -run TestStress, which
		// skips every testBackends test that would otherwise register it.
		def := &model.ProcessDefinition{Name: "test", Tasks: []*model.Task{{ID: "step1"}}}
		if err := pg.SaveDefinition(def, 1, nil, "test-hash", ""); err != nil {
			log.Fatalf("register baseline definition for stress tests: %v", err)
		}

		raw, err := sql.Open("postgres", dsn)
		if err != nil {
			log.Fatalf("open raw postgres for tests: %v", err)
		}
		sharedPgRaw = raw
		defer raw.Close()
	}
	os.Exit(m.Run())
}

type backend struct {
	db   *dbpkg.DB
	name string
}

// testBackends returns one backend per available driver.
// SQLite always runs using a fresh temp file.
// PostgreSQL runs when POSTGRES_DSN is set; tables are wiped between tests.
func testBackends(t *testing.T) []backend {
	t.Helper()

	f, err := os.CreateTemp("", "genroc-test-*.db")
	if err != nil {
		t.Fatal(err)
	}
	f.Close()
	sqlite, err := dbpkg.OpenSQLite(f.Name(), "")
	if err != nil {
		os.Remove(f.Name())
		t.Fatal(err)
	}
	t.Cleanup(func() { sqlite.Close(); os.Remove(f.Name()) })

	out := []backend{{sqlite, "sqlite"}}

	if sharedPgDB != nil {
		ctx := context.Background()
		// process_dependencies has an FK to process_definitions, so it must be
		// cleared first to avoid a constraint violation on Postgres.
		for _, table := range []string{"process_logs", "process_objects", "process_signals", "process_dependencies", "process_instances", "process_channels", "process_definitions"} {
			if _, err := sharedPgRaw.ExecContext(ctx, "DELETE FROM "+table); err != nil {
				t.Fatalf("reset %s: %v", table, err)
			}
		}
		out = append(out, backend{sharedPgDB, "postgres"})
	}

	// Register the baseline definition the fixtures reference (process "test" v1 with a
	// single task "step1"). Instances store only their current task id now, so the task
	// list is resolved from the definition — a fixture with no backing definition is
	// malformed. Tests needing different task shapes (e.g. only_once) register their own.
	for _, b := range out {
		saveDef(t, b.db, "test", 1, []*model.Task{{ID: "step1"}})
	}

	return out
}

// saveDef registers a process definition so instance fixtures referencing
// (name, version) can resolve their current task.
func saveDef(t *testing.T, db *dbpkg.DB, name string, version int, tasks []*model.Task) {
	t.Helper()
	def := &model.ProcessDefinition{Name: name, Tasks: tasks}
	if err := db.SaveDefinition(def, version, nil, name+"-hash", ""); err != nil {
		t.Fatalf("saveDef %q v%d: %v", name, version, err)
	}
}

func insertRunning(t *testing.T, db *dbpkg.DB, id string) {
	t.Helper()
	inst := &model.ProcessInstance{
		ID:             id,
		ProcessName:    "test",
		ProcessVersion: 1,
		Task:           "",
		ContextData:    map[string]any{},
		Status:         model.StatusRunning,
	}
	if err := db.SaveInstance(inst); err != nil {
		t.Fatalf("SaveInstance: %v", err)
	}
}

// TestClaimInstances_Basic verifies that an unclaimed instance is returned with
// the claiming worker's ID and a set lease expiry (RETURNING gives post-update state).
func TestClaimInstances_Basic(t *testing.T) {
	for _, b := range testBackends(t) {
		t.Run(b.name, func(t *testing.T) {
			insertRunning(t, b.db, "inst-1")

			got, err := b.db.ClaimInstances("worker-A", 10*time.Second, 10, dbpkg.AllowTakeover())
			if err != nil {
				t.Fatalf("ClaimInstances: %v", err)
			}
			if len(got) != 1 {
				t.Fatalf("expected 1 instance, got %d", len(got))
			}
			if got[0].WorkerID == nil || *got[0].WorkerID != "worker-A" {
				t.Errorf("expected WorkerID=worker-A, got %v", got[0].WorkerID)
			}
			if got[0].LeaseExpiresAt == nil {
				t.Error("expected lease_expires_at to be set")
			}
		})
	}
}

// TestClaimInstances_SkipsLiveLease verifies that a second worker cannot steal
// an instance whose lease has not yet expired.
func TestClaimInstances_SkipsLiveLease(t *testing.T) {
	for _, b := range testBackends(t) {
		t.Run(b.name, func(t *testing.T) {
			insertRunning(t, b.db, "inst-1")

			if _, err := b.db.ClaimInstances("worker-A", 10*time.Second, 10, dbpkg.AllowTakeover()); err != nil {
				t.Fatalf("first claim: %v", err)
			}

			got, err := b.db.ClaimInstances("worker-B", 10*time.Second, 10, dbpkg.AllowTakeover())
			if err != nil {
				t.Fatalf("second claim: %v", err)
			}
			if len(got) != 0 {
				t.Errorf("expected 0 instances (lease still live), got %d", len(got))
			}
		})
	}
}

// TestClaimInstances_ReclaimsExpiredLease verifies that after a lease expires a new
// worker can reclaim the instance.
func TestClaimInstances_ReclaimsExpiredLease(t *testing.T) {
	for _, b := range testBackends(t) {
		t.Run(b.name, func(t *testing.T) {
			insertRunning(t, b.db, "inst-1")

			if _, err := b.db.ClaimInstances("worker-A", 10*time.Millisecond, 10, dbpkg.AllowTakeover()); err != nil {
				t.Fatalf("first claim: %v", err)
			}

			time.Sleep(20 * time.Millisecond) // let the lease expire

			got, err := b.db.ClaimInstances("worker-B", 10*time.Second, 10, dbpkg.AllowTakeover())
			if err != nil {
				t.Fatalf("reclaim: %v", err)
			}
			if len(got) != 1 {
				t.Fatalf("expected 1 reclaimed instance, got %d", len(got))
			}
			if got[0].WorkerID == nil || *got[0].WorkerID != "worker-B" {
				t.Errorf("expected WorkerID=worker-B after reclaim, got %v", got[0].WorkerID)
			}
		})
	}
}

// TestClaimInstances_SkipTakeover verifies the claim mode a worker uses after it
// discovers it was not running: expired leases are left alone (their owners are about to
// repair them) while rows nobody holds are still picked up, so a worker in a grace window
// keeps working instead of idling.
func TestClaimInstances_SkipTakeover(t *testing.T) {
	for _, b := range testBackends(t) {
		t.Run(b.name, func(t *testing.T) {
			insertRunning(t, b.db, "a-held")    // claimed below, then left to expire
			insertRunning(t, b.db, "b-unowned") // never claimed

			// created_at ordering (then id) makes this deterministic: a-held is first.
			claimed, err := b.db.ClaimInstances("worker-A", 10*time.Millisecond, 1, dbpkg.AllowTakeover())
			if err != nil || len(claimed) != 1 || claimed[0].ID != "a-held" {
				t.Fatalf("setup claim: err=%v, got %d rows (%v)", err, len(claimed), claimed)
			}
			time.Sleep(20 * time.Millisecond) // let worker-A's lease expire

			got, err := b.db.ClaimInstances("worker-B", 10*time.Second, 10, dbpkg.SkipTakeover)
			if err != nil {
				t.Fatalf("SkipTakeover claim: %v", err)
			}
			if len(got) != 1 || got[0].ID != "b-unowned" {
				ids := make([]string, len(got))
				for i, g := range got {
					ids[i] = g.ID
				}
				t.Fatalf("SkipTakeover claimed %v, want only the unowned row", ids)
			}
			if got[0].ReclaimedExpired {
				t.Error("an unowned row was reported as a lease takeover")
			}

			// The expired row is still there, still carrying its owner, and is taken on
			// the next ordinary claim — the grace delays a takeover, it does not cancel it.
			got, err = b.db.ClaimInstances("worker-B", 10*time.Second, 10, dbpkg.AllowTakeover())
			if err != nil {
				t.Fatalf("AllowTakeover claim: %v", err)
			}
			if len(got) != 1 || got[0].ID != "a-held" {
				t.Fatalf("AllowTakeover claimed %d rows, want the expired a-held", len(got))
			}
			if !got[0].ReclaimedExpired {
				t.Error("expected ReclaimedExpired on a row taken over from another worker")
			}
		})
	}
}

// TestRenewLease_Extends verifies that a successful renewal pushes the expiry
// far enough forward that a competing worker cannot reclaim the instance.
func TestRenewLease_Extends(t *testing.T) {
	for _, b := range testBackends(t) {
		t.Run(b.name, func(t *testing.T) {
			insertRunning(t, b.db, "inst-1")

			if _, err := b.db.ClaimInstances("worker-A", 30*time.Millisecond, 10, dbpkg.AllowTakeover()); err != nil {
				t.Fatalf("claim: %v", err)
			}

			time.Sleep(20 * time.Millisecond)

			if _, err := b.db.RenewWorkerLeases("worker-A", []string{"inst-1"}, time.Second); err != nil {
				t.Fatalf("RenewWorkerLeases: %v", err)
			}

			time.Sleep(20 * time.Millisecond) // original lease would have expired here

			got, err := b.db.ClaimInstances("worker-B", 10*time.Second, 10, dbpkg.AllowTakeover())
			if err != nil {
				t.Fatalf("competitor claim: %v", err)
			}
			if len(got) != 0 {
				t.Errorf("expected 0 instances after successful renewal, got %d", len(got))
			}
		})
	}
}

// The contract the stale-lease gate reads renewals through: the returned instant is the one
// the expiries were derived from. A caller dating the evidence from after the write credits
// its leases with the write's duration, then hands out cutoffs reaching past lapsed leases.
func TestRenewLease_ReportsWhatItWrote(t *testing.T) {
	for _, b := range testBackends(t) {
		t.Run(b.name, func(t *testing.T) {
			// Over the renewal's chunk size, so the pass is several transactions: the
			// discrepancy this test is about is the duration of the write, and one row
			// renews inside a millisecond on both engines.
			const rows = 1000
			ids := make([]string, 0, rows)
			for i := 0; i < rows; i++ {
				id := fmt.Sprintf("inst-%03d", i)
				insertRunning(t, b.db, id)
				ids = append(ids, id)
			}
			if claimed, err := b.db.ClaimInstances("worker-A", 30*time.Millisecond, rows, dbpkg.AllowTakeover()); err != nil || len(claimed) != rows {
				t.Fatalf("claim: err=%v, count=%d", err, len(claimed))
			}

			const leaseDur = time.Second
			renewedAt, err := b.db.RenewWorkerLeases("worker-A", ids, leaseDur)
			if err != nil {
				t.Fatalf("RenewWorkerLeases: %v", err)
			}

			inst, err := b.db.GetInstance("inst-000")
			if err != nil {
				t.Fatalf("GetInstance: %v", err)
			}
			if want := renewedAt.Add(leaseDur); !inst.LeaseExpiresAt.Equal(want) {
				t.Errorf("renewal reported %v, so the lease should run to %v; it runs to %v — a gate trusting the reported instant would think the lease alive for %v longer than it is",
					renewedAt, want, *inst.LeaseExpiresAt, want.Sub(*inst.LeaseExpiresAt))
			}
		})
	}
}

// TestClaimInstances_CutoffIsTheCallersNotTheClaims verifies that a caller's takeover
// cutoff is honoured verbatim: a lease that expires between the caller deciding and this
// claim running is not swept in. That is what lets a worker holding leases of its own claim
// safely however long it is delayed in between — see Engine.leaseGate.
func TestClaimInstances_CutoffIsTheCallersNotTheClaims(t *testing.T) {
	for _, b := range testBackends(t) {
		t.Run(b.name, func(t *testing.T) {
			insertRunning(t, b.db, "inst-1")
			if _, err := b.db.ClaimInstances("worker-A", 30*time.Millisecond, 10, dbpkg.AllowTakeover()); err != nil {
				t.Fatalf("claim: %v", err)
			}

			cutoff := dbpkg.TakeoverBefore(dbpkg.Now()) // decided while worker-A's lease is alive
			time.Sleep(50 * time.Millisecond)           // ...and acted on after it has expired

			got, err := b.db.ClaimInstances("worker-B", 10*time.Second, 10, cutoff)
			if err != nil {
				t.Fatalf("claim: %v", err)
			}
			if len(got) != 0 {
				t.Errorf("claimed %d rows against a cutoff older than their expiry; the cutoff must bound the claim, not be re-derived from its own clock", len(got))
			}
		})
	}
}

// TestRenewLease_WrongWorker verifies that renewal by a non-owner is a no-op.
func TestRenewLease_WrongWorker(t *testing.T) {
	for _, b := range testBackends(t) {
		t.Run(b.name, func(t *testing.T) {
			insertRunning(t, b.db, "inst-1")

			if _, err := b.db.ClaimInstances("worker-A", 30*time.Millisecond, 10, dbpkg.AllowTakeover()); err != nil {
				t.Fatalf("claim: %v", err)
			}

			if _, err := b.db.RenewWorkerLeases("worker-Z", []string{"inst-1"}, time.Second); err != nil {
				t.Fatalf("RenewWorkerLeases (wrong worker): %v", err)
			}

			time.Sleep(40 * time.Millisecond)

			got, err := b.db.ClaimInstances("worker-B", 10*time.Second, 10, dbpkg.AllowTakeover())
			if err != nil {
				t.Fatalf("reclaim: %v", err)
			}
			if len(got) != 1 {
				t.Errorf("expected 1 instance after bad renewal, got %d", len(got))
			}
		})
	}
}

// TestUpdateInstance_ClearsLease verifies that UpdateInstance always releases the
// lease so the next worker can reclaim freely.
func TestUpdateInstance_ClearsLease(t *testing.T) {
	for _, b := range testBackends(t) {
		t.Run(b.name, func(t *testing.T) {
			insertRunning(t, b.db, "inst-1")

			claimed, err := b.db.ClaimInstances("worker-A", 10*time.Second, 10, dbpkg.AllowTakeover())
			if err != nil || len(claimed) != 1 {
				t.Fatalf("claim: err=%v, count=%d", err, len(claimed))
			}

			inst := claimed[0]
			inst.Status = model.StatusCompleted
			if err := b.db.UpdateInstance(inst); err != nil {
				t.Fatalf("UpdateInstance: %v", err)
			}

			row, err := b.db.GetInstance("inst-1")
			if err != nil {
				t.Fatalf("GetInstance: %v", err)
			}
			if row.WorkerID != nil {
				t.Errorf("expected worker_id=NULL after UpdateInstance, got %q", *row.WorkerID)
			}
			if row.LeaseExpiresAt != nil {
				t.Errorf("expected lease_expires_at=NULL after UpdateInstance, got %v", row.LeaseExpiresAt)
			}
		})
	}
}

package dbtest

import (
	"database/sql"
	"fmt"
	"net/url"
	"os"
	"sync"
	"testing"
	"time"

	dbpkg "genroc/internal/db"
)

// The fleet cold-start race: concurrent OpenPostgres against a brand-new database. Without
// the advisory lock serializing the post-migration bootstrap, the concurrent CREATE FUNCTION
// / ALTER TABLE fail with "tuple concurrently updated" and a worker dies on launch.
func TestConcurrentOpenPostgres(t *testing.T) {
	dsn := os.Getenv("POSTGRES_DSN")
	if dsn == "" {
		t.Skip("PostgreSQL not available (set POSTGRES_DSN)")
	}

	freshDSN, dropDB := freshDatabase(t, dsn)
	defer dropDB()

	const workers = 8
	var wg sync.WaitGroup
	errs := make([]error, workers)
	dbs := make([]*dbpkg.DB, workers)
	release := make(chan struct{})

	for i := range workers {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-release // unblock every goroutine at once to maximise the race window
			// Small pool so workers*pool stays well under max_connections.
			dbs[i], errs[i] = dbpkg.OpenPostgres(freshDSN, 2)
		}(i)
	}
	close(release)
	wg.Wait()

	for i, db := range dbs {
		if db != nil {
			db.Close()
		}
		if errs[i] != nil {
			t.Errorf("worker %d: concurrent OpenPostgres failed: %v", i, errs[i])
		}
	}
}

// freshDatabase creates a uniquely-named throwaway database on the same server as
// dsn and returns a DSN pointing at it plus a cleanup that drops it. CREATE/DROP
// DATABASE cannot run inside the target, so it connects to the "postgres"
// maintenance database. Skips the test if the role lacks CREATEDB.
func freshDatabase(t *testing.T, dsn string) (string, func()) {
	t.Helper()
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatalf("parse POSTGRES_DSN: %v", err)
	}
	// time-based name; digits only, so no injection risk in the CREATE/DROP below.
	name := fmt.Sprintf("genroc_bootstrap_%d", time.Now().UnixNano())

	admin := *u
	admin.Path = "/postgres"
	adminDSN := admin.String()

	adminDB, err := sql.Open("postgres", adminDSN)
	if err != nil {
		t.Fatalf("open maintenance db: %v", err)
	}
	defer adminDB.Close()
	if _, err := adminDB.Exec("CREATE DATABASE " + name); err != nil {
		t.Skipf("cannot create a throwaway database (role needs CREATEDB): %v", err)
	}

	fresh := *u
	fresh.Path = "/" + name
	return fresh.String(), func() {
		a, err := sql.Open("postgres", adminDSN)
		if err != nil {
			return
		}
		defer a.Close()
		// FORCE (Postgres 13+) terminates any lingering pooled connections so the
		// drop never blocks on a slow Close.
		a.Exec("DROP DATABASE IF EXISTS " + name + " WITH (FORCE)")
	}
}

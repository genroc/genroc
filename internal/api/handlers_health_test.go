package api

import (
	"net/http"
	"os"
	"testing"

	"genroc/internal/db"
)

// The 503 path needs a database that fails a ping, which means closing the pool — so this
// cannot use newTestHandlers, whose cleanup closes it a second time (db.Close closes a
// channel and would panic). The ok path is covered end-to-end in tests/integration.
func TestHealth_ReportsUnavailableWhenTheDatabaseIsGone(t *testing.T) {
	f, err := os.CreateTemp("", "genroc-health-*.db")
	if err != nil {
		t.Fatal(err)
	}
	f.Close()
	defer os.Remove(f.Name())

	database, err := db.OpenSQLite(f.Name(), "")
	if err != nil {
		t.Fatal(err)
	}
	// nil engine on purpose: the readiness verdict must be reached without consulting it,
	// so a worker whose database is gone still answers instead of panicking.
	h := NewHandlers(database, nil)
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}

	reply := h.health()

	if reply.OK {
		t.Fatalf("health reported ok with a closed database: %s", reply.Data)
	}
	if reply.Code != CodeUnavailable {
		t.Errorf("code = %q, want %q — a worker that cannot reach its database is not merely erroring, "+
			"it should be routed away from", reply.Code, CodeUnavailable)
	}
	if got := statusOf(reply.Code); got != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503: a readiness probe keys on the status, and 500 does not "+
			"take a worker out of rotation", got)
	}
}

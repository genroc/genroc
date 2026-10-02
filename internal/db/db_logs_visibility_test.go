package db

import (
	"path/filepath"
	"testing"
	"time"

	"genroc/internal/model"
)

func TestListLogs_WaitsForAnInFlightFlush(t *testing.T) {
	db, err := OpenSQLite(filepath.Join(t.TempDir(), "logs.db"), "OFF")
	if err != nil {
		t.Fatalf("OpenSQLite: %v", err)
	}
	defer db.Close()

	if err := db.AppendLog(&model.LogEntry{
		InstanceID: "inst-1",
		Level:      model.LogInfo,
		Event:      model.EventActionStarted,
	}); err != nil {
		t.Fatalf("AppendLog: %v", err)
	}

	// Stand in for a flusher parked between its detach and its insert.
	db.logFlushMu.Lock()
	batch := db.detachLogs()
	if len(batch) != 1 {
		t.Fatalf("detached %d rows, want the 1 just appended", len(batch))
	}

	type read struct {
		logs []*model.LogEntry
		err  error
	}
	done := make(chan read, 1)
	go func() {
		logs, _, err := db.ListLogs("inst-1", LogQuery{})
		done <- read{logs, err}
	}()

	select {
	case r := <-done:
		t.Fatalf("ListLogs returned %d rows while the batch holding the appended row was unwritten", len(r.logs))
	case <-time.After(100 * time.Millisecond):
	}

	if err := db.writeLogBatch(batch); err != nil {
		t.Fatalf("writeLogBatch: %v", err)
	}
	db.logFlushMu.Unlock()

	r := <-done
	if r.err != nil {
		t.Fatalf("ListLogs: %v", r.err)
	}
	if len(r.logs) != 1 {
		t.Fatalf("read that flushed for the appended row returned %d rows, want 1", len(r.logs))
	}
}

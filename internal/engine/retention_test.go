package engine

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"testing"
	"time"

	"genroc/internal/db"
	"genroc/internal/model"
)

// seedConcluded saves n completed roots, concluded at the current DB clock.
func seedConcluded(t *testing.T, database *db.DB, prefix string, n int) []string {
	t.Helper()
	if err := database.SaveDefinition(&model.ProcessDefinition{Name: "ret", Tasks: []*model.Task{{ID: "only"}}}, 1, nil, "ret-hash", "", ""); err != nil {
		t.Fatalf("SaveDefinition: %v", err)
	}
	ids := make([]string, n)
	for i := range n {
		ids[i] = fmt.Sprintf("%s-%02d", prefix, i)
		if err := database.SaveInstance(&model.ProcessInstance{
			ID: ids[i], ProcessName: "ret", ProcessVersion: 1, Task: "only",
			State: map[string]any{}, Status: model.StatusCompleted,
		}); err != nil {
			t.Fatalf("SaveInstance: %v", err)
		}
	}
	return ids
}

func remaining(t *testing.T, database *db.DB, ids []string) int {
	t.Helper()
	n := 0
	for _, id := range ids {
		if _, err := database.GetInstance(id); err == nil {
			n++
		}
	}
	return n
}

func advanceDBClock(t *testing.T, d time.Duration) {
	db.AdvanceClock(d)
	t.Cleanup(func() { db.AdvanceClock(-d) })
}

func quietEngine(database *db.DB, opts ...Option) *Engine {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	return New(database, 10*time.Millisecond, 4, true, 0, 0, LogConfig{}, log, opts...)
}

func TestPruneInstances_APassStopsAtItsCapAndResumes(t *testing.T) {
	database := openTestDB(t)
	ids := seedConcluded(t, database, "cap", 10)
	advanceDBClock(t, 2*time.Hour)
	eng := quietEngine(database, WithInstanceRetention(time.Hour))
	eng.pruneBatchTrees, eng.pruneBatches = 2, 3

	eng.pruneInstances()
	if got := remaining(t, database, ids); got != 4 {
		t.Fatalf("after one pass %d of 10 remain, want 4: a pass deletes at most batches × trees, so a backlog cannot stall the pump", got)
	}
	eng.pruneInstances()
	if got := remaining(t, database, ids); got != 0 {
		t.Fatalf("after the second pass %d remain, want 0: the next pass resumes where the cap stopped", got)
	}
}

func TestPruneInstances_OffDeletesNothing(t *testing.T) {
	database := openTestDB(t)
	ids := seedConcluded(t, database, "off", 3)
	advanceDBClock(t, 10*365*24*time.Hour)
	quietEngine(database).pruneInstances()
	if got := remaining(t, database, ids); got != 3 {
		t.Fatalf("%d of 3 remain with no retention set; deleting must be opt-in", got)
	}
}

// The pump, not only Tick, must run the prune: it is what every server without --poll 0 runs.
func TestPruneInstances_ThePumpRunsIt(t *testing.T) {
	database := openTestDB(t)
	ids := seedConcluded(t, database, "pump", 2)
	advanceDBClock(t, 2*time.Hour) // past the retention
	eng := quietEngine(database, WithInstanceRetention(time.Hour))
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { eng.Run(ctx); close(done) }()
	defer func() { cancel(); <-done }()

	// The pump schedules its first prune a minute after it starts, on the DB clock, at a moment
	// this goroutine cannot observe: nudging the clock each poll crosses it whenever that was.
	deadline := time.Now().Add(5 * time.Second)
	for remaining(t, database, ids) > 0 {
		if time.Now().After(deadline) {
			t.Fatal("the pump never pruned concluded processes past --instance-retention")
		}
		advanceDBClock(t, 2*time.Minute)
		time.Sleep(20 * time.Millisecond)
	}
}

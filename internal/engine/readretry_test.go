package engine

import (
	"errors"
	"strings"
	"testing"
	"time"

	"genroc/internal/model"
)

// A read that recovers is not a failure; a read that never recovers is, and it must say so
// rather than spin. Both halves matter: without the first a dropped connection terminally fails
// an instance a retry would have carried; without the second a dangling reference livelocks.
func TestRetryRead_RecoversFromABlipAndGivesUpOnAFault(t *testing.T) {
	boom := errors.New("connection refused")

	// Absolute numbers, deliberately: expressing these in terms of readAttempts makes the test
	// agree with whatever the constant says, which is how a test stops testing anything. Two
	// failures then a success must be ridden out whatever the constant is set to.
	t.Run("succeeds once the blip clears", func(t *testing.T) {
		calls := 0
		got, err := retryRead(func() (string, error) {
			calls++
			if calls <= 2 {
				return "", boom
			}
			return "value", nil
		})
		if err != nil || got != "value" {
			t.Fatalf("got %q err=%v after 2 failures; a blip must not become a terminal failure", got, err)
		}
		if calls != 3 {
			t.Errorf("read %d times, want 3", calls)
		}
	})

	t.Run("gives up and returns the fault", func(t *testing.T) {
		calls := 0
		_, err := retryRead(func() (string, error) {
			calls++
			return "", boom
		})
		if !errors.Is(err, boom) {
			t.Fatalf("err = %v, want the underlying fault so the instance fails with a reason", err)
		}
		// Bounded by a small number, not by the constant: a generous retry budget is the
		// livelock this exists to avoid, and it would hold the lease and a concurrency slot
		// while it ran.
		if calls > 5 {
			t.Errorf("read %d times before giving up; a real fault must fail promptly, not be retried at length", calls)
		}
		if calls < 2 {
			t.Errorf("read %d time(s); a single attempt is no retry at all", calls)
		}
	})

	t.Run("a read that works is not retried or delayed", func(t *testing.T) {
		calls := 0
		start := time.Now()
		if _, err := retryRead(func() (int, error) { calls++; return 1, nil }); err != nil {
			t.Fatalf("retryRead: %v", err)
		}
		if calls != 1 {
			t.Errorf("read %d times on the happy path, want 1", calls)
		}
		if elapsed := time.Since(start); elapsed > readRetryDelay {
			t.Errorf("the happy path slept %v; retries must cost nothing when nothing fails", elapsed)
		}
	})
}

// A reference to content that is gone is a real fault, not a blip: it fails identically every
// time. The instance must fail LOUDLY and promptly — the retries bound how long that takes.
func TestRetryRead_DanglingReferenceFailsTheInstanceLoudly(t *testing.T) {
	database := openTestDB(t)
	eng := tickEngine(t, database)

	process := "dangling-proc"
	if err := database.SaveDefinition(&model.ProcessDefinition{
		Name:  process,
		Tasks: []*model.Task{{ID: "read", Output: &model.Shape{Raw: map[string]any{"got": "$: input.blob"}}, Switch: model.SwitchMap{{Goto: model.GotoEnd}}}},
	}, 1, nil, process+"-hash", ""); err != nil {
		t.Fatalf("SaveDefinition: %v", err)
	}

	// A marker for content that was never written: the encode re-emits an existing reference
	// without storing anything, so the row points at an object that does not exist.
	id := "dangling-inst"
	if err := database.SaveInstance(&model.ProcessInstance{
		ID: id, ProcessName: process, ProcessVersion: 1, Task: "read", Status: model.StatusRunning,
		ContextData: map[string]any{"input": map[string]any{
			"blob": &model.ObjectRef{Ref: "0000000000000000000000000000dead", Size: 99},
		}},
	}); err != nil {
		t.Fatalf("SaveInstance: %v", err)
	}

	inst := claimOne(t, database, eng, id)
	start := time.Now()
	if err := eng.runAdvance(t.Context(), inst); err != nil {
		t.Fatalf("runAdvance: %v", err)
	}
	elapsed := time.Since(start)

	got, err := database.GetInstance(id)
	if err != nil {
		t.Fatalf("GetInstance: %v", err)
	}
	if got.Status != model.StatusFailed {
		t.Fatalf("status = %q, want failed — a permanently unreadable value must not be retried forever", got.Status)
	}
	if !strings.Contains(got.Error, "0000000000000000000000000000dead") {
		t.Errorf("the failure does not name the object it could not read: %q", got.Error)
	}
	// Bounded: readAttempts reads with readRetryDelay between them, not an open-ended wait.
	if max := time.Duration(readAttempts) * readRetryDelay * 4; elapsed > max {
		t.Errorf("the advance took %v (bound %v); the retries are not bounded", elapsed, max)
	}
}

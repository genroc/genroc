package idgen

import (
	"strings"
	"sync"
	"testing"
)

// An id is unique by construction, and its counter is what a row stores when its order has to
// survive a millisecond it shares with another row.
func TestIDsAreUniqueAndCarryTheirCounter(t *testing.T) {
	m, err := NewMinter(1)
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for i := 1; i <= 5000; i++ {
		id, seq := m.Next()
		if seen[id] {
			t.Fatalf("id %q was minted twice", id)
		}
		if seq != int64(i) {
			t.Fatalf("counter jumped: want %d beside %q, got %d", i, id, seq)
		}
		seen[id] = true
	}
}

// The counter is the only thing separating two ids from one worker, so it must never repeat,
// however fast or from however many goroutines ids are asked for.
func TestConcurrentMintsAreUnique(t *testing.T) {
	m, _ := NewMinter(3)
	var mu sync.Mutex
	seen := map[string]bool{}
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 1000 {
				id, _ := m.Next()
				mu.Lock()
				if seen[id] {
					t.Errorf("%q minted twice", id)
				}
				seen[id] = true
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
}

// A worker number is what makes an id unique without randomness, so two of them must never
// produce the same id -- which is also why the worker leads: no counter value can reach across.
func TestTwoWorkersNeverCollide(t *testing.T) {
	a, _ := NewMinter(1)
	b, _ := NewMinter(2)
	seen := map[string]bool{}
	for range 5000 {
		idA, _ := a.Next()
		idB, _ := b.Next()
		for _, id := range []string{idA, idB} {
			if seen[id] {
				t.Fatalf("two workers minted %q", id)
			}
			seen[id] = true
		}
	}
}

// Neither half is bounded: they are rendered side by side, not packed, so a worker number the
// id_counters row has grown to just makes a longer id. Only a negative one is refused.
func TestAWorkerNumberIsNotBounded(t *testing.T) {
	huge := int64(1) << 62
	m, err := NewMinter(huge)
	if err != nil {
		t.Fatalf("worker %d was refused, but nothing sizes the field: %v", huge, err)
	}
	id, _ := m.Next()
	if !strings.Contains(id, "-") {
		t.Errorf("id %q is not <worker>-<counter>", id)
	}
	if _, err := NewMinter(-1); err == nil {
		t.Error("a negative worker number was accepted")
	}
}

func TestTheAlphabetSurvivesBeingReadAloud(t *testing.T) {
	m, _ := NewMinter(1)
	for range 500 {
		id, _ := m.Next()
		if i := strings.IndexAny(id, "ilou"); i >= 0 {
			t.Fatalf("%q carries a character the alphabet excludes for legibility", id)
		}
		// `.` would split an external task's token, which is `<instance-id>.<task_epoch>`.
		if strings.Contains(id, ".") {
			t.Fatalf("%q carries a dot, which the token parser cuts on", id)
		}
	}
}

// A run of ids at one width sorts by accident, which is exactly the trap: the moment the
// counter outgrows its padding the order inverts. Pinned so nobody reintroduces an ordering
// assumption the format cannot carry -- `process_logs.seq` is where a trail's order lives.
func TestIDsStopSortingWhenTheWidthGrows(t *testing.T) {
	m, _ := NewMinter(1)
	m.counter.Store(1<<(5*counterMin) - 2) // the last id before the counter needs a fifth digit

	last, _ := m.Next()
	grown, _ := m.Next()
	if len(grown) <= len(last) {
		t.Fatalf("expected the counter to outgrow its padding: %q then %q", last, grown)
	}
	if grown > last {
		t.Errorf("%q sorts after %q; ids happen to be ordered here and nothing may depend on it",
			grown, last)
	}
}

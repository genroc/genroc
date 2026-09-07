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

// A wrapped worker number would mint ids another process owns, so it is refused rather than
// masked -- the one failure the scheme exists to rule out.
func TestAnOutgrownWorkerNumberIsRefused(t *testing.T) {
	if _, err := NewMinter(MaxWorker); err != nil {
		t.Errorf("the last usable worker number was refused: %v", err)
	}
	if _, err := NewMinter(MaxWorker + 1); err == nil {
		t.Error("a worker number past the field was accepted, and would collide silently")
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

// The rendering is short because it is unpadded, which is exactly why it does not sort. Pinned
// so nobody reintroduces an ordering assumption the format cannot carry: `process_logs.seq` is
// where a trail's order lives.
func TestIDsDoNotSort(t *testing.T) {
	m, _ := NewMinter(1)
	var ids []string
	for range 40 {
		id, _ := m.Next()
		ids = append(ids, id)
	}
	sorted := true
	for i := 1; i < len(ids); i++ {
		if ids[i] < ids[i-1] {
			sorted = false
		}
	}
	if sorted {
		t.Errorf("ids happen to sort in mint order (%v); nothing may come to depend on that", ids[:12])
	}
}

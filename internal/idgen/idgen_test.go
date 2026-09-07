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

// Neither number has a ceiling: the worker is coded suffix-free rather than given a field, so a
// process that starts after a million others just mints slightly longer ids. Only a negative
// worker number is refused.
func TestNeitherNumberIsBounded(t *testing.T) {
	for _, worker := range []int64{1, 1 << 10, 1 << 20, 1 << 30} {
		m, err := NewMinter(worker)
		if err != nil {
			t.Fatalf("worker %d was refused, but nothing sizes it: %v", worker, err)
		}
		if id, _ := m.Next(); len(id) < 2 {
			t.Errorf("worker %d minted %q", worker, id)
		}
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
		// A leading digit is what tells an id from a process name where both are read from
		// one positional (upgrade, compat).
		if id[0] < '0' || id[0] > '9' {
			t.Fatalf("%q does not start with a digit, so a process name could match it", id)
		}
		// `.` would split an external task's token, which is `<instance-id>.<task_epoch>`.
		if strings.Contains(id, ".") {
			t.Fatalf("%q carries a dot, which the token parser cuts on", id)
		}
	}
}

// The property hashing cannot give: distinct pairs render as distinct ids, always. Scattering
// is modular multiplication by a coprime -- a bijection inside each width -- and widths do not
// overlap, so the whole map is injective rather than merely unlikely to collide.
func TestTheRenderingIsInjective(t *testing.T) {
	seen := map[string]uint64{}
	for worker := range uint64(40) {
		for counter := uint64(1); counter <= 3000; counter++ {
			v := pair(worker, counter)
			id := render(scatter(v))
			if prev, dup := seen[id]; dup {
				t.Fatalf("%q renders both %d and %d", id, prev, v)
			}
			seen[id] = v
		}
	}
}

// Scattering is a permutation OF a width, not across widths: an id's length still follows the
// numbers behind it, which is what keeps short ids short.
func TestScatterStaysInsideItsWidth(t *testing.T) {
	for _, v := range []uint64{7, 1 << 10, 1 << 20, 1<<20 + 7, 1 << 25, 1<<30 + 12345, 1 << 40} {
		if got, want := len(render(scatter(v))), len(render(v)); got != want {
			t.Errorf("scatter(%d) renders %d chars, the value itself %d", v, got, want)
		}
	}
}

// Consecutive mints must not look consecutive, and the check has to cover EVERY position: a
// multiplicative scatter passed a first-characters test while leaving the whole tail frozen,
// because consecutive pairs differ by a constant stride and a multiply carries it through.
func TestNoCharacterPositionIsFrozenAcrossARun(t *testing.T) {
	m, _ := NewMinter(3)
	var ids []string
	for range 200 {
		id, _ := m.Next()
		ids = append(ids, id)
	}
	for pos := range len(ids[0]) {
		distinct := map[byte]bool{}
		for _, id := range ids {
			distinct[id[pos]] = true
		}
		if len(distinct) < 8 {
			t.Errorf("character %d takes only %d values across 200 consecutive ids (%v...): "+
				"the pair behind them is showing through", pos, len(distinct), ids[:4])
		}
	}
}

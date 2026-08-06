package delayspec

import (
	"fmt"
	"math/rand"
	"testing"
	"time"
)

// Randomised cross-checks of the pattern scheduler against the brute-force scan it replaced —
// where the carry cascade, the DST rules and the date walk meet in combinations nobody wrote
// down. The seed is fixed: an unreproducible once-a-year failure is worth re-running.
const randomSeed = 20260801

// Two suites, because one exclusion is unavoidable in a DST zone: a pattern naming a concrete
// hour can name a wall clock spring-forward deletes, and normalising forward lands on a time
// the pattern never named. With the hour left open the two sides agree again.

// Suite 1: zones with no transitions at all, so every shape of pattern is fair game —
// including the fully concrete clocks the DST suite has to leave out.
func TestInstant_RandomPatternsMatchAScan_FixedOffsetZones(t *testing.T) {
	for _, name := range []string{"UTC", "Asia/Kolkata", "+05:45"} {
		loc, err := LoadLocation(name)
		if err != nil {
			t.Skipf("%s unavailable: %v", name, err)
		}
		t.Run(name, func(t *testing.T) {
			rng := rand.New(rand.NewSource(randomSeed))
			for round := 0; round < 300; round++ {
				spec := randomClockSpec(rng, false)
				now := randomNow(rng, time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), 365*24*time.Hour)
				checkAgainstScan(t, spec, now, loc)
			}
		})
	}
}

// Suite 2: zones that transition, with `now` drawn from a two-day window around each
// transition so the repeated and the deleted hour are hit constantly rather than by luck.
// Santiago is here because it transitions at midnight — the repeat spans a date boundary,
// which the date walk and the repeat search have to agree about.
func TestInstant_RandomPatternsMatchAScan_AcrossDSTTransitions(t *testing.T) {
	for _, tc := range []struct {
		zone        string
		transitions []time.Time // UTC instants near which `now` is drawn
	}{
		{zone: "Europe/Prague", transitions: []time.Time{
			time.Date(2026, 3, 29, 1, 0, 0, 0, time.UTC),  // spring forward, 02:00 → 03:00 local
			time.Date(2026, 10, 25, 1, 0, 0, 0, time.UTC), // fall back, 03:00 → 02:00 local
		}},
		{zone: "America/Santiago", transitions: []time.Time{
			time.Date(2026, 4, 4, 4, 0, 0, 0, time.UTC), // fall back at midnight
			time.Date(2026, 9, 6, 4, 0, 0, 0, time.UTC), // spring forward at midnight
		}},
		{zone: "Australia/Sydney", transitions: []time.Time{
			time.Date(2026, 4, 4, 16, 0, 0, 0, time.UTC), // southern hemisphere, the other way round
			time.Date(2026, 10, 3, 16, 0, 0, 0, time.UTC),
		}},
	} {
		loc, err := LoadLocation(tc.zone)
		if err != nil {
			t.Skipf("%s unavailable: %v", tc.zone, err)
		}
		t.Run(tc.zone, func(t *testing.T) {
			rng := rand.New(rand.NewSource(randomSeed))
			for _, at := range tc.transitions {
				for round := 0; round < 200; round++ {
					spec := randomClockSpec(rng, true)
					now := randomNow(rng, at.Add(-24*time.Hour), 48*time.Hour)
					checkAgainstScan(t, spec, now, loc)
				}
			}
		})
	}
}

// checkAgainstScan resolves one pattern one way and then the other, and reports the
// disagreement in full: without the spec, the zone and the exact instant, a failure here is
// unreproducible.
func checkAgainstScan(t *testing.T, spec clockSpec, now time.Time, loc *time.Location) {
	t.Helper()
	i, err := ParseInstant(spec.text)
	if err != nil {
		t.Fatalf("ParseInstant(%q): %v", spec.text, err)
	}
	got, err := i.Resolve(now, loc)
	if err != nil {
		t.Fatalf("%q from %s in %s: %v", spec.text, now.In(loc).Format(wallFmt), loc, err)
	}
	// 26 hours covers even a once-a-day pattern from just after its daily match.
	want, ok := scanForMatch(now, 26*time.Hour, loc, spec.matches)
	if !ok {
		t.Fatalf("%q: the scan found no match within 26h of %s in %s", spec.text, now.In(loc).Format(wallFmt), loc)
	}
	if !got.Equal(want) {
		t.Errorf("%q in %s from %s:\n  resolved %s\n  scan     %s",
			spec.text, loc, now.In(loc).Format(wallFmt),
			got.In(loc).Format(wallFmt), want.In(loc).Format(wallFmt))
	}
}

// clockSpec is a generated pattern: the text handed to the parser, and an independently
// written predicate for the scan. The two are built from the same field draws but by
// different code — the point is that nothing but the draw is shared.
type clockSpec struct {
	text    string
	matches func(time.Time) bool
}

// randomClockSpec draws a clock pattern. openHour forces the hour field to "*", which is
// what keeps a DST zone comparable — see the note above the suites.
func randomClockSpec(rng *rand.Rand, openHour bool) clockSpec {
	h := randomField(rng, 23, openHour)
	m := randomField(rng, 59, false)
	s := randomField(rng, 59, false)
	return clockSpec{
		text: fmt.Sprintf("%s:%s:%s", h.text, m.text, s.text),
		matches: func(t time.Time) bool {
			return h.admits(t.Hour()) && m.admits(t.Minute()) && s.admits(t.Second())
		},
	}
}

type fieldSpec struct {
	text   string
	admits func(int) bool
}

// randomField draws one of the three shapes a clock field can take, weighted so that steps
// — the newest and least exercised shape — come up most often.
func randomField(rng *rand.Rand, max int, forceOpen bool) fieldSpec {
	if forceOpen {
		return fieldSpec{text: "*", admits: func(int) bool { return true }}
	}
	switch rng.Intn(10) {
	case 0, 1, 2:
		return fieldSpec{text: "*", admits: func(int) bool { return true }}
	case 3, 4:
		v := rng.Intn(max + 1)
		return fieldSpec{text: fmt.Sprintf("%02d", v), admits: func(got int) bool { return got == v }}
	default:
		step := 1 + rng.Intn(max) // 1..max, the range parseClockField accepts
		base := rng.Intn(max + 1)
		return fieldSpec{
			text:   fmt.Sprintf("%d/%d", base, step),
			admits: func(got int) bool { return got >= base && (got-base)%step == 0 },
		}
	}
}

// randomNow draws an instant in [from, from+window), on a whole second or a fractional one
// — the sub-second case is its own edge, since a match can only land on a whole second.
func randomNow(rng *rand.Rand, from time.Time, window time.Duration) time.Time {
	at := from.Add(time.Duration(rng.Int63n(int64(window))))
	if rng.Intn(2) == 0 {
		return at.Truncate(time.Second)
	}
	return at.Truncate(time.Second).Add(time.Duration(rng.Intn(1_000_000_000)))
}

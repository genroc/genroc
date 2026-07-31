package delayspec

import (
	"testing"
	"time"
)

// The calendar rules from the package comment, one test each. These are the cases that
// distinguish calendar arithmetic from millisecond arithmetic, and every one of them is a
// behaviour Go's own time package would get differently if left to its defaults.

// Rule 1: a calendar day keeps the *wall clock*, so its real length is 23, 24 or 25 hours
// depending on whether a DST transition falls inside it. "Same time tomorrow" is what a
// person means by "in a day"; "exactly 24 hours later" is not.
func TestDuration_ADayKeepsTheWallClockAcrossDST(t *testing.T) {
	loc := prague(t)
	for _, tc := range []struct {
		day         string
		now         string
		wantElapsed time.Duration
	}{
		{day: "the day before the clock springs forward", now: "2026-03-28 12:00", wantElapsed: 23 * time.Hour},
		{day: "the day before the clock falls back", now: "2026-10-24 12:00", wantElapsed: 25 * time.Hour},
		{day: "an ordinary day", now: "2026-06-10 12:00", wantElapsed: 24 * time.Hour},
	} {
		t.Run(tc.day, func(t *testing.T) {
			now := at(t, loc, tc.now)
			d, err := ParseDuration("1d")
			if err != nil {
				t.Fatal(err)
			}
			got := d.Resolve(now, loc).In(loc)

			if h, m, s := got.Clock(); h != 12 || m != 0 || s != 0 {
				t.Errorf("%s + 1d is %02d:%02d:%02d; a day should land on the same wall clock, 12:00:00", tc.now, h, m, s)
			}
			if elapsed := got.Sub(now); elapsed != tc.wantElapsed {
				t.Errorf("%s + 1d elapsed %v of real time; want %v", tc.now, elapsed, tc.wantElapsed)
			}
		})
	}
}

// With no tz the location is UTC, which has no transitions — so "a day is fixed without a
// tz" is not a separate rule, it falls out of the calendar one.
func TestDuration_ADayInUTCIsExactly24Hours(t *testing.T) {
	// The same date that is 23 hours long in Prague.
	now := at(t, time.UTC, "2026-03-28 12:00")
	d, err := ParseDuration("1d")
	if err != nil {
		t.Fatal(err)
	}
	if elapsed := d.Resolve(now, time.UTC).Sub(now); elapsed != 24*time.Hour {
		t.Errorf("1d in UTC elapsed %v; want exactly 24h", elapsed)
	}
}

// Rule 2: month arithmetic clamps to the end of the target month. Go's time.AddDate
// normalizes overflow instead, so it would roll Jan 31 + 1 month forward into March.
func TestDuration_MonthArithmeticClampsToTheEndOfTheMonth(t *testing.T) {
	for _, tc := range []struct {
		from string
		add  string
		want string
		why  string
	}{
		{from: "2026-01-31", add: "1mo", want: "2026-02-28", why: "February has no 31st; time.AddDate would give 2026-03-03"},
		{from: "2024-01-31", add: "1mo", want: "2024-02-29", why: "the clamp follows the leap year"},
		{from: "2026-01-31", add: "3mo", want: "2026-04-30", why: "clamping applies to the target month, not each month on the way"},
		{from: "2026-03-31", add: "1mo", want: "2026-04-30", why: "April has 30 days"},
		{from: "2024-02-29", add: "1y", want: "2025-02-28", why: "a leap day plus a year lands in a common year"},
		{from: "2026-05-15", add: "1mo", want: "2026-06-15", why: "a day every month has is untouched"},
	} {
		t.Run(tc.from+"+"+tc.add, func(t *testing.T) {
			from, err := time.Parse("2006-01-02", tc.from)
			if err != nil {
				t.Fatal(err)
			}
			got := resolveDuration(t, tc.add, from, time.UTC, "2006-01-02")
			if got != tc.want {
				t.Errorf("%s + %s = %s; want %s (%s)", tc.from, tc.add, got, tc.want, tc.why)
			}
		})
	}
}

// Rule 3: calendar components apply before fixed ones no matter which order they were
// written in, so a spec means the same thing however it was typed. Only a DST boundary can
// tell the two orders apart, which is why this is anchored on one.
func TestDuration_CalendarUnitsApplyBeforeFixedOnes(t *testing.T) {
	loc := prague(t)
	now := at(t, loc, "2026-03-28 12:00")

	// Written either way: cross the spring-forward boundary as a calendar day (landing back
	// on 12:00, having spent 23 real hours), then add 2 fixed hours.
	const want = "2026-03-29 14:00:00 +02:00"

	for _, spec := range []string{"1d 2h", "2h 1d"} {
		if got := resolveDuration(t, spec, now, loc, wallFmt); got != want {
			t.Errorf("2026-03-28 12:00 + %q = %s; want %s — the calendar day must apply first regardless of written order", spec, got, want)
		}
	}
}

// The two DST edges of building a wall clock, pinned here rather than inherited from
// time.Date, whose own documentation declines to guarantee the ambiguous case.

// Spring forward: 02:30 does not exist on 2026-03-29, where the clock jumps 02:00 → 03:00.
func TestResolveWall_NonexistentClockNormalizesForward(t *testing.T) {
	loc := prague(t)
	got := resolveWall(loc, 2026, time.March, 29, 2, 30, 0)
	if want := "2026-03-29 03:30:00 +02:00"; got.Format(wallFmt) != want {
		t.Errorf("02:30 on the spring-forward day resolved to %s; want %s (normalized forward past the gap)", got.Format(wallFmt), want)
	}
}

// Fall back: 02:30 happens twice on 2026-10-25, where the clock repeats 03:00 → 02:00. We
// take the first occurrence — the one still on summer time, +02:00.
func TestResolveWall_AmbiguousClockTakesTheFirstOccurrence(t *testing.T) {
	loc := prague(t)
	got := resolveWall(loc, 2026, time.October, 25, 2, 30, 0)
	if want := "2026-10-25 02:30:00 +02:00"; got.Format(wallFmt) != want {
		t.Errorf("the repeated 02:30 resolved to %s; want %s (the earlier of the two, still on summer time)", got.Format(wallFmt), want)
	}
	// The same input must keep choosing the same one of the two.
	if again := resolveWall(loc, 2026, time.October, 25, 2, 30, 0); !again.Equal(got) {
		t.Errorf("resolving the ambiguous clock twice gave %s then %s; it must be stable", got.Format(wallFmt), again.Format(wallFmt))
	}
}

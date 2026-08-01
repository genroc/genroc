package delayspec

import (
	"testing"
	"time"
)

// How a pattern's next match is found, and the two ways looking for one can end badly.
// A pattern's date is resolved by walking forward a day at a time rather than by arithmetic
// per field, which is what keeps "the 31st" and leap days correct with no special cases —
// and what makes termination something to test rather than assume. Its clock is the other
// way round: computed, never walked, because a wildcarded one matches up to 86 400 times a
// day. The last two tests here are about that half.

// The walk must skip a month that has no such day, never land on the normalized overflow
// date that arithmetic would produce (February 31st becoming March 3rd).
func TestInstant_PatternSkipsMonthsWithoutThatDay(t *testing.T) {
	for _, tc := range []struct {
		now  string
		want string
		why  string
	}{
		{now: "2026-02-01 12:00", want: "2026-03-31 08:00:00 +00:00", why: "February has no 31st, so the next one is in March"},
		{now: "2026-01-01 12:00", want: "2026-01-31 08:00:00 +00:00", why: "January does have one"},
		{now: "2026-04-15 12:00", want: "2026-05-31 08:00:00 +00:00", why: "April has only 30 days"},
	} {
		t.Run(tc.now, func(t *testing.T) {
			got := resolveInstant(t, "*-*-31 08:00", at(t, time.UTC, tc.now), time.UTC)
			if got != tc.want {
				t.Errorf("the next 31st after %s is %s; want %s (%s)", tc.now, got, tc.want, tc.why)
			}
		})
	}
}

// A month/day pair no year can ever satisfy is decidable without consulting a clock, so it
// fails at parse. Keeping it there is what makes registration deterministic: the
// alternative — discovering it during the resolve walk — would let the same definition
// validate or not depending on the day someone registered it.
func TestInstant_ImpossibleDateIsRejectedAtParse(t *testing.T) {
	if _, err := ParseInstant("*-02-30 08:00"); err == nil {
		t.Error("*-02-30 was accepted; February never has 30 days, in any year")
	}
	if _, err := ParseInstant("*-02-29 08:00"); err != nil {
		t.Errorf("*-02-29 was rejected, but leap years do have one: %v", err)
	}
}

// Unsatisfiability that only a calendar walk can see must terminate with an error rather
// than spin — the bounded search is the only thing standing between a typo and a hung
// worker.
func TestInstant_UnreachablePatternFailsInsteadOfSpinning(t *testing.T) {
	// The year is pinned to 2026 and 2026-01-01 is a Thursday, so "mon" can never match it.
	i, err := ParseInstant("mon 2026-01-01 08:00")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := i.Resolve(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), time.UTC); err == nil {
		t.Error("a pattern with no reachable match resolved successfully; it should fail once the search is exhausted")
	}
}

// Carry is where a stepped field differs from a wildcard: when a field runs out, the fields
// below it reset to their *base*, not to zero. A phase that survived the first tick and
// then quietly vanished at the minute boundary would be the natural bug here.
func TestInstant_SteppedFieldsCarryBackToTheirBase(t *testing.T) {
	for _, tc := range []struct {
		spec string
		now  string
		want string
		note string
	}{
		{spec: "*:*:0/5", now: "12:00:00", want: "12:00:05", note: "strictly after now, so not 12:00:00 itself"},
		{spec: "*:*:0/5", now: "12:00:01", want: "12:00:05", note: "a bound between two grid points rounds up"},
		{spec: "*:*:0/5", now: "12:00:04", want: "12:00:05"},
		{spec: "*:*:0/5", now: "12:00:55", want: "12:01:00", note: "the last tick of the minute carries into the next"},
		{spec: "*:*:0/5", now: "12:59:57", want: "13:00:00", note: "and through the hour"},
		{spec: "*:*:0/5", now: "23:59:57", want: "00:00:00", note: "and through midnight, onto the next day"},
		{spec: "*:*:2/5", now: "12:00:00", want: "12:00:02", note: "the phase, from before the first grid point"},
		{spec: "*:*:2/5", now: "12:00:02", want: "12:00:07"},
		{spec: "*:*:2/5", now: "12:00:52", want: "12:00:57", note: "57 = 2 + 11*5, the last point on the grid this minute"},
		{spec: "*:*:2/5", now: "12:00:57", want: "12:01:02", note: "the carry resets to the base 2, not to 0"},
		{spec: "*:2/5:00", now: "12:58:00", want: "13:02:00", note: "the same carry a level up"},
		{spec: "*:0/15:00", now: "12:07:00", want: "12:15:00"},
		{spec: "*:0/15:00", now: "12:50:00", want: "13:00:00"},
		{spec: "0/6:00:00", now: "19:00:00", want: "00:00:00", note: "no hour left today, so the next day's first"},
	} {
		t.Run(tc.spec+"_from_"+tc.now, func(t *testing.T) {
			now, err := time.Parse(time.RFC3339, "2026-07-31T"+tc.now+"Z")
			if err != nil {
				t.Fatal(err)
			}
			i, err := ParseInstant(tc.spec)
			if err != nil {
				t.Fatal(err)
			}
			got, err := i.Resolve(now, time.UTC)
			if err != nil {
				t.Fatal(err)
			}
			if clock := got.Format("15:04:05"); clock != tc.want {
				t.Errorf("from %s, %q resolved to %s; want %s (%s)", tc.now, tc.spec, clock, tc.want, tc.note)
			}
		})
	}
}

// The gotcha every stepped schedule inherits from cron: a step that does not divide its
// field's range leaves a short interval at the wrap. Pinned rather than fixed — "every 7
// seconds" cannot be both aligned to the minute and evenly spaced, and alignment is what
// the grammar promises.
func TestInstant_StepThatDoesNotDivideItsRangeWrapsShort(t *testing.T) {
	i, err := ParseInstant("*:*:0/7")
	if err != nil {
		t.Fatal(err)
	}
	from := time.Date(2026, 7, 31, 12, 0, 56, 0, time.UTC) // 56 = 7*8, the last tick
	got, err := i.Resolve(from, time.UTC)
	if err != nil {
		t.Fatal(err)
	}
	if want := "12:01:00"; got.Format("15:04:05") != want {
		t.Errorf("after the last tick of the minute, 0/7 resolved to %s; want %s", got.Format("15:04:05"), want)
	}
	if gap := got.Sub(from); gap != 4*time.Second {
		t.Errorf("the wrap gap is %v; want 4s — 60 is not a multiple of 7, and the grid restarts at :00", gap)
	}
}

// The property the feature exists for: repeated resolution stays on the grid. A drifting
// implementation passes every single-shot test above and still fails this one, because
// drift only shows up once the result is fed back in — which is exactly what a process loop
// does.
func TestInstant_RepeatedResolutionDoesNotDrift(t *testing.T) {
	i, err := ParseInstant("*:*:0/5")
	if err != nil {
		t.Fatal(err)
	}
	// Start deliberately off-grid, and with a fractional second: an implementation that
	// anchored on "now" instead of on the field's base would keep this offset forever.
	at := time.Date(2026, 7, 31, 12, 0, 3, 400*int(time.Millisecond), time.UTC)
	var prev time.Time
	for tick := 0; tick < 200; tick++ {
		next, err := i.Resolve(at, time.UTC)
		if err != nil {
			t.Fatal(err)
		}
		if next.Second()%5 != 0 || next.Nanosecond() != 0 {
			t.Fatalf("tick %d landed on %s; every tick must sit on a multiple of five seconds", tick, next.Format("15:04:05.000"))
		}
		if !prev.IsZero() && next.Sub(prev) != 5*time.Second {
			t.Fatalf("tick %d is %v after the previous one; want exactly 5s", tick, next.Sub(prev))
		}
		prev, at = next, next
	}
}

// The cascade checked against the obvious implementation it replaced: scan forward a second
// at a time and take the first second whose wall clock matches. The predicates below are
// written by hand rather than derived from the parse, so the two sides share nothing but
// the spec string.
//
// Every case widens at least one clock field. A pattern naming a single concrete clock is
// excluded on purpose: DST can make such a clock nonexistent, and resolveWall's documented
// answer is to normalize forward onto a time the pattern never named — which a scan cannot
// reproduce, and which the spring-forward test above pins instead.
func TestInstant_MatchesABruteForceScan(t *testing.T) {
	loc := prague(t)
	cases := []struct {
		spec   string
		window time.Duration
		match  func(time.Time) bool
	}{
		{spec: "*:*:*", window: time.Minute, match: func(time.Time) bool { return true }},
		{spec: "*:*:0/5", window: 2 * time.Hour, match: func(t time.Time) bool { return t.Second()%5 == 0 }},
		{spec: "*:*:2/5", window: 2 * time.Hour, match: func(t time.Time) bool { return t.Second() >= 2 && (t.Second()-2)%5 == 0 }},
		{spec: "*:0/15:00", window: 3 * 24 * time.Hour, match: func(t time.Time) bool { return t.Second() == 0 && t.Minute()%15 == 0 }},
		{spec: "*:*:00", window: 3 * 24 * time.Hour, match: func(t time.Time) bool { return t.Second() == 0 }},
		{spec: "0/6:30:00", window: 3 * 24 * time.Hour, match: func(t time.Time) bool {
			return t.Second() == 0 && t.Minute() == 30 && t.Hour()%6 == 0
		}},
		{spec: "*:30:00", window: 3 * 24 * time.Hour, match: func(t time.Time) bool {
			return t.Second() == 0 && t.Minute() == 30
		}},
		{spec: "mon *:0/30:00", window: 9 * 24 * time.Hour, match: func(t time.Time) bool {
			return t.Second() == 0 && t.Minute()%30 == 0 && t.Weekday() == time.Monday
		}},
	}
	// Ordinary instants, then the two DST transitions from every side: before, inside the
	// hour that repeats, and after.
	nows := []time.Time{
		time.Date(2026, 7, 31, 12, 0, 0, 0, time.UTC),
		time.Date(2026, 7, 31, 12, 0, 3, 400*int(time.Millisecond), time.UTC),
		time.Date(2026, 7, 31, 23, 59, 59, 0, time.UTC),
		time.Date(2026, 3, 29, 0, 45, 0, 0, time.UTC),  // 01:45 Prague, before the spring gap
		time.Date(2026, 3, 29, 1, 30, 0, 0, time.UTC),  // 03:30 Prague, after it
		time.Date(2026, 10, 25, 0, 30, 0, 0, time.UTC), // 02:30 CEST, first pass of the repeat
		time.Date(2026, 10, 25, 1, 30, 0, 0, time.UTC), // 02:30 CET, second pass
		time.Date(2026, 10, 25, 2, 30, 0, 0, time.UTC), // 03:30 CET, past it
	}

	for _, tc := range cases {
		i, err := ParseInstant(tc.spec)
		if err != nil {
			t.Fatalf("ParseInstant(%q): %v", tc.spec, err)
		}
		for _, now := range nows {
			t.Run(tc.spec+"_from_"+now.In(loc).Format(wallFmt), func(t *testing.T) {
				want, ok := scanForMatch(now, tc.window, loc, tc.match)
				if !ok {
					t.Fatalf("the scan found no match within %v; the case is mis-specified", tc.window)
				}
				got, err := i.Resolve(now, loc)
				if err != nil {
					t.Fatal(err)
				}
				if !got.Equal(want) {
					t.Errorf("resolved to %s; the scan says the first match after %s is %s",
						got.In(loc).Format(wallFmt), now.In(loc).Format(wallFmt), want.In(loc).Format(wallFmt))
				}
			})
		}
	}
}

// scanForMatch is the brute-force oracle: the first whole second strictly after now whose
// wall clock in loc satisfies match.
func scanForMatch(now time.Time, window time.Duration, loc *time.Location, match func(time.Time) bool) (time.Time, bool) {
	start := now.Truncate(time.Second).Add(time.Second)
	for at := start; at.Before(now.Add(window)); at = at.Add(time.Second) {
		if match(at.In(loc)) {
			return at, true
		}
	}
	return time.Time{}, false
}

// The two DST transitions, seen by a per-minute pattern — the case a schedule dense enough
// to land inside a transition can actually hit, unlike a once-a-day clock.
func TestInstant_ClockWildcardAcrossDSTTransitions(t *testing.T) {
	loc := prague(t)

	// Spring forward: the clock jumps 02:00 → 03:00, so 02:00 through 02:59 never happen.
	// The next per-minute match after 01:59 is 03:00 — which is also exactly 60 seconds of
	// real time later, so nothing is actually skipped.
	t.Run("spring forward", func(t *testing.T) {
		now := at(t, loc, "2026-03-29 01:59")
		got := resolveInstant(t, "*:*:00", now, loc)
		if want := "2026-03-29 03:00:00 +02:00"; got != want {
			t.Errorf("the next whole minute after %s is %s; want %s", now.Format(wallFmt), got, want)
		}
		parsed, _ := ParseInstant("*:*:00")
		at, _ := parsed.Resolve(now, loc)
		if elapsed := at.Sub(now); elapsed != time.Minute {
			t.Errorf("it is %v of real time away; want exactly 1m — the hour that vanished from the clock was never a minute of elapsed time", elapsed)
		}
	})

	// Fall back: the clock repeats 03:00 → 02:00, so every wall clock in that hour happens
	// twice. resolveWall names the first occurrence, and once now is past it the *second*
	// one is the next match. Getting this wrong is not a rounding error: a per-minute
	// schedule would go an hour without firing, once a year.
	t.Run("fall back", func(t *testing.T) {
		// Pinned through UTC, because parsing the wall clock "02:30" cannot say which of the
		// two it means — that is the whole ambiguity. Prague falls back at 01:00 UTC, so
		// 01:30 UTC is 02:30 on winter time: the second pass.
		secondPass := time.Date(2026, 10, 25, 1, 30, 0, 0, time.UTC).In(loc)
		if got := secondPass.Format(wallFmt); got != "2026-10-25 02:30:00 +01:00" {
			t.Fatalf("the second pass is %s; want 2026-10-25 02:30:00 +01:00", got)
		}
		got := resolveInstant(t, "*:*:00", secondPass, loc)
		if want := "2026-10-25 02:31:00 +01:00"; got != want {
			t.Errorf("during the repeated hour, the next whole minute after 02:30 is %s; want %s — "+
				"the second occurrence of 02:31, not an hour of silence", got, want)
		}
	})
}

// The reason the clock is computed rather than walked. This pattern's next match is 577
// days out and matches every second of every one of those days: a search that stepped the
// clock would visit ~50 million candidates to find it. The date walk visits 577.
//
// The bound is deliberately loose — the real figure is microseconds. It is here to fail on
// an implementation that scans, not to police a few milliseconds either way.
func TestInstant_DenseClockDoesNotSearchSecondBySecond(t *testing.T) {
	now := time.Date(2026, 7, 31, 12, 0, 0, 0, time.UTC)
	i, err := ParseInstant("*-02-29 *:*:*")
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	got, err := i.Resolve(now, time.UTC)
	if err != nil {
		t.Fatal(err)
	}
	elapsed := time.Since(start)

	if want := "2028-02-29 00:00:00 +00:00"; got.Format(wallFmt) != want {
		t.Errorf("resolved to %s; want %s — the first second of the next leap day", got.Format(wallFmt), want)
	}
	if elapsed > time.Second {
		t.Errorf("resolving took %v; a per-second pattern must not be searched a second at a time", elapsed)
	}
}

// A wall clock a spring-forward deleted normalizes forward, and "forward" has to hold
// wherever the gap sits. Santiago's is at midnight, and time.Date answers it with the
// previous day's 23:49 — trusting that direction cost a whole day before the cross-check
// caught it.
func TestInstant_DeletedWallClockNormalizesForwardEvenAtMidnight(t *testing.T) {
	for _, tc := range []struct {
		zone string
		spec string
		now  time.Time
		want string
		why  string
	}{
		{
			zone: "America/Santiago",
			spec: "*:49:21",
			now:  time.Date(2026, 9, 6, 3, 53, 40, 0, time.UTC), // 23:53:40 -04:00, minutes before the jump
			want: "2026-09-06 01:49:21 -03:00",
			why:  "the clock jumps 00:00 → 01:00, so 00:49:21 is carried past the gap — not back to the 5th",
		},
		{
			zone: "Europe/Prague",
			spec: "*-*-* 02:30",
			now:  time.Date(2026, 3, 29, 0, 0, 0, 0, time.UTC), // 01:00 CET, the jump is at 02:00
			want: "2026-03-29 03:30:00 +02:00",
			why:  "the same rule where the gap sits mid-morning, which is the case that always worked",
		},
	} {
		t.Run(tc.zone, func(t *testing.T) {
			loc, err := LoadLocation(tc.zone)
			if err != nil {
				t.Skipf("%s unavailable: %v", tc.zone, err)
			}
			if got := resolveInstant(t, tc.spec, tc.now, loc); got != tc.want {
				t.Errorf("%q from %s resolved to %s; want %s (%s)", tc.spec, tc.now.In(loc).Format(wallFmt), got, tc.want, tc.why)
			}
		})
	}
}

// Boundaries a schedule crosses on its own, with no zone involved: the ends of a minute, a
// day, a year, and a leap cycle.
func TestInstant_RollsOverEveryCalendarBoundary(t *testing.T) {
	for _, tc := range []struct {
		spec string
		now  string
		want string
		why  string
	}{
		{spec: "*:*:00", now: "2026-12-31 23:59:30", want: "2027-01-01 00:00:00", why: "into the next year"},
		{spec: "*:*:0/5", now: "2026-12-31 23:59:59", want: "2027-01-01 00:00:00", why: "the last second of the year"},
		{spec: "*-*-01 00:00:00", now: "2026-12-31 12:00:00", want: "2027-01-01 00:00:00", why: "a date pattern across the year end"},
		{spec: "*-02-29 12:00:00", now: "2026-07-31 12:00:00", want: "2028-02-29 12:00:00", why: "the next leap day, 577 days out"},
		{spec: "*-02-29 12:00:00", now: "2028-02-29 12:00:01", want: "2032-02-29 12:00:00", why: "a second past one leap day is four years to the next"},
		{spec: "*-*-31 23:59:59", now: "2026-01-31 23:59:59", want: "2026-03-31 23:59:59", why: "February and the short months have no 31st"},
	} {
		t.Run(tc.spec+"_from_"+tc.now, func(t *testing.T) {
			now, err := time.Parse("2006-01-02 15:04:05", tc.now)
			if err != nil {
				t.Fatal(err)
			}
			i, err := ParseInstant(tc.spec)
			if err != nil {
				t.Fatal(err)
			}
			got, err := i.Resolve(now, time.UTC)
			if err != nil {
				t.Fatal(err)
			}
			if stamp := got.Format("2006-01-02 15:04:05"); stamp != tc.want {
				t.Errorf("from %s, %q resolved to %s; want %s (%s)", tc.now, tc.spec, stamp, tc.want, tc.why)
			}
		})
	}
}

// Sub-second bounds. A match is always on a whole second, so the fractional part of now
// decides only whether the second it sits in still counts as ahead — it never does.
func TestInstant_SubSecondNowRoundsToTheNextWholeSecond(t *testing.T) {
	i, err := ParseInstant("*:*:0/5")
	if err != nil {
		t.Fatal(err)
	}
	base := time.Date(2026, 7, 31, 12, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		now  time.Duration
		want string
		why  string
	}{
		{now: 4*time.Second + 999999999, want: "12:00:05", why: "a nanosecond before the match is still before it"},
		{now: 5 * time.Second, want: "12:00:10", why: "exactly on a match: strictly after means the next one"},
		{now: 5*time.Second + 1, want: "12:00:10", why: "a nanosecond past it, the same"},
	} {
		t.Run(tc.want, func(t *testing.T) {
			got, err := i.Resolve(base.Add(tc.now), time.UTC)
			if err != nil {
				t.Fatal(err)
			}
			if clock := got.Format("15:04:05"); clock != tc.want {
				t.Errorf("from 12:00:00+%v resolved to %s; want %s (%s)", tc.now, clock, tc.want, tc.why)
			}
		})
	}
}

// A weekday and a date in the same pattern are an AND. cron makes the same pair an OR when
// both are restricted — "0 0 13 * 5" is every 13th *or* every Friday — which is the wart
// this grammar does not inherit.
func TestInstant_WeekdayAndDateAreAnAnd(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	got := resolveInstant(t, "mon *-*-13 09:00", now, time.UTC)
	if want := "2026-04-13 09:00:00 +00:00"; got != want {
		t.Errorf("the next Monday the 13th after %s is %s; want %s — an OR would have answered within days",
			now.Format("2006-01-02"), got, want)
	}
}

// The search is bounded at five years, and both sides of that bound are behaviour: inside
// it resolves, outside it fails rather than spinning.
func TestInstant_FiveYearBoundIsTheLimitOfTheSearch(t *testing.T) {
	// Both are patterns, not dated literals: a literal names its instant outright and the
	// walk never runs, so only a pattern can reach the bound at all.
	now := time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)
	within, err := ParseInstant("2030-*-01 08:00")
	if err != nil {
		t.Fatal(err)
	}
	if got, err := within.Resolve(now, time.UTC); err != nil {
		t.Errorf("a match four years out failed: %v", err)
	} else if want := "2030-01-01 08:00:00"; got.Format("2006-01-02 15:04:05") != want {
		t.Errorf("resolved to %s; want %s", got.Format("2006-01-02 15:04:05"), want)
	}

	beyond, err := ParseInstant("2032-*-01 08:00")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := beyond.Resolve(now, time.UTC); err == nil {
		t.Error("a match 6.5 years out resolved; past the bound the search must fail, not walk on")
	}
}

// An instant already behind now is a legitimate state, not an error. Timers keep running
// while an instance is paused, so an `until` written months ago can resolve into the past
// the moment it resumes — the caller clamps it to now, and delayspec must hand it over
// rather than fail.
func TestInstant_PastTargetResolvesWithoutError(t *testing.T) {
	i, err := ParseInstant("2020-01-01 08:00")
	if err != nil {
		t.Fatal(err)
	}
	got, err := i.Resolve(time.Date(2026, 7, 31, 12, 0, 0, 0, time.UTC), time.UTC)
	if err != nil {
		t.Fatalf("a target in the past must resolve, not error — pause/resume depends on it: %v", err)
	}
	if want := "2020-01-01 08:00:00 +00:00"; got.Format(wallFmt) != want {
		t.Errorf("resolved to %s; want the literal instant %s, unclamped", got.Format(wallFmt), want)
	}
}

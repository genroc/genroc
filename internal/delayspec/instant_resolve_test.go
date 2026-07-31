package delayspec

import (
	"testing"
	"time"
)

// How a pattern's next match is found, and the two ways looking for one can end badly.
// A pattern is resolved by walking forward a day at a time rather than by arithmetic per
// field, which is what keeps "the 31st" and leap days correct with no special cases — and
// what makes termination something to test rather than assume.

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

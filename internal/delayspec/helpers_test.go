package delayspec

import (
	"testing"
	"time"
)

// Every test in this package makes one of three kinds of statement: which strings a grammar
// accepts, what instant a spec lands on, or which of the two DST rules applies. The helpers
// here exist so a table row can say that in one readable line — a spec, a "now", and the
// wall clock it should land on.
//
// The files are split the same way:
//
//	location_test.go        what a `tz` slot may say
//	duration_parse_test.go  the `for` grammar
//	calendar_test.go        the calendar rules: DST, month ends, unit ordering
//	instant_parse_test.go   the three `until` forms
//	instant_resolve_test.go finding a pattern's next match
//	instant_random_test.go  the same, cross-checked against a brute-force scan on
//	                        generated patterns and instants

const (
	// wallFmt renders an instant with its UTC offset, for the cases where the zone is the
	// point ("+02:00" is CEST, "+01:00" is CET).
	wallFmt = "2006-01-02 15:04:05 -07:00"
	// millisFmt renders a UTC instant to millisecond precision, for the fixed-unit cases.
	millisFmt = "2006-01-02 15:04:05.000"
	// nowFmt is how a table row writes the "now" a spec resolves against.
	nowFmt = "2006-01-02 15:04"
)

// prague is the reference DST zone for these tests. In 2026 it springs forward on March 29
// (02:00→03:00) and falls back on October 25 (03:00→02:00) — the two transitions almost
// every calendar test turns on.
func prague(t *testing.T) *time.Location {
	t.Helper()
	loc, err := LoadLocation("Europe/Prague")
	if err != nil {
		t.Skipf("Europe/Prague unavailable in this environment: %v", err)
	}
	return loc
}

// at reads a "2006-01-02 15:04" wall clock in loc — the "now" a spec resolves against.
func at(t *testing.T, loc *time.Location, wall string) time.Time {
	t.Helper()
	now, err := time.ParseInLocation(nowFmt, wall, loc)
	if err != nil {
		t.Fatalf("at(%q): %v", wall, err)
	}
	return now
}

// resolveDuration parses a `for` spec and reports the wall clock it lands on, formatted
// with layout — so a table row can compare two human-readable strings.
func resolveDuration(t *testing.T, spec string, now time.Time, loc *time.Location, layout string) string {
	t.Helper()
	d, err := ParseDuration(spec)
	if err != nil {
		t.Fatalf("ParseDuration(%q): %v", spec, err)
	}
	return d.Resolve(now, loc).In(loc).Format(layout)
}

// resolveInstant is the `until` twin of resolveDuration.
func resolveInstant(t *testing.T, spec string, now time.Time, loc *time.Location) string {
	t.Helper()
	i, err := ParseInstant(spec)
	if err != nil {
		t.Fatalf("ParseInstant(%q): %v", spec, err)
	}
	got, err := i.Resolve(now, loc)
	if err != nil {
		t.Fatalf("Resolve(%q): %v", spec, err)
	}
	return got.In(loc).Format(wallFmt)
}

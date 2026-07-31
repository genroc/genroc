package delayspec

import (
	"strconv"
	"testing"
)

// The three closed forms an `until` literal may take, one test per form. Every row resolves
// against the same "now" so the tables can be read side by side.

// Friday, 2026-07-31, midday in Prague — on summer time, so the offset is +02:00 unless a
// row deliberately lands in winter.
const instantNow = "2026-07-31 12:00"

// Form 1: an absolute instant carries its own UTC offset, so it denotes a point in time
// before tz is ever consulted.
func TestInstant_AbsoluteFormCarriesItsOwnOffset(t *testing.T) {
	loc := prague(t)
	for _, tc := range []struct {
		spec string
		want string
		note string
	}{
		{spec: "2026-09-01T08:00:00+02:00", want: "2026-09-01 08:00:00 +02:00", note: "RFC 3339"},
		{
			spec: "2026-09-01T08:00:00+02:00[Europe/Prague]",
			want: "2026-09-01 08:00:00 +02:00",
			note: "RFC 9557: the annotation is validated, then dropped — the offset already fixes the instant",
		},
	} {
		t.Run(tc.spec, func(t *testing.T) {
			if got := resolveInstant(t, tc.spec, at(t, loc, instantNow), loc); got != tc.want {
				t.Errorf("%q resolved to %s; want %s (%s)", tc.spec, got, tc.want, tc.note)
			}
		})
	}
}

// A relaxed spelling carries no zone, so it is read as a wall clock in tz.
func TestInstant_WallClockFormIsReadInTZ(t *testing.T) {
	loc := prague(t)
	for _, tc := range []struct {
		spec string
		want string
		note string
	}{
		{spec: "2026-09-01 08:00", want: "2026-09-01 08:00:00 +02:00", note: "08:00 in Prague, not in UTC"},
		{spec: "2026-09-01", want: "2026-09-01 00:00:00 +02:00", note: "a bare date is midnight"},
		{spec: "2026-12-25 08:00", want: "2026-12-25 08:00:00 +01:00", note: "winter, so the same wall clock is a different offset"},
	} {
		t.Run(tc.spec, func(t *testing.T) {
			if got := resolveInstant(t, tc.spec, at(t, loc, instantNow), loc); got != tc.want {
				t.Errorf("%q resolved to %s; want %s (%s)", tc.spec, got, tc.want, tc.note)
			}
		})
	}
}

// Form 2: an offset and a clock — "in two days, at 08:00". The offset picks the day, the
// clock replaces the time of day.
func TestInstant_OffsetFormIsADurationThenAClock(t *testing.T) {
	loc := prague(t)
	for _, tc := range []struct {
		spec string
		want string
		note string
	}{
		{spec: "+2d 08:00", want: "2026-08-02 08:00:00 +02:00", note: "two days on, at 8 — the motivating example"},
		{spec: "+1d 23:30", want: "2026-08-01 23:30:00 +02:00"},
		{spec: "+1mo 08:00", want: "2026-08-31 08:00:00 +02:00", note: "the offset may be any duration"},
	} {
		t.Run(tc.spec, func(t *testing.T) {
			if got := resolveInstant(t, tc.spec, at(t, loc, instantNow), loc); got != tc.want {
				t.Errorf("from %s, %q resolved to %s; want %s (%s)", instantNow, tc.spec, got, tc.want, tc.note)
			}
		})
	}
}

// Form 3: a wildcarded date, a weekday, or both — the next match strictly after now.
func TestInstant_PatternFormFindsTheNextMatch(t *testing.T) {
	loc := prague(t)
	for _, tc := range []struct {
		spec string
		want string
		note string
	}{
		{spec: "*-*-01 08:00", want: "2026-08-01 08:00:00 +02:00", note: "the next 1st of a month at 8"},
		{spec: "*-*-* 08:00", want: "2026-08-01 08:00:00 +02:00", note: "today's 08:00 already passed, so it is tomorrow's"},
		{spec: "*-*-* 18:00", want: "2026-07-31 18:00:00 +02:00", note: "today's 18:00 is still ahead"},
		{spec: "mon 09:00", want: "2026-08-03 09:00:00 +02:00", note: "2026-07-31 is a Friday"},
		{spec: "*-12-25 00:00", want: "2026-12-25 00:00:00 +01:00", note: "months ahead, and into winter time"},
	} {
		t.Run(tc.spec, func(t *testing.T) {
			if got := resolveInstant(t, tc.spec, at(t, loc, instantNow), loc); got != tc.want {
				t.Errorf("from %s, %q resolved to %s; want %s (%s)", instantNow, tc.spec, got, tc.want, tc.note)
			}
		})
	}
}

func TestParseInstant_Rejects(t *testing.T) {
	for _, tc := range []struct {
		spec string
		why  string
	}{
		{spec: "", why: "empty"},
		{spec: "in two days", why: "natural language, which is deliberately not supported"},
		{spec: "next friday", why: "natural language again — `mon 09:00` is the supported spelling"},
		{spec: "+2d", why: "an offset with no clock; that is what `for` is for"},
		{spec: "*-*-01", why: "a pattern with no clock — it would name a whole day, not an instant"},
		{spec: "*-13-01 08:00", why: "there is no month 13"},
		{spec: "*-*-01 25:00", why: "there is no hour 25"},
		{spec: "2026-09-01 08:00[", why: "a malformed zone annotation"},
		{spec: "xyz 08:00", why: "neither a weekday nor a Y-M-D date"},
	} {
		t.Run(strconv.Quote(tc.spec), func(t *testing.T) {
			if _, err := ParseInstant(tc.spec); err == nil {
				t.Errorf("ParseInstant(%q) was accepted, but it is %s", tc.spec, tc.why)
			}
		})
	}
}

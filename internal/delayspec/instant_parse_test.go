package delayspec

import (
	"strconv"
	"strings"
	"testing"
	"time"
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

// A pattern may wildcard the clock as well as the date, which is the only way to say
// "every minute" — the resolution below a day is what the date fields cannot reach.
func TestInstant_ClockWildcardsNameASubDailySchedule(t *testing.T) {
	loc := prague(t)
	// A "now" with a live second and a stray millisecond, so the rows also pin that the
	// next match is the next *whole* second, never a rounded-down one already behind.
	now := at(t, loc, instantNow).Add(37*time.Second + 400*time.Millisecond)
	for _, tc := range []struct {
		spec string
		want string
		note string
	}{
		{spec: "*:*:*", want: "2026-07-31 12:00:38 +02:00", note: "every second: the next whole second, not 12:00:37 which is behind now"},
		{spec: "*:*:00", want: "2026-07-31 12:01:00 +02:00", note: "every whole minute — the cron `* * * * *`"},
		{spec: "*:*:30", want: "2026-07-31 12:01:30 +02:00", note: "this minute's :30 is already behind now, so it carries to the next minute"},
		{spec: "*:30:00", want: "2026-07-31 12:30:00 +02:00", note: "every hour at half past"},
		{spec: "*:00:00", want: "2026-07-31 13:00:00 +02:00", note: "every whole hour; this hour's has passed"},
		{spec: "12:*:00", want: "2026-07-31 12:01:00 +02:00", note: "every minute, but only during the 12th hour"},
		{spec: "mon *:*:00", want: "2026-08-03 00:00:00 +02:00", note: "a weekday still narrows a per-minute clock, from midnight"},
		{spec: "*-*-01 *:00:00", want: "2026-08-01 00:00:00 +02:00", note: "date and clock wildcards compose"},
	} {
		t.Run(tc.spec, func(t *testing.T) {
			if got := resolveInstant(t, tc.spec, now, loc); got != tc.want {
				t.Errorf("from %s+37.4s, %q resolved to %s; want %s (%s)", instantNow, tc.spec, got, tc.want, tc.note)
			}
		})
	}
}

// A step field: "base/step" — every step-th value, counted from the base. The base is the
// phase, which is the whole reason this spelling was taken from systemd rather than cron's
// "*/5", where the phase has nowhere to go.
func TestInstant_StepFieldsRepeatFromTheirBase(t *testing.T) {
	loc := prague(t)
	now := at(t, loc, instantNow) // 12:00:00 exactly
	for _, tc := range []struct {
		spec string
		want string
		note string
	}{
		{spec: "*:*:0/5", want: "2026-07-31 12:00:05 +02:00", note: "every five seconds, on the multiples"},
		{spec: "*:0/5:00", want: "2026-07-31 12:05:00 +02:00", note: "every five minutes"},
		{spec: "*:0/15:00", want: "2026-07-31 12:15:00 +02:00", note: "the quarter hour"},
		{spec: "0/6:00:00", want: "2026-07-31 18:00:00 +02:00", note: "every six hours: 00, 06, 12, 18 — 12:00 is now, so the next is 18"},
		{spec: "*:*:2/5", want: "2026-07-31 12:00:02 +02:00", note: "a base of 2 shifts the whole grid: :02, :07, :12 …"},
		{spec: "*:7/20:00", want: "2026-07-31 12:07:00 +02:00", note: "phase and step together"},
		{spec: "mon *:0/30:00", want: "2026-08-03 00:00:00 +02:00", note: "a weekday still narrows a stepped clock"},
		{spec: "*-*-01 0/6:00:00", want: "2026-08-01 00:00:00 +02:00", note: "date pattern and stepped clock compose"},
		{spec: "MON 09:00", want: "2026-08-03 09:00:00 +02:00", note: "weekday names are case-insensitive"},
		// The two ends of what a step may say. Neither is useful, both are legal, and the
		// arithmetic has to hold: a base past the first multiple, and a step so long the
		// field wraps before repeating.
		{spec: "*:*:57/5", want: "2026-07-31 12:00:57 +02:00", note: "a base above its own step: 57 only, then the minute wraps"},
		{spec: "*:*:0/59", want: "2026-07-31 12:00:59 +02:00", note: "the largest step a seconds field takes: :00 and :59"},
		{spec: "0/23:00:00", want: "2026-07-31 23:00:00 +02:00", note: "the same at the top of the hours field"},
	} {
		t.Run(tc.spec, func(t *testing.T) {
			if got := resolveInstant(t, tc.spec, now, loc); got != tc.want {
				t.Errorf("from %s, %q resolved to %s; want %s (%s)", instantNow, tc.spec, got, tc.want, tc.note)
			}
		})
	}
}

// cron's "*/5" is the spelling most people will reach for first, so it must be turned away
// with the right one rather than with "out of range".
func TestParseInstant_CronStepSpellingIsRejectedByName(t *testing.T) {
	_, err := ParseInstant("*:*:*/5")
	if err == nil {
		t.Fatal(`"*:*:*/5" was accepted; the base is written explicitly in this grammar`)
	}
	if !strings.Contains(err.Error(), "0/5") {
		t.Errorf("the error for cron's spelling is %q; it must name the spelling that works, %q", err, "0/5")
	}
}

// An omitted seconds field is :00, not a wildcard. Every pattern written before clock
// wildcards existed relies on it — "08:00" names one instant a day, and widening it to 60
// would silently turn every such schedule into a per-second one.
func TestInstant_OmittedSecondsStayZeroRatherThanWidening(t *testing.T) {
	loc := prague(t)
	now := at(t, loc, instantNow)
	if got, want := resolveInstant(t, "*:*", now, loc), "2026-07-31 12:01:00 +02:00"; got != want {
		t.Errorf("*:* resolved to %s; want %s — the seconds field defaults to :00, so this is every whole minute", got, want)
	}
	if got, want := resolveInstant(t, "*-*-* 18:00", now, loc), "2026-07-31 18:00:00 +02:00"; got != want {
		t.Errorf("18:00 resolved to %s; want %s", got, want)
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
		{spec: "+2d *:00", why: "a wildcard in the offset form: it names a day, so it must name one clock on it"},
		{spec: "+2d 0/5:00", why: "a step in the offset form, for the same reason"},
		{spec: "*:*:0/0", why: "a step of zero, which would name a single value infinitely often"},
		{spec: "*:*:0/-5", why: "a negative step"},
		{spec: "*:*:0/61", why: "a step past the field's range — it would match only its base, which is what writing the base says"},
		{spec: "*:*:60/5", why: "a base out of range"},
		{spec: "*:*:a/5", why: "a non-numeric base"},
		{spec: "*:*:0/b", why: "a non-numeric step"},
		{spec: "*:*:0/5/2", why: "two steps"},
		// A sign is never a clock reading, and strconv.Atoi would take both of these.
		{spec: "*:+5:00", why: "a signed field"},
		{spec: "*:*:0/+5", why: "a signed step"},
		{spec: "*-+2-01 08:00", why: "a signed date field"},
		{spec: "*: :00", why: "a blank field"},
		{spec: "*:*:1.5", why: "a fractional second"},
		// Two tokens of a kind is a spec meaning "either", which this grammar cannot say —
		// and silently keeping the last one would schedule something nobody asked for.
		{spec: "mon tue 09:00", why: "two weekdays"},
		{spec: "*-*-01 *-*-02 08:00", why: "two dates"},
		{spec: "*:*:*:*", why: "a fourth clock field"},
		{spec: "*-*-* *:*:60", why: "there is no second 60, wildcards or not"},
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

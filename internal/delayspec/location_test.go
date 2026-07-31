package delayspec

import (
	"strconv"
	"testing"
	"time"
)

// What a `tz` slot may say. One rule decides every case below: a zone is accepted if it
// names the same offset rule on every host and on every date. Anything whose meaning
// depends on the season, or on the host's zone database, is refused — otherwise the same
// stored definition would mean different things on different workers.

func TestLoadLocation_Accepts(t *testing.T) {
	for _, tc := range []struct {
		tz  string
		why string
	}{
		{tz: "", why: "an omitted tz means UTC"},
		{tz: "UTC", why: "UTC spelled out"},
		{tz: "Europe/Prague", why: "an IANA name — the intended spelling"},
		{tz: "+02:00", why: "a fixed offset"},
		{tz: "-05:30", why: "a negative half-hour offset"},
		{tz: "+0200", why: "the colonless offset spelling"},
	} {
		t.Run(strconv.Quote(tc.tz), func(t *testing.T) {
			if _, err := LoadLocation(tc.tz); err != nil {
				t.Errorf("LoadLocation(%q) rejected it, but %s: %v", tc.tz, tc.why, err)
			}
		})
	}
}

func TestLoadLocation_Rejects(t *testing.T) {
	for _, tc := range []struct {
		tz  string
		why string
	}{
		{tz: "CET", why: "an abbreviation: the same definition would mean +01:00 in winter and +02:00 in summer"},
		{tz: "EST", why: "an abbreviation, and one Go resolves from the host's own zone database"},
		{tz: "Local", why: "the worker's own zone — the definition would resolve differently per worker"},
		{tz: "Europe/Nowhere", why: "not a real IANA name"},
		{tz: "+99:00", why: "not a real offset"},
	} {
		t.Run(strconv.Quote(tc.tz), func(t *testing.T) {
			if _, err := LoadLocation(tc.tz); err == nil {
				t.Errorf("LoadLocation(%q) was accepted, but it is %s", tc.tz, tc.why)
			}
		})
	}
}

// A fixed offset must actually shift the clock, not merely parse.
func TestLoadLocation_FixedOffsetShiftsTheClock(t *testing.T) {
	for _, tc := range []struct {
		tz   string
		want string
	}{
		{tz: "+02:00", want: "2026-06-10 14:00:00 +02:00"},
		{tz: "+0200", want: "2026-06-10 14:00:00 +02:00"},
		{tz: "-05:30", want: "2026-06-10 06:30:00 -05:30"},
	} {
		t.Run(tc.tz, func(t *testing.T) {
			loc, err := LoadLocation(tc.tz)
			if err != nil {
				t.Fatal(err)
			}
			noon := time.Date(2026, 6, 10, 12, 0, 0, 0, time.UTC)
			if got := noon.In(loc).Format(wallFmt); got != tc.want {
				t.Errorf("12:00 UTC seen from %q is %s; want %s", tc.tz, got, tc.want)
			}
		})
	}
}

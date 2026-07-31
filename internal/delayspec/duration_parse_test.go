package delayspec

import (
	"strconv"
	"strings"
	"testing"
	"time"
)

// The `for` grammar, tested by what each spec *means* rather than by how it decomposes
// internally: every row resolves from one fixed anchor in UTC, where there are no DST
// transitions and a calendar day is exactly 24 hours. The calendar behaviour those same
// units carry under a real tz is the subject of calendar_test.go.

// anchor is a Wednesday in midsummer, far from any transition in any zone.
const anchor = "2026-06-10 12:00"

func TestParseDuration_UnitsMeanWhatTheySay(t *testing.T) {
	for _, tc := range []struct {
		spec string
		want string
		note string
	}{
		{spec: "500ms", want: "2026-06-10 12:00:00.500"},
		{spec: "30s", want: "2026-06-10 12:00:30.000"},
		{spec: "90m", want: "2026-06-10 13:30:00.000"},
		{spec: "2h30m", want: "2026-06-10 14:30:00.000", note: "components add up"},
		{spec: "1m30s", want: "2026-06-10 12:01:30.000"},
		{spec: "1d12h", want: "2026-06-12 00:00:00.000"},
		{spec: "1d 12h", want: "2026-06-12 00:00:00.000", note: "whitespace is optional"},
		{spec: "2w", want: "2026-06-24 12:00:00.000", note: "a week is seven days"},
		{spec: "3mo", want: "2026-09-10 12:00:00.000"},
		{spec: "1y", want: "2027-06-10 12:00:00.000", note: "a year is twelve months"},
	} {
		t.Run(tc.spec, func(t *testing.T) {
			now := at(t, time.UTC, anchor)
			got := resolveDuration(t, tc.spec, now, time.UTC, millisFmt)
			if got != tc.want {
				t.Errorf("%s + %q = %s; want %s", anchor, tc.spec, got, tc.want)
			}
		})
	}
}

// "ms" and "mo" both start with "m", so the unit table is matched longest-first. Without
// that, "5ms" would read as five minutes followed by a stray "s".
func TestParseDuration_MsAndMoWinOverM(t *testing.T) {
	for _, tc := range []struct {
		spec string
		want string
		is   string
	}{
		{spec: "5ms", want: "2026-06-10 12:00:00.005", is: "five milliseconds"},
		{spec: "5m", want: "2026-06-10 12:05:00.000", is: "five minutes"},
		{spec: "5mo", want: "2026-11-10 12:00:00.000", is: "five months"},
	} {
		t.Run(tc.spec, func(t *testing.T) {
			now := at(t, time.UTC, anchor)
			got := resolveDuration(t, tc.spec, now, time.UTC, millisFmt)
			if got != tc.want {
				t.Errorf("%q should be %s (%s), got %s", tc.spec, tc.is, tc.want, got)
			}
		})
	}
}

func TestParseDuration_Rejects(t *testing.T) {
	for _, tc := range []struct {
		spec string
		why  string
	}{
		{spec: "5000", why: "unitless — the exact ambiguity this syntax exists to remove"},
		{spec: "", why: "empty"},
		{spec: "h", why: "a unit with no count"},
		{spec: "5x", why: "not a unit"},
		{spec: "5mm", why: "not a unit, even though it starts like one"},
		{spec: "-5m", why: "signed — the grammar has no leading sign, and a delay never runs backwards"},
	} {
		t.Run(strconv.Quote(tc.spec), func(t *testing.T) {
			if _, err := ParseDuration(tc.spec); err == nil {
				t.Errorf("ParseDuration(%q) was accepted, but it is %s", tc.spec, tc.why)
			}
		})
	}
}

// A unitless number is the mistake an author actually makes, and either reading is
// plausible — so the error names both rather than only refusing.
func TestParseDuration_UnitlessErrorNamesBothReadings(t *testing.T) {
	_, err := ParseDuration("5000")
	if err == nil {
		t.Fatal("ParseDuration(\"5000\") should have been rejected")
	}
	for _, reading := range []string{"5000ms", "5000s"} {
		if !strings.Contains(err.Error(), reading) {
			t.Errorf("the error should offer %q as a reading, got: %v", reading, err)
		}
	}
}

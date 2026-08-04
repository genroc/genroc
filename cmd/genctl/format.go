package main

import (
	"fmt"
	"time"
)

// shortID returns a compact id tag for tree-log display: the id's random tail, not its
// timestamp-prefixed head, so a parent and same-millisecond child differ.
func shortID(id string) string {
	if len(id) > 6 {
		return id[len(id)-6:]
	}
	return id
}

// ── time formatting ─────────────────────────────────────────────────────────────

// parseTime parses an RFC3339(/Nano) timestamp and converts it to local time.
func parseTime(rfc string) (time.Time, bool) {
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339} {
		if t, err := time.Parse(layout, rfc); err == nil {
			return t.Local(), true
		}
	}
	return time.Time{}, false
}

// whenLayouts are the absolute forms --since/--until accept. They mirror delayspec's
// absolute/wall layouts so a timestamp is written the same way in a definition and on
// the command line; a form without a zone is read in the local one.
var whenLayouts = []string{
	time.RFC3339Nano,
	time.RFC3339,
	"2006-01-02T15:04:05",
	"2006-01-02 15:04:05",
	"2006-01-02T15:04",
	"2006-01-02 15:04",
	"2006-01-02",
}

// parseWhen converts a --since/--until value to the unix millis the API expects: a
// duration ("2h", "45m") counts back from now, an absolute timestamp is taken as written.
// flag names the flag being parsed, so the error points at the one the user typed.
//
// A bare integer is rejected rather than read as epoch millis — "--since 30" means half an
// hour to everyone who types it, and silently returning everything instead is worse than
// an error.
//
// A duration resolves against *this machine's* clock, while rows are stamped with the
// server's (db.nowMillis, which is time.Now plus a test-only offset). The two agree to
// within NTP skew in production, so "2h" is off by seconds at most. They do not agree when
// the server's clock has been shifted — db.AdvanceClock moves it by hours in the tick
// tests — so a duration against such a server selects a window the server never had. Pass
// an absolute timestamp there, or bound with the values a row actually reports.
func parseWhen(flag, s string) (int64, error) {
	if d, err := time.ParseDuration(s); err == nil {
		if d < 0 {
			d = -d
		}
		return time.Now().Add(-d).UnixMilli(), nil
	}
	for _, layout := range whenLayouts {
		if t, err := time.ParseInLocation(layout, s, time.Local); err == nil {
			return t.UnixMilli(), nil
		}
	}
	return 0, fmt.Errorf("invalid %s %q: want a duration (2h, 45m) or a timestamp (2006-01-02, 2006-01-02 15:04)", flag, s)
}

// shortTime renders a timestamp compactly for list columns: a relative age ("5m ago")
// within a week, else a short absolute "YY-MM-DD HH:MM". Unparseable input is unchanged.
func shortTime(rfc string) string {
	t, ok := parseTime(rfc)
	if !ok {
		return rfc
	}
	return relAge(t)
}

func relAge(t time.Time) string {
	d := time.Since(t)
	switch {
	case d < 0, d >= 7*24*time.Hour:
		return t.Format("06-01-02 15:04")
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd ago", int(d.Hours()/24))
	}
}

// longTime renders a full local timestamp with its relative age: "2006-01-02 15:04:05  (5m ago)".
func longTime(rfc string) string {
	t, ok := parseTime(rfc)
	if !ok {
		return rfc
	}
	return fmt.Sprintf("%s  (%s)", t.Format("2006-01-02 15:04:05"), relAge(t))
}

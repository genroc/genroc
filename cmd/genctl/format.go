package main

import (
	"fmt"
	"os"
	"strconv"
	"time"
)

// ── output width ────────────────────────────────────────────────────────────────

// logLineWidth is fixed, not read off the terminal: genctl links no terminal library. $COLUMNS
// overrides, and --json is never cut.
func logLineWidth() int {
	if n, err := strconv.Atoi(os.Getenv("COLUMNS")); err == nil && n > 0 {
		return n
	}
	return defaultLogWidth
}

// Wide enough for the 59-column row prefix plus a short payload, narrow enough to fit the
// terminal most trails are read in.
const defaultLogWidth = 120

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

// whenLayouts mirror delayspec's absolute/wall layouts, so a timestamp is written the same way in
// a definition and on the command line.
var whenLayouts = []string{
	time.RFC3339Nano,
	time.RFC3339,
	"2006-01-02T15:04:05",
	"2006-01-02 15:04:05",
	"2006-01-02T15:04",
	"2006-01-02 15:04",
	"2006-01-02",
}

// parseWhen counts a duration back from THIS machine's clock (wrong against a test-shifted
// server — pass a timestamp there). Bare integers are rejected.
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

// span renders a duration at one unit of precision: "25s", "4m", "3h", "2d".
func span(d time.Duration) string {
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd", int(d.Hours()/24))
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

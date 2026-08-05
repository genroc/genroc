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

// parseWhen converts --since/--until to unix millis: a duration counts back from THIS
// machine's clock (fine in production, wrong against a test-shifted server — pass an
// absolute timestamp there), a timestamp is taken as written. Bare integers are rejected.
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

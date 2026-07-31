// Package delayspec parses the human-facing literals of the delay action: `for` (a
// duration relative to arm time) and `until` (an instant), plus the `tz` slot both
// resolve against.
//
// It has no engine or database dependency, so the calendar edge cases — DST transitions,
// month-end overflow — are table-testable in isolation, which is where they belong.
//
// Three rules govern everything here:
//
//   - Calendar units (d, w, mo, y) are calendar arithmetic in the target location, so "1d"
//     means the same wall clock tomorrow — 23 or 25 hours across a DST boundary. With no
//     tz the location is UTC, which has no transitions, so "1d" is exactly 24h: the
//     "without tz they are fixed" rule falls out of the general case instead of being a
//     separate one. Fixed units (ms, s, m, h) are always absolute elapsed time.
//   - Calendar units apply before fixed ones, regardless of the order they were written
//     in. Only a DST boundary can tell the two orders apart, and fixing the order keeps a
//     spec's meaning independent of how it was typed.
//   - A wall clock DST makes nonexistent (spring forward) normalizes forward; one it makes
//     ambiguous (autumn fall-back) takes the first (earlier) occurrence.
//
// Resolution never fails on a target in the past — the caller clamps to now. That is
// forced by the pause design: timers keep running while an instance is paused, so an
// `until` can legitimately resolve behind now on resume and must not turn into an error.
package delayspec

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// maxPatternDays bounds the forward search for a calendar pattern's next match. Five
// years is far past any plausible schedule, and the bound is what stops a pattern that
// can never match (e.g. "*-02-30") from spinning.
const maxPatternDays = 366 * 5

// ---------------------------------------------------------------------------
// Locations
// ---------------------------------------------------------------------------

// LoadLocation resolves a `tz` slot to a location: an IANA name ("Europe/Prague"), the
// literal "UTC", or a fixed offset ("+02:00"). An empty tz is UTC.
//
// Abbreviations are rejected by design. "CET" denotes the wrong thing for half the year
// (CET vs CEST) and Go's time.LoadLocation resolves abbreviations from the host's zone
// database, so the same definition would mean different things on different workers.
// "Local" is rejected for the same reason.
func LoadLocation(tz string) (*time.Location, error) {
	tz = strings.TrimSpace(tz)
	if tz == "" || tz == "UTC" {
		return time.UTC, nil
	}
	if off, ok := parseFixedOffset(tz); ok {
		return time.FixedZone(tz, off), nil
	}
	if !strings.Contains(tz, "/") {
		return nil, fmt.Errorf("tz %q: use an IANA name (e.g. %q) or a fixed offset (e.g. %q); "+
			"abbreviations are ambiguous across DST and resolve differently per host", tz, "Europe/Prague", "+02:00")
	}
	loc, err := time.LoadLocation(tz)
	if err != nil {
		return nil, fmt.Errorf("tz %q: unknown IANA location", tz)
	}
	return loc, nil
}

// parseFixedOffset accepts ±HH, ±HH:MM and ±HHMM, returning the offset in seconds.
func parseFixedOffset(s string) (int, bool) {
	if len(s) < 2 || (s[0] != '+' && s[0] != '-') {
		return 0, false
	}
	sign, body := 1, s[1:]
	if s[0] == '-' {
		sign = -1
	}
	body = strings.Replace(body, ":", "", 1)
	if len(body) != 2 && len(body) != 4 {
		return 0, false
	}
	for i := 0; i < len(body); i++ {
		if body[i] < '0' || body[i] > '9' {
			return 0, false
		}
	}
	h, _ := strconv.Atoi(body[:2])
	m := 0
	if len(body) == 4 {
		m, _ = strconv.Atoi(body[2:])
	}
	if h > 23 || m > 59 {
		return 0, false
	}
	return sign * (h*3600 + m*60), true
}

// ---------------------------------------------------------------------------
// Wall-clock construction
// ---------------------------------------------------------------------------

// resolveWall builds the instant for a wall clock in loc, pinning the two DST edge cases
// rather than inheriting whatever time.Date happens to do (which its own docs decline to
// guarantee for ambiguous times):
//
//   - Nonexistent (spring forward): 02:30 where the clock jumps 02:00→03:00 does not
//     exist. time.Date normalizes it forward to 03:30, which is what we want and keep.
//   - Ambiguous (autumn fall-back): 02:30 where the clock repeats 03:00→02:00 happens
//     twice. We take the first (earlier) occurrence, detected by the same wall clock
//     existing one hour earlier.
//
// Out-of-range date fields normalize the way time.Date does, which is what lets day
// arithmetic run past a month end.
func resolveWall(loc *time.Location, year int, month time.Month, day, hour, min, sec int) time.Time {
	t := time.Date(year, month, day, hour, min, sec, 0, loc)
	if e := t.Add(-time.Hour); sameClock(e, t) {
		return e
	}
	return t
}

// sameClock reports whether two instants show the identical wall clock — true only for the
// repeated hour of a fall-back transition.
func sameClock(a, b time.Time) bool {
	ay, am, ad := a.Date()
	by, bm, bd := b.Date()
	ah, ami, as := a.Clock()
	bh, bmi, bs := b.Clock()
	return ay == by && am == bm && ad == bd && ah == bh && ami == bmi && as == bs
}

// daysIn returns the number of days in a month: day 0 of the next month is the last day of
// this one.
func daysIn(year int, month time.Month) int {
	return time.Date(year, month+1, 0, 0, 0, 0, 0, time.UTC).Day()
}

// addMonths adds n calendar months, clamping to the end of the target month. The clamp is
// the whole point: time.AddDate normalizes overflow, so Jan 31 + 1 month lands on Mar 3
// rather than Feb 28.
func addMonths(t time.Time, n int) time.Time {
	year, month, day := t.Date()
	hour, min, sec := t.Clock()
	total := int(month) - 1 + n
	ny := year + floorDiv(total, 12)
	nm := time.Month(floorMod(total, 12) + 1)
	if last := daysIn(ny, nm); day > last {
		day = last
	}
	return resolveWall(t.Location(), ny, nm, day, hour, min, sec)
}

// addDays adds n calendar days, preserving the wall clock across any DST boundary in
// between — "tomorrow at the same time", not "24 hours later".
func addDays(t time.Time, n int) time.Time {
	year, month, day := t.Date()
	hour, min, sec := t.Clock()
	return resolveWall(t.Location(), year, month, day+n, hour, min, sec)
}

func floorDiv(a, b int) int {
	q := a / b
	if a%b != 0 && (a < 0) != (b < 0) {
		q--
	}
	return q
}

func floorMod(a, b int) int { return a - floorDiv(a, b)*b }

// ---------------------------------------------------------------------------
// Durations (`for`)
// ---------------------------------------------------------------------------

// Duration is a parsed `for` literal: a fixed part (ms/s/m/h) plus a calendar part
// (d/w/mo/y) that only means anything once a location is supplied.
type Duration struct {
	src    string
	fixed  time.Duration
	days   int
	months int
}

// Source returns the literal the duration was parsed from, for logs and error messages.
func (d *Duration) Source() string { return d.src }

// Millis builds a Duration from a bare millisecond count — the JSON-number form of `for`,
// and the shape every expression-valued delay arrives in.
func Millis(ms int64) *Duration {
	return &Duration{src: fmt.Sprintf("%dms", ms), fixed: time.Duration(ms) * time.Millisecond}
}

// durUnits is ordered longest-first so that "ms" and "mo" are matched before "m".
var durUnits = []struct {
	suffix string
	apply  func(*Duration, int64)
}{
	{"ms", func(d *Duration, n int64) { d.fixed += time.Duration(n) * time.Millisecond }},
	{"mo", func(d *Duration, n int64) { d.months += int(n) }},
	{"s", func(d *Duration, n int64) { d.fixed += time.Duration(n) * time.Second }},
	{"m", func(d *Duration, n int64) { d.fixed += time.Duration(n) * time.Minute }},
	{"h", func(d *Duration, n int64) { d.fixed += time.Duration(n) * time.Hour }},
	{"d", func(d *Duration, n int64) { d.days += int(n) }},
	{"w", func(d *Duration, n int64) { d.days += int(n) * 7 }},
	{"y", func(d *Duration, n int64) { d.months += int(n) * 12 }},
}

// ParseDuration parses a `for` literal: unit-suffixed counts, concatenated, whitespace
// optional — "2h30m", "90m", "1d 12h", "3mo".
//
// A unitless string is rejected on purpose. "5000" is exactly the ambiguity this syntax
// exists to remove, so it errors and names both readings; the bare JSON number `for: 5000`
// is the way to say milliseconds.
func ParseDuration(s string) (*Duration, error) {
	raw := strings.TrimSpace(s)
	if raw == "" {
		return nil, fmt.Errorf("duration is empty")
	}
	d := &Duration{src: raw}
	rest, any := raw, false
	for {
		rest = strings.TrimLeft(rest, " \t")
		if rest == "" {
			break
		}
		i := 0
		for i < len(rest) && rest[i] >= '0' && rest[i] <= '9' {
			i++
		}
		if i == 0 {
			return nil, fmt.Errorf("duration %q: expected a number before %q", raw, rest)
		}
		n, err := strconv.ParseInt(rest[:i], 10, 64)
		if err != nil {
			return nil, fmt.Errorf("duration %q: %q is out of range", raw, rest[:i])
		}
		rest = rest[i:]
		if rest == "" {
			return nil, fmt.Errorf("duration %q: %d has no unit — did you mean %q or %q? "+
				"(a bare number, without quotes, is milliseconds)", raw, n, fmt.Sprintf("%dms", n), fmt.Sprintf("%ds", n))
		}
		matched := false
		for _, u := range durUnits {
			if strings.HasPrefix(rest, u.suffix) {
				u.apply(d, n)
				rest, matched = rest[len(u.suffix):], true
				break
			}
		}
		if !matched {
			return nil, fmt.Errorf("duration %q: unknown unit %q; use ms, s, m, h, d, w, mo or y", raw, rest)
		}
		any = true
	}
	if !any {
		return nil, fmt.Errorf("duration %q has no components", raw)
	}
	return d, nil
}

// Resolve returns the instant this duration lands on, measured from now in loc. Calendar
// components apply first (see the package comment), then the fixed remainder.
func (d *Duration) Resolve(now time.Time, loc *time.Location) time.Time {
	t := now.In(loc)
	if d.months != 0 {
		t = addMonths(t, d.months)
	}
	if d.days != 0 {
		t = addDays(t, d.days)
	}
	return t.Add(d.fixed)
}

// ---------------------------------------------------------------------------
// Instants (`until`)
// ---------------------------------------------------------------------------

type instantKind int

const (
	kindAbsolute instantKind = iota // carries its own offset: already an instant
	kindWall                        // concrete date + time, interpreted in tz
	kindOffset                      // "+2d 08:00": a calendar offset, then a wall clock
	kindPattern                     // "*-*-01 08:00", "mon 09:00": next match after now
)

// Instant is a parsed `until` literal.
type Instant struct {
	src  string
	kind instantKind

	abs time.Time // kindAbsolute

	off *Duration // kindOffset

	// Date fields; -1 is a wildcard. kindWall and kindOffset use only the clock fields.
	year, month, day, weekday int
	hour, min, sec            int
}

// Source returns the literal the instant was parsed from, for logs and error messages.
func (i *Instant) Source() string { return i.src }

var weekdays = map[string]int{
	"sun": 0, "mon": 1, "tue": 2, "wed": 3, "thu": 4, "fri": 5, "sat": 6,
}

// absoluteLayouts carry a zone, so they resolve to an instant without tz.
var absoluteLayouts = []string{time.RFC3339Nano, time.RFC3339}

// wallLayouts carry no zone and are interpreted in tz at resolve time.
var wallLayouts = []string{
	"2006-01-02T15:04:05",
	"2006-01-02 15:04:05",
	"2006-01-02T15:04",
	"2006-01-02 15:04",
	"2006-01-02",
}

// ParseInstant parses an `until` literal in one of three closed forms:
//
//  1. absolute — RFC 3339, RFC 9557 with an IANA annotation
//     ("2026-09-01T08:00:00+02:00[Europe/Prague]"), or relaxed "2026-09-01 08:00"
//  2. offset + wall clock — "+2d 08:00": two days from now, at 08:00 in tz
//  3. calendar pattern — a systemd OnCalendar subset: "*-*-01 08:00", "mon 09:00"
//
// Natural language is deliberately absent: a definition is stored, versioned and replayed,
// and a locale-dependent parser would let an upgrade silently change what rows already in
// the database mean.
func ParseInstant(s string) (*Instant, error) {
	raw := strings.TrimSpace(s)
	if raw == "" {
		return nil, fmt.Errorf("instant is empty")
	}
	// RFC 9557's [Europe/Prague] annotation: the offset already fixes the instant, so the
	// name is validated and then dropped.
	body := raw
	if open := strings.IndexByte(body, '['); open >= 0 && strings.HasSuffix(body, "]") {
		name := body[open+1 : len(body)-1]
		if _, err := LoadLocation(name); err != nil {
			return nil, fmt.Errorf("until %q: %v", raw, err)
		}
		body = strings.TrimSpace(body[:open])
	}
	for _, layout := range absoluteLayouts {
		if t, err := time.Parse(layout, body); err == nil {
			return &Instant{src: raw, kind: kindAbsolute, abs: t}, nil
		}
	}
	for _, layout := range wallLayouts {
		if t, err := time.Parse(layout, body); err == nil {
			return &Instant{
				src: raw, kind: kindWall,
				year: t.Year(), month: int(t.Month()), day: t.Day(),
				hour: t.Hour(), min: t.Minute(), sec: t.Second(),
			}, nil
		}
	}
	if strings.HasPrefix(body, "+") {
		return parseOffset(raw, body)
	}
	return parsePattern(raw, body)
}

// parseOffset handles "+2d 08:00": a duration from now, then a wall clock on that day.
func parseOffset(raw, body string) (*Instant, error) {
	f := strings.Fields(body)
	if len(f) != 2 {
		return nil, fmt.Errorf("until %q: the offset form is a duration and a clock, e.g. %q; "+
			"for an offset with no clock use `for` instead", raw, "+2d 08:00")
	}
	off, err := ParseDuration(strings.TrimPrefix(f[0], "+"))
	if err != nil {
		return nil, fmt.Errorf("until %q: %v", raw, err)
	}
	h, m, sec, err := parseClock(f[1])
	if err != nil {
		return nil, fmt.Errorf("until %q: %v", raw, err)
	}
	return &Instant{src: raw, kind: kindOffset, off: off, hour: h, min: m, sec: sec}, nil
}

// parsePattern handles the calendar-pattern form: an optional weekday, an optional
// wildcarded Y-M-D, and a clock.
func parsePattern(raw, body string) (*Instant, error) {
	f := strings.Fields(body)
	if len(f) == 0 {
		return nil, fmt.Errorf("until %q is not a recognised instant", raw)
	}
	i := &Instant{src: raw, kind: kindPattern, year: -1, month: -1, day: -1, weekday: -1}
	h, m, sec, err := parseClock(f[len(f)-1])
	if err != nil {
		return nil, fmt.Errorf("until %q: %v", raw, err)
	}
	i.hour, i.min, i.sec = h, m, sec

	for _, tok := range f[:len(f)-1] {
		if wd, ok := weekdays[strings.ToLower(tok)]; ok {
			i.weekday = wd
			continue
		}
		if !strings.Contains(tok, "-") {
			return nil, fmt.Errorf("until %q: %q is neither a weekday nor a Y-M-D date", raw, tok)
		}
		parts := strings.Split(tok, "-")
		if len(parts) != 3 {
			return nil, fmt.Errorf("until %q: date %q must be Y-M-D, e.g. %q", raw, tok, "*-*-01")
		}
		fields := []struct {
			dst      *int
			name     string
			min, max int
		}{
			{&i.year, "year", 1970, 9999},
			{&i.month, "month", 1, 12},
			{&i.day, "day", 1, 31},
		}
		for n, fd := range fields {
			if parts[n] == "*" {
				continue
			}
			v, err := strconv.Atoi(parts[n])
			if err != nil || v < fd.min || v > fd.max {
				return nil, fmt.Errorf("until %q: %s %q must be %q or %d-%d", raw, fd.name, parts[n], "*", fd.min, fd.max)
			}
			*fd.dst = v
		}
	}
	// A concrete month/day pair that no year can satisfy ("*-02-30") is decidable now, and
	// catching it here keeps registration deterministic — the alternative, resolving
	// against the current clock, would make the same definition validate differently on
	// different days. A leap year is used as the bound so "*-02-29" stays legal.
	if i.month >= 0 && i.day >= 0 {
		if last := daysIn(2024, time.Month(i.month)); i.day > last {
			return nil, fmt.Errorf("until %q: %s has no day %d", raw, time.Month(i.month), i.day)
		}
	}
	return i, nil
}

// parseClock parses HH:MM or HH:MM:SS.
func parseClock(s string) (hour, min, sec int, err error) {
	parts := strings.Split(s, ":")
	if len(parts) != 2 && len(parts) != 3 {
		return 0, 0, 0, fmt.Errorf("clock %q must be HH:MM or HH:MM:SS", s)
	}
	bounds := []int{23, 59, 59}
	out := []int{0, 0, 0}
	for n, p := range parts {
		v, e := strconv.Atoi(p)
		if e != nil || v < 0 || v > bounds[n] {
			return 0, 0, 0, fmt.Errorf("clock %q: %q is out of range", s, p)
		}
		out[n] = v
	}
	return out[0], out[1], out[2], nil
}

// Resolve returns the instant this spec denotes, relative to now in loc. A pattern with no
// match inside maxPatternDays is the only failure — every other form resolves, including
// one already in the past, which the caller clamps.
func (i *Instant) Resolve(now time.Time, loc *time.Location) (time.Time, error) {
	switch i.kind {
	case kindAbsolute:
		return i.abs, nil
	case kindWall:
		return resolveWall(loc, i.year, time.Month(i.month), i.day, i.hour, i.min, i.sec), nil
	case kindOffset:
		base := i.off.Resolve(now, loc)
		return resolveWall(loc, base.Year(), base.Month(), base.Day(), i.hour, i.min, i.sec), nil
	default:
		return i.nextMatch(now, loc)
	}
}

// nextMatch walks forward a day at a time for the first day satisfying the pattern whose
// clock lands strictly after now. Day-stepping (rather than arithmetic per field) is what
// keeps "*-*-31 08:00" and leap days correct without special cases.
func (i *Instant) nextMatch(now time.Time, loc *time.Location) (time.Time, error) {
	cur := now.In(loc)
	for n := 0; n <= maxPatternDays; n++ {
		day := addDays(cur, n)
		year, month, dom := day.Date()
		if (i.year >= 0 && year != i.year) ||
			(i.month >= 0 && int(month) != i.month) ||
			(i.day >= 0 && dom != i.day) ||
			(i.weekday >= 0 && int(day.Weekday()) != i.weekday) {
			continue
		}
		// resolveWall normalizes an impossible day (Feb 30) into the next month, which
		// would match a date the pattern never named — so verify the day survived.
		cand := resolveWall(loc, year, month, dom, i.hour, i.min, i.sec)
		if cand.Day() != dom {
			continue
		}
		if cand.After(now) {
			return cand, nil
		}
	}
	return time.Time{}, fmt.Errorf("until %q has no match within %d years", i.src, maxPatternDays/366)
}

// Package delayspec parses the human-facing literals of the delay action: `for` (a
// duration from arm time) and `until` (an instant), plus the `tz` both resolve against.
// Grammars, decisions and edge cases: specs/delay-syntax.md.
//
// No engine or database dependency, so the calendar cases are table-testable in isolation.
//
// Three rules govern everything here:
//
//   - Calendar units (d, w, mo, y) are calendar arithmetic in the target location: "1d" is
//     the same wall clock tomorrow, 23 or 25 hours across a DST boundary. Fixed units
//     (ms, s, m, h) are always absolute elapsed time. With no tz the location is UTC,
//     which has no transitions, so the "without tz they are fixed" rule falls out of the
//     general case rather than being a separate one.
//   - Calendar units apply before fixed ones whatever order they were written in, so a
//     spec's meaning does not depend on how it was typed.
//   - A nonexistent wall clock (spring forward) normalizes forward; an ambiguous one
//     (autumn fall-back) takes the first occurrence — or the second where a pattern's next
//     match is sought and the first has gone by.
//
// Resolution never fails on a target in the past; the caller clamps to now. Timers keep
// running while an instance is paused, so an `until` behind now is legitimate on resume.

package delayspec

import (
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"
)

// maxPatternDays bounds the forward search for a calendar pattern's next match. Five
// years is far past any plausible schedule, and the bound is what stops a pattern that
// can never match (e.g. "*-02-30") from spinning.
const maxPatternDays = 366 * 5

// maxClockRetries bounds how many candidate times of day nextMatch reconsiders within one
// date. A candidate is only ever reconsidered because its wall clock is ambiguous and both
// of its occurrences are behind now, which takes at most one retry to leave — a fall-back
// repeats an hour once, not repeatedly. The cap exists so a zone rule nobody anticipated
// cannot turn a resolve into a scan of the day.
const maxClockRetries = 4

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

// resolveWall pins the two DST cases time.Date declines to guarantee: a nonexistent wall
// clock (spring forward) normalizes FORWARD past the jump; an ambiguous one (fall back)
// takes the first occurrence. time.Date's own direction varies by zone — see CLAUDE.md.
func resolveWall(loc *time.Location, year int, month time.Month, day, hour, min, sec int) time.Time {
	t := time.Date(year, month, day, hour, min, sec, 0, loc)
	// The same fields read as UTC: a canonical form of what was asked for, with any
	// out-of-range date already carried, and no zone rules involved.
	want := time.Date(year, month, day, hour, min, sec, 0, time.UTC)
	if !sameClock(t, want) {
		return skippedForward(loc, want)
	}
	if e := t.Add(-time.Hour); sameClock(e, t) {
		return e
	}
	return t
}

// skippedForward resolves a wall clock a transition deleted. want carries the requested
// reading as UTC; reading it against each of the two offsets in play around the gap gives
// two instants, and the later one is the reading carried forward — the pre-gap offset's
// answer, whichever direction the zone's rules run.
func skippedForward(loc *time.Location, want time.Time) time.Time {
	_, off := want.In(loc).Zone()
	first := want.Add(-time.Duration(off) * time.Second)
	_, off = first.In(loc).Zone()
	second := want.Add(-time.Duration(off) * time.Second)
	if second.After(first) {
		return second.In(loc)
	}
	return first.In(loc)
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

// addFixed accumulates n units into the fixed part, refusing the multiply and the add that
// would wrap. `time.Duration` is int64 *nanoseconds*, so the wrap is reachable from a
// literal an author could plausibly type — and it is not merely a large or negative
// result: "5124096h" wrapped to a positive **25 minutes**, a value that passes every
// downstream sanity check there is.
func (d *Duration) addFixed(n int64, unit time.Duration) error {
	if n > int64(math.MaxInt64)/int64(unit) {
		return errDurationRange
	}
	add := time.Duration(n) * unit
	if d.fixed > time.Duration(math.MaxInt64)-add {
		return errDurationRange
	}
	d.fixed += add
	return nil
}

// addCalendar is the same guard for the calendar part, whose components are scaled (a week
// is 7 days, a year 12 months) before `time.AddDate` ever sees them.
func addCalendar(field *int, n, scale int64) error {
	if n > math.MaxInt64/scale {
		return errDurationRange
	}
	scaled := n * scale
	if int64(*field) > math.MaxInt64-scaled {
		return errDurationRange
	}
	*field += int(scaled)
	return nil
}

var errDurationRange = errors.New("out of range")

// durUnits is ordered longest-first so that "ms" and "mo" are matched before "m".
var durUnits = []struct {
	suffix string
	apply  func(*Duration, int64) error
}{
	{"ms", func(d *Duration, n int64) error { return d.addFixed(n, time.Millisecond) }},
	{"mo", func(d *Duration, n int64) error { return addCalendar(&d.months, n, 1) }},
	{"s", func(d *Duration, n int64) error { return d.addFixed(n, time.Second) }},
	{"m", func(d *Duration, n int64) error { return d.addFixed(n, time.Minute) }},
	{"h", func(d *Duration, n int64) error { return d.addFixed(n, time.Hour) }},
	{"d", func(d *Duration, n int64) error { return addCalendar(&d.days, n, 1) }},
	{"w", func(d *Duration, n int64) error { return addCalendar(&d.days, n, 7) }},
	{"y", func(d *Duration, n int64) error { return addCalendar(&d.months, n, 12) }},
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
				if err := u.apply(d, n); err != nil {
					return nil, fmt.Errorf("duration %q: %d%s is out of range; a duration cannot exceed about 292 years", raw, n, u.suffix)
				}
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

// Fixed returns the duration as a plain time.Duration, and false if it carries calendar
// components. A calendar duration has no length until a location and a start instant fix
// it, so a caller that must do arithmetic on the value — scale it, compare it to a
// ceiling — has to refuse one rather than pick a nominal length for it.
func (d *Duration) Fixed() (time.Duration, bool) {
	if d.days != 0 || d.months != 0 {
		return 0, false
	}
	return d.fixed, true
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

	// Date fields; `wildcard` matches every value.
	year, month, day, weekday int
	// Clock fields. kindWall and kindOffset use only these, and only ever as single values —
	// widening a clock is a pattern-only spelling.
	hour, min, sec clockField
}

// wildcard is the stored form of a `*` date field.
const wildcard = -1

// One field of a pattern's clock: base, base+step, … within range. The representation
// collapses the three spellings — "*" is 0/1, "0/5" is 0/5, "07" is 7 with step 0 — so
// nothing downstream branches on which was written.
type clockField struct {
	base, step int
}

func exactValue(v int) clockField { return clockField{base: v} }

// everyValue is the stored form of "*" — from zero, in ones.
var everyValue = clockField{base: 0, step: 1}

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
//  3. calendar pattern — a systemd OnCalendar subset: "*-*-01 08:00", "mon 09:00",
//     "*:*:00" (every whole minute), "*:*:*" (every second), "*:*:0/5" (every five
//     seconds), "*:2/5:00" (every five minutes, from :02)
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
				hour: exactValue(t.Hour()), min: exactValue(t.Minute()), sec: exactValue(t.Second()),
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
	h, m, sec, err := parseClock(f[1], false)
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
	i := &Instant{src: raw, kind: kindPattern, year: wildcard, month: wildcard, day: wildcard, weekday: wildcard}
	h, m, sec, err := parseClock(f[len(f)-1], true)
	if err != nil {
		return nil, fmt.Errorf("until %q: %v", raw, err)
	}
	i.hour, i.min, i.sec = h, m, sec

	// Two tokens of the same kind are a spec meaning "either", which this grammar cannot
	// express — and quietly keeping the last one would schedule something nobody asked for.
	var sawWeekday, sawDate bool
	for _, tok := range f[:len(f)-1] {
		if wd, ok := weekdays[strings.ToLower(tok)]; ok {
			if sawWeekday {
				return nil, fmt.Errorf("until %q: two weekdays; a pattern names one, and there is no way to say %q", raw, "either")
			}
			sawWeekday, i.weekday = true, wd
			continue
		}
		if !strings.Contains(tok, "-") {
			return nil, fmt.Errorf("until %q: %q is neither a weekday nor a Y-M-D date", raw, tok)
		}
		if sawDate {
			return nil, fmt.Errorf("until %q: two dates; a pattern names one", raw)
		}
		sawDate = true
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
			if !allDigits(parts[n]) || err != nil || v < fd.min || v > fd.max {
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

// parseClock parses HH:MM[:SS]. In a pattern a field widens via "*" or "base/step" only
// (systemd's spellings; cron's "*/5" is rejected BY NAME — it cannot carry a phase, and
// the error names the right form). Omitted seconds stay :00 — one instant a day, always.
func parseClock(s string, allowPattern bool) (hour, min, sec clockField, err error) {
	parts := strings.Split(s, ":")
	if len(parts) != 2 && len(parts) != 3 {
		return clockField{}, clockField{}, clockField{}, fmt.Errorf("clock %q must be HH:MM or HH:MM:SS", s)
	}
	bounds := []int{23, 59, 59}
	out := []clockField{exactValue(0), exactValue(0), exactValue(0)}
	for n, p := range parts {
		f, err := parseClockField(s, p, bounds[n], allowPattern)
		if err != nil {
			return clockField{}, clockField{}, clockField{}, err
		}
		out[n] = f
	}
	return out[0], out[1], out[2], nil
}

// parseClockField parses one field of a clock: "*", "base/step", or a plain number.
func parseClockField(clock, p string, max int, allowPattern bool) (clockField, error) {
	widened := func(what string) error {
		return fmt.Errorf("clock %q: %s is only allowed in a calendar pattern, e.g. %q", clock, what, "*:*:0/5")
	}
	if p == "*" {
		if !allowPattern {
			return clockField{}, widened(`"*"`)
		}
		return everyValue, nil
	}
	if base, step, ok := strings.Cut(p, "/"); ok {
		if !allowPattern {
			return clockField{}, widened("a step")
		}
		// The one place cron habit lands wrong, so it gets its own message rather than
		// "out of range".
		if base == "*" {
			return clockField{}, fmt.Errorf("clock %q: %q is cron's spelling; write the base explicitly, e.g. %q for every %s values",
				clock, p, "0/"+step, step)
		}
		b, err := parseClockValue(clock, base, max)
		if err != nil {
			return clockField{}, err
		}
		n, err := strconv.Atoi(step)
		if !allDigits(step) || err != nil || n < 1 {
			return clockField{}, fmt.Errorf("clock %q: step %q must be a positive number, as in %q", clock, step, "0/5")
		}
		// A step past the field's range matches its base and nothing else, which is what
		// writing the base alone already says — so it is a mistake, not a shorthand.
		if n > max {
			return clockField{}, fmt.Errorf("clock %q: step %d is past the field's range (0-%d), so it would match only %d; write %q if that is what you mean",
				clock, n, max, b, base)
		}
		return clockField{base: b, step: n}, nil
	}
	v, err := parseClockValue(clock, p, max)
	if err != nil {
		return clockField{}, err
	}
	return exactValue(v), nil
}

func parseClockValue(clock, p string, max int) (int, error) {
	// Digits only. strconv.Atoi would take "+5" and "-0", which are not clock readings —
	// and a sign here is far likelier to be a typo for something else than a deliberate 5.
	if !allDigits(p) {
		return 0, fmt.Errorf("clock %q: %q is out of range", clock, p)
	}
	v, err := strconv.Atoi(p)
	if err != nil || v < 0 || v > max {
		return 0, fmt.Errorf("clock %q: %q is out of range", clock, p)
	}
	return v, nil
}

func allDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// Resolve returns the instant this spec denotes, relative to now in loc. A pattern with no
// match inside maxPatternDays is the only failure — every other form resolves, including
// one already in the past, which the caller clamps.
func (i *Instant) Resolve(now time.Time, loc *time.Location) (time.Time, error) {
	switch i.kind {
	case kindAbsolute:
		return i.abs, nil
	case kindWall:
		// Both non-pattern forms name a single clock — parseClock refuses to widen one for
		// them — so reading the fields' bases is reading their values.
		return resolveWall(loc, i.year, time.Month(i.month), i.day, i.hour.base, i.min.base, i.sec.base), nil
	case kindOffset:
		base := i.off.Resolve(now, loc)
		return resolveWall(loc, base.Year(), base.Month(), base.Day(), i.hour.base, i.min.base, i.sec.base), nil
	default:
		return i.nextMatch(now, loc)
	}
}

// nextMatch: the date half is WALKED a day at a time (bounded by maxPatternDays — stepping
// keeps "*-*-31" and leap days correct for free), the clock half is COMPUTED as a carry
// cascade, never stepped — "*:*:*" matches 86400 times a day and a search would melt.
func (i *Instant) nextMatch(now time.Time, loc *time.Location) (time.Time, error) {
	// Matches land on whole seconds, so the earliest one that can be strictly after now is
	// the second after now's. Truncating in absolute time is exact here: every zone offset
	// is a whole number of minutes, so second boundaries are the same in every zone.
	lb := now.Truncate(time.Second).Add(time.Second).In(loc)
	// The date walk is anchored at midday, not at the bound's own clock. addDays preserves
	// the wall clock, and a clock sitting next to midnight can be carried across a date
	// boundary by a DST jump, which would skip a date without anything reporting it. No
	// zone rule moves midday off its own date.
	anchor := resolveWall(loc, lb.Year(), lb.Month(), lb.Day(), 12, 0, 0)

	// The walk visits wall clocks in increasing order, which misses one thing: inside a
	// fall-back repeat, wall clocks *behind* now happen again ahead of it. Those second
	// occurrences are found separately, and whichever of the two searches lands earlier
	// wins. See repeatedMatch.
	repeated, hasRepeated := i.repeatedMatch(now, loc)

	for n := 0; n <= maxPatternDays; n++ {
		day := addDays(anchor, n)
		year, month, dom := day.Date()
		if !i.matchesDate(day) {
			continue
		}
		// Only the first day of the walk is constrained by now; every later one is open
		// from midnight.
		fromH, fromM, fromS := 0, 0, 0
		if n == 0 {
			fromH, fromM, fromS = lb.Clock()
		}
		for retry := 0; retry < maxClockRetries; retry++ {
			h, m, s, ok := i.nextClock(fromH, fromM, fromS)
			if !ok {
				break // this day has no clock left; try the next matching date
			}
			// The date here always exists — the walk enumerates real dates and matchesDate
			// is what rejects the ones the pattern excludes — so the only way this lands on
			// another date is a transition carrying the reading over midnight, and that
			// instant is the honest answer for the clock that was asked for.
			cand := resolveWall(loc, year, month, dom, h, m, s)
			if cand.After(now) {
				return earlier(cand, repeated, hasRepeated), nil
			}
			// Behind now, which on a lower bound taken from now can only mean an ambiguous
			// wall clock: resolveWall names the *first* occurrence of one, and now is
			// already in the second. Then the second occurrence is the next match — "the
			// next 02:30" after the first 02:30 has passed is the other 02:30, not a reason
			// to skip the repeated hour entirely. Detected and offset the same way
			// resolveWall detects ambiguity, so both rest on the same whole-hour assumption.
			if alt := cand.Add(time.Hour); sameClock(alt, cand) && alt.After(now) {
				return earlier(alt, repeated, hasRepeated), nil
			}
			// Neither occurrence is ahead: retry from the next second the pattern could
			// match. Strictly monotone, so the retry cap is never the thing that ends this.
			fromH, fromM, fromS = h, m, s+1
		}
	}
	if hasRepeated {
		return repeated, nil
	}
	return time.Time{}, fmt.Errorf("until %q has no match within %d years", i.src, maxPatternDays/366)
}

// earlier picks the sooner of the walk's candidate and the repeat's, when there is one.
func earlier(cand, repeated time.Time, hasRepeated bool) time.Time {
	if hasRepeated && repeated.Before(cand) {
		return repeated
	}
	return cand
}

func (i *Instant) matchesDate(day time.Time) bool {
	year, month, dom := day.Date()
	return (i.year == wildcard || year == i.year) &&
		(i.month == wildcard || int(month) == i.month) &&
		(i.day == wildcard || dom == i.day) &&
		(i.weekday == wildcard || int(day.Weekday()) == i.weekday)
}

// repeatedMatch finds the match ordered wall clocks cannot see: during fall-back an hour
// repeats, so a wall clock behind now occurs again ahead of it. The repeat is searched
// from its own start and offset by its length; the caller keeps the sooner candidate.
func (i *Instant) repeatedMatch(now time.Time, loc *time.Location) (time.Time, bool) {
	// Read now as a wall clock in loc: whether it is ambiguous is a question about the
	// target zone, not about whichever zone the caller's clock happens to carry.
	local := now.In(loc)
	// The same wall clock an hour later means now is in a repeat's first pass — the mirror
	// of resolveWall's own ambiguity test, and it rests on the same whole-hour assumption.
	if !sameClock(local, local.Add(time.Hour)) {
		return time.Time{}, false
	}
	start := fallBackAt(local).In(loc)
	if !i.matchesDate(start) {
		return time.Time{}, false
	}
	h, m, s, ok := i.nextClock(start.Clock())
	if !ok {
		return time.Time{}, false
	}
	year, month, dom := start.Date()
	first := resolveWall(loc, year, month, dom, h, m, s)
	second := first.Add(time.Hour)
	// Only a genuinely ambiguous wall clock has a second occurrence; past the repeat's end
	// the walk is the one with the answer.
	if first.Day() != dom || !sameClock(second, first) || !second.After(now) {
		return time.Time{}, false
	}
	return second, true
}

// fallBackAt returns the instant a fall-back takes effect, for a now already known to sit
// in the repeated hour's first pass. Go exposes no transition table, so the offset is
// bisected: it changes exactly once in the hour or two after such a now, and the bisection
// converges on the second it changes.
func fallBackAt(now time.Time) time.Time {
	_, base := now.Zone()
	lo, hi := now, now.Add(2*time.Hour)
	for hi.Sub(lo) > time.Second {
		mid := lo.Add(hi.Sub(lo) / 2)
		if _, off := mid.Zone(); off == base {
			lo = mid
		} else {
			hi = mid
		}
	}
	return hi
}

// nextClock: the earliest admissible time of day at/after from, carrying like a clock —
// an exhausted field advances the one above and resets those below. fromS/fromM may
// arrive as 60 (nextMatch's retry step); an out-of-range bound simply admits nothing.
func (i *Instant) nextClock(fromH, fromM, fromS int) (h, m, s int, ok bool) {
	h, ok = fieldAtLeast(i.hour, fromH, 23)
	if !ok {
		return 0, 0, 0, false
	}
	if h > fromH {
		// A later hour than the bound: the minutes and seconds are unconstrained by it.
		return h, fieldMin(i.min), fieldMin(i.sec), true
	}
	if m, ok = fieldAtLeast(i.min, fromM, 59); ok {
		if m > fromM {
			return h, m, fieldMin(i.sec), true
		}
		if s, ok = fieldAtLeast(i.sec, fromS, 59); ok {
			return h, m, s, true
		}
		// This minute is spent; the next minute this hour, if the pattern allows one.
		if m, ok = fieldAtLeast(i.min, fromM+1, 59); ok {
			return h, m, fieldMin(i.sec), true
		}
	}
	// This hour is spent; the next hour today, if the pattern allows one.
	if h, ok = fieldAtLeast(i.hour, fromH+1, 23); ok {
		return h, fieldMin(i.min), fieldMin(i.sec), true
	}
	return 0, 0, 0, false
}

// fieldAtLeast returns the smallest value a clock field admits that is at least from, and
// whether it admits one at all. This is the one place a field's shape is interpreted, which
// is why a step costs no extra branch anywhere else: the answer is arithmetic, never a
// scan, so a field with 60 admissible values is no more expensive than one with a single
// value.
func fieldAtLeast(field clockField, from, max int) (int, bool) {
	if field.step == 0 {
		if field.base < from {
			return 0, false
		}
		return field.base, true
	}
	v := field.base
	if from > v {
		// Round the bound up to the next value on the step's own grid, anchored at base.
		v += ceilDiv(from-field.base, field.step) * field.step
	}
	if v > max {
		return 0, false
	}
	return v, true
}

// fieldMin is a clock field's smallest admissible value, for the fields below one that has
// just advanced.
func fieldMin(field clockField) int { return field.base }

// ceilDiv divides rounding away from zero, for non-negative operands.
func ceilDiv(a, b int) int { return (a + b - 1) / b }

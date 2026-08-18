package model

import (
	"fmt"
	"sort"
	"strconv"
	"strings"

	"genroc/internal/schema"
)

// Status patterns are one vocabulary shared by `accepted_status` and the keys of a fetch's
// `responses` map: an exact three-digit code ("404") or a hundred-range ("4xx"). The
// registration format check and the runtime match resolve through this file so they cannot
// drift. See specs/fetch-http-surface.md §2.

// ValidStatusPattern reports whether p is a pattern the matcher can ever match.
func ValidStatusPattern(p string) bool {
	if len(p) != 3 {
		return false
	}
	if p[1] == 'x' && p[2] == 'x' {
		return p[0] >= '1' && p[0] <= '5'
	}
	for _, c := range p {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

// MatchStatusPattern reports whether p covers code.
func MatchStatusPattern(p string, code int) bool {
	if len(p) == 3 && p[1] == 'x' && p[2] == 'x' {
		hundreds := int(p[0]-'0') * 100
		return code >= hundreds && code <= hundreds+99
	}
	n, err := strconv.Atoi(p)
	return err == nil && n == code
}

// MatchAnyStatus reports whether any pattern covers code. An empty list accepts any 2xx —
// the `accepted_status` default, applied here so the one caller cannot forget it.
func MatchAnyStatus(code int, patterns []string) bool {
	if len(patterns) == 0 {
		return code >= 200 && code <= 299
	}
	for _, p := range patterns {
		if MatchStatusPattern(p, code) {
			return true
		}
	}
	return false
}

// IsSuccessPattern reports whether every status p matches is a 2xx. It is what splits a
// `responses` declaration between self.result and error.data.
func IsSuccessPattern(p string) bool {
	if len(p) == 3 && p[1] == 'x' && p[2] == 'x' {
		return p[0] == '2'
	}
	n, err := strconv.Atoi(p)
	return err == nil && n >= 200 && n <= 299
}

// patternWidth is how many statuses p can match. It orders overlapping declarations —
// exact beats range — and equal widths are refused at registration, so it never ties.
func patternWidth(p string) int {
	if len(p) == 3 && p[1] == 'x' && p[2] == 'x' {
		return 100
	}
	return 1
}

// ParseResponseKey splits a `responses` key into its patterns. Whitespace around a comma is
// ignored; an empty element, a malformed pattern, a repeat, and a key mixing success with
// failure statuses are all refused — the last because a key decides acceptance, so one
// spanning both channels would narrow it from a line written for the error side.
func ParseResponseKey(key string) ([]string, error) {
	parts := strings.Split(key, ",")
	out := make([]string, 0, len(parts))
	seen := make(map[string]bool, len(parts))
	for _, part := range parts {
		p := strings.TrimSpace(part)
		if p == "" {
			return nil, fmt.Errorf("empty status pattern")
		}
		if !ValidStatusPattern(p) {
			return nil, fmt.Errorf("%q is not a status pattern — use an exact code (\"404\") or a hundred-range (\"4xx\")", p)
		}
		if seen[p] {
			return nil, fmt.Errorf("%q is listed twice", p)
		}
		seen[p] = true
		out = append(out, p)
	}
	for _, p := range out {
		if IsSuccessPattern(p) != IsSuccessPattern(out[0]) {
			return nil, fmt.Errorf("mixes success and failure statuses (%q and %q) — a declaration decides acceptance, so split it into two keys", out[0], p)
		}
	}
	return out, nil
}

// ResponseFor returns the schema declared for code, and whether any key declared it. A nil
// schema with declared=true is the "no body" entry — key presence, never nil-ness, is what
// says a status was described.
func (a *Action) ResponseFor(code int) (*schema.Schema, bool) {
	best, bestWidth := "", 0
	var bestSchema *schema.Schema
	for key, sc := range a.Responses {
		patterns, err := ParseResponseKey(key)
		if err != nil {
			continue // refused at registration; a stored definition cannot hold one
		}
		for _, p := range patterns {
			if !MatchStatusPattern(p, code) {
				continue
			}
			if best == "" || patternWidth(p) < bestWidth {
				best, bestWidth, bestSchema = p, patternWidth(p), sc
			}
		}
	}
	return bestSchema, best != ""
}

// EffectiveAcceptedStatus is rule 1: the resolved accepted_status where it names anything,
// otherwise the 2xx patterns of responses, otherwise nil — which MatchAnyStatus reads as any
// 2xx. The runtime and inference MUST resolve it through here. They diverged once: the engine
// fell back to "any 2xx" while inference read the declared set, so a 201 against {200: T} was
// accepted, matched no declaration, skipped validation, and landed in self.result typed as a
// T it had never been checked against.
func (a *Action) EffectiveAcceptedStatus(resolved []string) []string {
	if len(resolved) > 0 {
		return resolved
	}
	return a.SuccessPatterns()
}

// SuccessPatterns returns the 2xx patterns declared in responses, sorted. Empty means the
// declaration says nothing about which statuses succeed, and the 2xx default stands.
func (a *Action) SuccessPatterns() []string {
	var out []string
	for key := range a.Responses {
		patterns, err := ParseResponseKey(key)
		if err != nil {
			continue
		}
		for _, p := range patterns {
			if IsSuccessPattern(p) {
				out = append(out, p)
			}
		}
	}
	sort.Strings(out)
	return out
}

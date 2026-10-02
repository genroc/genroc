package errcode

import "testing"

// Not SQL LIKE: a '_' wildcard would make `order_%` match `order.placed`.
func TestMatchCode(t *testing.T) {
	cases := []struct {
		pattern, code string
		want          bool
	}{
		// '_' is literal: an underscore pattern does not match a dotted code.
		{"order_%", "order_placed", true},
		{"order_%", "order.placed", false},
		{"fourth_%", "fourth_failed", true},
		{"fourth_%", "fourth.failed", false},
		// '.' is literal.
		{"order.%", "order.placed", true},
		{"order.%", "order_placed", false},
		{"http.%", "http.500", true},
		{"http.%", "https.500", false},
		// '%' matches any run, including none and across dots.
		{"%", "anything.at_all", true},
		{"pre.%", "pre.timeout", true},
		{"pre.%", "pre.", true},
		{"exact", "exact", true},
		{"exact", "exacty", false},
		// '%' in the middle.
		{"order.%.done", "order.ship.done", true},
		{"order.%.done", "order.done", false},
	}
	for _, c := range cases {
		if got := MatchCode(c.pattern, c.code); got != c.want {
			t.Errorf("MatchCode(%q, %q) = %v, want %v", c.pattern, c.code, got, c.want)
		}
	}
}

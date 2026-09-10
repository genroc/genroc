package template

import (
	"strings"
	"testing"
)

// Scan is Parse's scanner with offsets kept, so the two must find the same expressions in the
// same order. Nothing else notices when one learns a rule the other does not — the marker set,
// the $$ escape, the shortest-body-that-parses terminator.
func TestScanAgreesWithParse(t *testing.T) {
	sources := []string{
		`$: input.who`,
		`  $: (self.previous.count ?? 0) + 1`,
		`hello, ${ input.who }`,
		`${ input.a }/${ input.b }`,
		`https://x/${ input.id }?q=${ input.q }`,
		`$: map(input.rows, i => {id: i.id})`,
		`${ map(input.rows, i => {id: i.id}) }`,
		`plain text with no marker`,
		`costs $$5 and $notavar`,
		`$${ not an interpolation }`,
		`$$: not an expression`,
		`$: x ?? "none"`,
	}

	for _, src := range sources {
		tmpl, err := Parse(src)
		if err != nil {
			t.Fatalf("Parse(%q): %v", src, err)
		}
		// Compared trimmed: Parse hands `${ x }`'s body to the lexer with its padding still
		// on, Scan reports the tight span a marker has to be drawn over. The expressions are
		// the same; only the whitespace around them differs.
		want := []string{}
		for _, c := range tmpl.chunks {
			if c.node != nil {
				want = append(want, strings.TrimSpace(c.text))
			}
		}

		got := []string{}
		for _, r := range Scan(src) {
			got = append(got, src[r.Body.From:r.Body.To])
		}

		if len(got) != len(want) {
			t.Errorf("%q: Scan found %d expressions, Parse found %d (%q vs %q)", src, len(got), len(want), got, want)
			continue
		}
		for i := range want {
			if got[i] != want[i] {
				t.Errorf("%q: expression %d is %q to Scan and %q to Parse", src, i, got[i], want[i])
			}
		}
	}
}

// The offsets are the whole point: a marker reported at the wrong column underlines the wrong
// text, and every span has to be a real slice of the source it came from.
func TestScanReportsWhereTheMarkerIs(t *testing.T) {
	src := `hello, ${ input.who }!`
	regions := Scan(src)
	if len(regions) != 1 {
		t.Fatalf("got %d regions, want 1", len(regions))
	}
	r := regions[0]
	if s := src[r.Marker.From:r.Marker.To]; s != "${" {
		t.Errorf("marker is %q, want %q", s, "${")
	}
	if s := src[r.Body.From:r.Body.To]; s != "input.who" {
		t.Errorf("body is %q, want %q", s, "input.who")
	}
	if s := src[r.Close.From:r.Close.To]; s != "}" {
		t.Errorf("close is %q, want %q", s, "}")
	}
}

func TestScanMarksATypedLeafFromItsMarker(t *testing.T) {
	src := `  $:   input.who  `
	regions := Scan(src)
	if len(regions) != 1 {
		t.Fatalf("got %d regions, want 1", len(regions))
	}
	r := regions[0]
	if s := src[r.Marker.From:r.Marker.To]; s != "$:" {
		t.Errorf("marker is %q, want %q", s, "$:")
	}
	// Trimmed on both sides: the body is what reaches the expression lexer.
	if s := src[r.Body.From:r.Body.To]; s != "input.who" {
		t.Errorf("body is %q, want %q", s, "input.who")
	}
	if !r.Close.Empty() {
		t.Errorf("a $: leaf has no closing brace, got %v", r.Close)
	}
}

// A buffer being typed in is the normal input, and Parse refuses it. Scan must still place the
// marker — the alternative is highlighting that disappears while someone writes.
func TestScanHandlesWhatParseRefuses(t *testing.T) {
	for _, src := range []string{`x ${ input.`, `${ }`, `$: (`} {
		if _, err := Parse(src); err == nil {
			t.Fatalf("%q parses; pick a source that does not", src)
		}
		for _, r := range Scan(src) {
			if r.Marker.To > len(src) || r.Body.To > len(src) || r.Body.From > r.Body.To {
				t.Errorf("%q: span outside the source: %+v", src, r)
			}
		}
	}
}

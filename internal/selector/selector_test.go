package selector

import (
	"strings"
	"testing"
)

func TestLexReadsEveryDelimiterInOnePass(t *testing.T) {
	cases := []struct {
		in     string
		delims []byte
		want   []string
	}{
		{"order_proc:charge:fetch.result.fee", []byte{':', '.'},
			[]string{"order_proc", ":", "charge", ":", "fetch", ".", "result", ".", "fee"}},
		{"output", []byte{':', '.'}, []string{"output"}},
		{"order_proc:output", []byte{':', '.'}, []string{"order_proc", ":", "output"}},
		// A delimiter inside a quoted name is a character, which is the only reason quoting
		// exists at all.
		{`"odd:name":"a.task":external.result`, []byte{':', '.'},
			[]string{"odd:name", ":", "a.task", ":", "external", ".", "result"}},
		{`fetch.result."say \"hi\""`, []byte{':', '.'},
			[]string{"fetch", ".", "result", ".", `say "hi"`}},
		{`fetch."back\\slash"`, []byte{':', '.'}, []string{"fetch", ".", `back\slash`}},
		// A name always sits between two delimiters, empty when nothing was written there —
		// so a trailing scope stays visible instead of vanishing.
		{"order_proc:", []byte{':', '.'}, []string{"order_proc", ":", ""}},
		{"order_proc:charge:", []byte{':', '.'}, []string{"order_proc", ":", "charge", ":", ""}},
		{":output", []byte{':', '.'}, []string{"", ":", "output"}},
		{"fetch..fee", []byte{':', '.'}, []string{"fetch", ".", "", ".", "fee"}},
		// Order is not the lexer's business: it reports what it saw, and the caller judges
		// whether a scope may follow a path.
		{"fetch.result:x", []byte{':', '.'}, []string{"fetch", ".", "result", ":", "x"}},
		// A delimiter the caller did not ask for is an ordinary character.
		{"order_proc:charge", []byte{'.'}, []string{"order_proc:charge"}},
		// A whole flag value goes through one pass, commas included — the level a name may
		// hold a comma at is the level it may hold a colon at.
		{`upgrade,"odd,name":charge:fetch.result`, []byte{',', ':', '.'},
			[]string{"upgrade", ",", "odd,name", ":", "charge", ":", "fetch", ".", "result"}},
	}
	for _, c := range cases {
		t.Run(c.in, func(t *testing.T) {
			got, err := Lex(c.in, c.delims...)
			if err != nil {
				t.Fatalf("Lex(%q): %v", c.in, err)
			}
			if strings.Join(got, "|") != strings.Join(c.want, "|") {
				t.Fatalf("got %q, want %q", got, c.want)
			}
		})
	}
}

// The caller reads the sequence by position, so the shape is the contract: odd length, names
// at even indices, one-character delimiters at odd ones. Asserted over every case above,
// since a single missing empty name would shift every index after it.
func TestLexAlwaysAlternates(t *testing.T) {
	for _, in := range []string{
		"order_proc:charge:fetch.result.fee", "output", "order_proc:", ":", "..", "a:.b",
		`"odd:name":"a.task":external.result`, `""`, "fetch..fee", `upgrade,"odd,name":charge`,
	} {
		t.Run(in, func(t *testing.T) {
			got, err := Lex(in, ',', ':', '.')
			if err != nil {
				t.Fatalf("Lex(%q): %v", in, err)
			}
			if len(got)%2 == 0 {
				t.Fatalf("Lex(%q) = %q: a name must open and close the sequence", in, got)
			}
			for i, element := range got {
				if i%2 == 0 {
					continue
				}
				if len(element) != 1 || strings.IndexByte(",:.", element[0]) < 0 {
					t.Errorf("element %d = %q, want one of the delimiters", i, element)
				}
			}
		})
	}
}

// A quote opens a name and closes it at a delimiter, nowhere else. Mid-name quoting would
// give one name two spellings — `cha"rg"e` and `"cha\"rg\"e"` — and only one of them says
// so, which every later comparison of two selectors would have to know about.
func TestLexRefusesQuotesAwayFromADelimiter(t *testing.T) {
	cases := []struct{ in, why string }{
		{`cha"rg"e`, "quote the whole name"},
		{`cha"rge`, "quote the whole name"},
		{`back\slash`, "quote the whole name"},
		{`"odd:name"x:output`, "must end at a delimiter"},
		{`order_proc:"charge"x:fetch`, "must end at a delimiter"},
		{`"unterminated:output`, "unterminated quote"},
		{`"trailing escape\`, "trailing backslash"},
		{`"a\nb"`, `\n is not an escape`},
	}
	for _, c := range cases {
		t.Run(c.in, func(t *testing.T) {
			got, err := Lex(c.in, ':', '.')
			if err == nil {
				t.Fatalf("Lex(%q) = %q, expected an error", c.in, got)
			}
			if !strings.Contains(err.Error(), c.why) {
				t.Errorf("Lex(%q) said %q, which does not mention %q", c.in, err, c.why)
			}
		})
	}
}

// Quoting removes a delimiter's meaning and nothing else, so a name that needed no quotes
// reads the same either way — and an empty name reads empty however it was written, which is
// what makes the trailing scope the only empty worth a meaning.
func TestLexQuotingChangesNothingButTheDelimiters(t *testing.T) {
	quoted, err := Lex(`"fetch"."result"`, ':', '.')
	if err != nil {
		t.Fatalf("Lex: %v", err)
	}
	bare, err := Lex("fetch.result", ':', '.')
	if err != nil {
		t.Fatalf("Lex: %v", err)
	}
	if strings.Join(quoted, "|") != strings.Join(bare, "|") {
		t.Errorf("quoted %q, bare %q — quoting must not change the name", quoted, bare)
	}

	written, err := Lex(`x.""`, ':', '.')
	if err != nil {
		t.Fatalf("Lex: %v", err)
	}
	missing, err := Lex("x.", ':', '.')
	if err != nil {
		t.Fatalf("Lex: %v", err)
	}
	if strings.Join(written, "|") != strings.Join(missing, "|") {
		t.Errorf(`"" and an absent name must read alike, got %q and %q`, written, missing)
	}
}

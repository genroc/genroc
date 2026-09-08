package lsp

// One document's text to its diagnostics. Every answer here is the server's own — the
// structural half is `DecodeStrict` + `Validate`, the type half is `validation.Check` — so the
// editor cannot disagree with what an apply would say. specs/language-server.md §5.

import (
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"genroc/internal/defdoc"
	"genroc/internal/model"
	"genroc/internal/numeric"
	"genroc/internal/validation"
)

const source = "genroc"

// analyse returns every diagnostic for text, in the order they were found. A file may hold
// several definitions; each is indexed and analysed on its own.
func analyse(text string) []diagnostic {
	lines := splitLines(text)
	docs, err := defdoc.ParseAll([]byte(text))
	if err != nil {
		return []diagnostic{{
			Range:    toRange(lines, yamlErrorRange(err)),
			Severity: severityError,
			Source:   source,
			Code:     "def.syntax",
			Message:  strings.TrimPrefix(err.Error(), "yaml: "),
		}}
	}

	out := []diagnostic{}
	for _, doc := range docs {
		out = append(out, analyseDoc(doc, lines)...)
	}
	return out
}

func marshal(v any) ([]byte, error) { return json.Marshal(v) }

// decodeLenient reads as far as the document allows, ignoring keys with no home. Hover uses it
// and diagnostics do not: a reader asking about one slot is not asking about a typo in another,
// and the strict decode is a VERDICT, which is the diagnostics path's job alone.
func decodeLenient(raw []byte, into *model.ProcessDefinition) error {
	return numeric.Decode(raw, into)
}

func analyseDoc(doc *defdoc.Doc, lines []string) []diagnostic {
	raw, err := json.Marshal(doc.Value)
	if err != nil {
		return []diagnostic{at(doc, lines, "", "def.structure", err.Error())}
	}

	var def model.ProcessDefinition
	if err := numeric.DecodeStrict(raw, &def); err != nil {
		return []diagnostic{decodeDiagnostic(doc, lines, err)}
	}

	var out []diagnostic
	if err := def.Validate(); err != nil {
		var ve *model.ValidationError
		if errors.As(err, &ve) {
			for _, f := range ve.Fields {
				out = append(out, at(doc, lines, f.Field, "def.structure", f.Message))
			}
		} else {
			// A hand-written rule — a goto naming no task, a schema document that will not
			// parse. Those report prose with no path, so the document carries them until
			// they carry an address of their own (§2).
			out = append(out, at(doc, lines, "", "def.structure", err.Error()))
		}
		return out
	}

	_, ds := validation.Check(&def)
	for _, d := range ds {
		out = append(out, at(doc, lines, d.Address, string(d.Code), d.Message))
	}
	return out
}

func at(doc *defdoc.Doc, lines []string, address, code, message string) diagnostic {
	span, _ := doc.Locate(address)
	return diagnostic{
		Range:    toRange(lines, span.Value),
		Severity: severityError,
		Source:   source,
		Code:     code,
		Message:  message,
	}
}

// unknownFieldRe reads the key out of encoding/json's message, which is the only place it
// appears: DisallowUnknownFields reports prose and stops at the first. Underlining the right
// key matters more than the shortcut costs, and §5's reflection walk is what replaces this —
// it reports every unknown key with a path instead of one with a name.
var unknownFieldRe = regexp.MustCompile(`unknown field "([^"]+)"`)

func decodeDiagnostic(doc *defdoc.Doc, lines []string, err error) diagnostic {
	msg := err.Error()
	m := unknownFieldRe.FindStringSubmatch(msg)
	if m == nil {
		return at(doc, lines, "", "def.structure", msg)
	}
	key := m[1]
	if path, ok := solePathEndingIn(doc, key); ok {
		d := at(doc, lines, path, "def.unknown_key", fmt.Sprintf("unknown field %q", key))
		// The key, not its value: it is the spelling that is wrong.
		if span, found := doc.Locate(path); found && !span.Key.Empty() {
			d.Range = toRange(lines, span.Key)
		}
		return d
	}
	return at(doc, lines, "", "def.unknown_key", msg)
}

// solePathEndingIn finds the one place key appears. Several occurrences means the decoder's
// name does not identify a node, and pointing at either would be a guess.
//
// Matches are counted by SPAN, not by path: every node is addressable twice — physically and
// logically — so counting paths would find two of everything and never decide.
func solePathEndingIn(doc *defdoc.Doc, key string) (string, bool) {
	var found string
	seen := map[defdoc.Span]bool{}
	for _, p := range doc.Paths() {
		if p != key && !strings.HasSuffix(p, "."+key) {
			continue
		}
		span, ok := doc.Span(p)
		if !ok || seen[span] {
			continue
		}
		seen[span] = true
		found = p
	}
	return found, len(seen) == 1
}

// yamlErrorRange reads the line out of a yaml.v3 parse failure, which reports it only in
// prose. An unreadable one falls back to the top of the file rather than to nothing.
var yamlLineRe = regexp.MustCompile(`line (\d+):`)

func yamlErrorRange(err error) defdoc.Range {
	r := defdoc.Range{Line: 1, Col: 1, EndLine: 1, EndCol: 1}
	if m := yamlLineRe.FindStringSubmatch(err.Error()); m != nil {
		if n, e := strconv.Atoi(m[1]); e == nil {
			r.Line, r.EndLine = n, n
		}
	}
	return r
}

package lsp

// One document's text to its diagnostics. Every answer here is the server's own — the
// structural half is `DecodeStrict` + `Validate`, the type half is `validation.Check` — so the
// editor cannot disagree with what an apply would say. specs/language-server.md §5.

import (
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"regexp"
	"strconv"
	"strings"

	"genroc/internal/defdoc"
	"genroc/internal/model"
	"genroc/internal/numeric"
	"genroc/internal/schema"
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
			// A hand-written rule. Most now carry the slot they came from; the rest fall back
			// to the value their message names (locatedRange).
			out = append(out, at(doc, lines, model.PathOf(err), "def.structure", err.Error()))
		}
		return out
	}

	_, ds := validation.Check(&def)
	for _, d := range ds {
		// Location, not Address: the reader wants the line underlined, and Address is the
		// scope that line is written in. specs/language-server.md §7b.
		out = append(out, at(doc, lines, d.Location, string(d.Code), d.Message))
	}
	return out
}

func at(doc *defdoc.Doc, lines []string, address, code, message string) diagnostic {
	r := locatedRange(doc, address, message)
	return diagnostic{
		Range:    toRange(lines, r),
		Severity: severityError,
		Source:   source,
		Code:     code,
		Message:  message,
	}
}

// locatedRange is where to underline. An address resolves directly; without one — the
// hand-written rules in model.Validate and the decoders report prose — the message names the
// offending VALUE instead, so look for the node holding it.
//
// Failing both, the FIRST LINE. Falling back to the document root underlines every line of the
// file for one bad word, which is what a reader sees while still typing it.
func locatedRange(doc *defdoc.Doc, address, message string) defdoc.Range {
	if address != "" {
		if span, ok := doc.Locate(address); ok {
			return span.Value
		}
	}
	if r, ok := nodeHoldingQuoted(doc, message); ok {
		return r
	}
	return defdoc.Range{Line: 1, Col: 1, EndLine: 1, EndCol: 1}
}

var quotedRe = regexp.MustCompile(`"([^"]*)"`)

// nodeHoldingQuoted finds the sole node whose value is one of the message's quoted words. The
// LAST quoted word is tried first: a message names its subject before its complaint, so
// `task "tick" switch: goto "$" is not a known task` is about the `$`.
func nodeHoldingQuoted(doc *defdoc.Doc, message string) (defdoc.Range, bool) {
	quoted := quotedRe.FindAllStringSubmatch(message, -1)
	for i := len(quoted) - 1; i >= 0; i-- {
		word := quoted[i][1]
		if word == "" {
			continue
		}
		var found defdoc.Range
		seen := 0
		for _, path := range doc.Paths() {
			if v, ok := doc.ValueAt(path); !ok || v != any(word) {
				continue
			}
			span, ok := doc.Span(path)
			if !ok || span.Value == found {
				continue
			}
			found, seen = span.Value, seen+1
		}
		if seen == 1 {
			return found, true
		}
	}
	return defdoc.Range{}, false
}

// unknownFieldRe reads the key out of encoding/json's message, which is the only place it
// appears: DisallowUnknownFields reports prose and stops at the first. Underlining the right
// key matters more than the shortcut costs, and §5's reflection walk is what replaces this —
// it reports every unknown key with a path instead of one with a name.
var unknownFieldRe = regexp.MustCompile(`unknown field "([^"]+)"`)

func decodeDiagnostic(doc *defdoc.Doc, lines []string, err error) diagnostic {
	msg := err.Error()
	if m := unknownFieldRe.FindStringSubmatch(msg); m != nil {
		key := m[1]
		if path, ok := solePathEndingIn(doc, key); ok {
			return keyDiagnostic(doc, lines, path, fmt.Sprintf("unknown field %q", key))
		}
		return at(doc, lines, "", "def.unknown_key", msg)
	}
	// A schema reports where it failed relative to its OWN root (`properties.who`), because a
	// sub-schema does not know which slot of which document holds it. The document path ends
	// in that one, so the suffix finds it.
	if p := schema.PathOf(err); p != "" {
		if path, ok := solePathEndingIn(doc, p); ok {
			if errors.Is(err, schema.ErrUnknownKeyword) {
				return keyDiagnostic(doc, lines, path, msg)
			}
			return at(doc, lines, path, "def.structure", msg)
		}
	}
	// A schema that is not an object at all reports the slot IS the mistake, with no path
	// inside it to name; the document holds few schema slots, so ask which one that is.
	if errors.Is(err, schema.ErrNotASchema) {
		if path, ok := soleSchemaSlotNotAnObject(doc); ok {
			return at(doc, lines, path, "def.structure", msg)
		}
	}
	// encoding/json names the field that failed LAST in a dotted stack that skips list indices
	// and map keys ("tasks.only_once"), so the whole of it rarely resolves and its last segment
	// usually does. Failing that, the stack itself is still nearer than the top of the file.
	var typeErr *json.UnmarshalTypeError
	if errors.As(err, &typeErr) && typeErr.Field != "" {
		msg = typeErrorMessage(typeErr, msg)
		if path, ok := solePathEndingIn(doc, lastSegment(typeErr.Field)); ok {
			return at(doc, lines, path, "def.structure", msg)
		}
		if _, ok := doc.Locate(typeErr.Field); ok {
			return at(doc, lines, typeErr.Field, "def.structure", msg)
		}
	}
	return at(doc, lines, "", "def.structure", msg)
}

// typeErrorMessage says what the field takes in the words the document is written in.
// encoding/json's own prose names the Go type that could not hold the value
// ("json: cannot unmarshal number into Go struct field Task.tasks.only_once of type bool"),
// which is an implementation detail wherever it is read. Falls back to that prose for a type
// with no such word.
func typeErrorMessage(e *json.UnmarshalTypeError, raw string) string {
	want := goTypeWord(e.Type)
	got := jsonValueWord(e.Value)
	if want == "" || got == "" {
		return raw
	}
	return fmt.Sprintf("%s must be %s, not %s", lastSegment(e.Field), want, got)
}

func goTypeWord(t reflect.Type) string {
	for t != nil && t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	if t == nil {
		return ""
	}
	switch t.Kind() {
	case reflect.Bool:
		return "a boolean"
	case reflect.String:
		return "a string"
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64,
		reflect.Float32, reflect.Float64:
		return "a number"
	case reflect.Slice, reflect.Array:
		return "a list"
	case reflect.Map, reflect.Struct:
		return "an object"
	}
	return ""
}

func jsonValueWord(value string) string {
	switch value {
	case "bool":
		return "a boolean"
	case "string":
		return "a string"
	case "number":
		return "a number"
	case "array":
		return "a list"
	case "object":
		return "an object"
	case "null":
		return "null"
	}
	return ""
}

// keyDiagnostic underlines a node's KEY rather than its value, for the mistakes that are in
// the spelling. It falls back to the value's range where the key has none (a list element).
func keyDiagnostic(doc *defdoc.Doc, lines []string, path, message string) diagnostic {
	d := at(doc, lines, path, "def.unknown_key", message)
	if span, found := doc.Locate(path); found && !span.Key.Empty() {
		d.Range = toRange(lines, span.Key)
	}
	return d
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

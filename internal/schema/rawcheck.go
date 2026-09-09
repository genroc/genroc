package schema

// The shape of a schema document, read before it is decoded. encoding/json's own type error
// names a Go type and the OUTERMOST slot it was decoding ("ProcessDefinition.input_schema.
// properties of type map[string]json.RawMessage"), so a reader is pointed at the whole
// input_schema for a mistake in one property. checkRawNode says which property, in the same
// prose + path pair checkDoc uses. specs/language-server.md §2.

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sort"
)

// ErrUnknownKeyword marks a keyword outside the supported subset, so a client can underline
// the KEY — it is the spelling that is wrong, not the value beneath it.
var ErrUnknownKeyword = errors.New("unsupported schema keyword")

// ErrNotASchema marks a value written where a schema goes that is not an object at all. It
// carries no path — the failure is the slot itself, and a sub-schema does not know which slot
// of which document holds it — so a client matches on this to find where it belongs.
var ErrNotASchema = errors.New("a schema must be an object")

// rawSlot is how a keyword holds sub-schemas: one, a map of them, or a list of them. It is the
// raw-JSON half of mapChildren, which cannot serve here because it walks decoded nodes;
// TestRawSlotsCoverEveryChildSlot keeps the two in step.
type rawSlot uint8

const (
	slotNone rawSlot = iota
	slotOne
	slotMap
	slotList
)

var rawSlots = map[string]rawSlot{
	"properties":           slotMap,
	"$defs":                slotMap,
	"items":                slotOne,
	"additionalProperties": slotOne,
	"oneOf":                slotList,
	"anyOf":                slotList,
	"allOf":                slotList,
}

func checkRawNode(data []byte) error {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		// A bare boolean is JSON Schema's true/false schema, which genroc does not have (see
		// additionalProperties below for the reason). Caught here so the author reads why
		// rather than a message naming an internal Go type and no fix.
		if b := bytes.TrimSpace(data); string(b) == "true" || string(b) == "false" {
			return fmt.Errorf("boolean schemas are not supported: write {} for the top type (any value) rather than %s", b)
		}
		if kind := jsonKind(data); kind != "" {
			return fmt.Errorf("%w, not %s", ErrNotASchema, kind)
		}
		return err
	}
	// The allowlist first, over every key: a misspelled keyword is the likelier mistake, and
	// reporting it before whatever is wrong inside a well-spelled one keeps the order stable.
	for _, kw := range sortedKeys(raw) {
		if _, ok := allowedKeywords[kw]; !ok {
			return AtPath(kw, fmt.Errorf("%w %q", ErrUnknownKeyword, kw))
		}
	}
	for _, kw := range readingOrder(raw) {
		v := raw[kw]
		// null decodes into every one of these as a no-op, and checkDoc reports the ones that
		// are meaningless. Rejecting it here would make an already-stored schema undecodable.
		if jsonKind(v) == "null" {
			continue
		}
		// Only the typed (schema-object) form of additionalProperties is supported; the
		// boolean form is rejected so genroc never accepts untyped extra data (true) and so
		// "closed" is always expressed by absence rather than an explicit false.
		if kw == "additionalProperties" && jsonKind(v) == "a boolean" {
			return AtPath(kw, fmt.Errorf("additionalProperties must be a schema object; the boolean form is not supported"))
		}
		if want := allowedKeywords[kw].kind; !accepts(want, jsonKind(v)) {
			return AtPath(kw, fmt.Errorf("%s must be %s, not %s", kw, kindWord(want), jsonKind(v)))
		}
		if err := checkRawSlot(kw, v); err != nil {
			return err
		}
	}
	return nil
}

// checkRawSlot descends into the sub-schemas a keyword holds, labelling each hop the way
// checkDoc does: prose a reader follows, path a document index resolves.
func checkRawSlot(kw string, v json.RawMessage) error {
	switch rawSlots[kw] {
	case slotOne:
		sl := childSlot{kw: kw, idx: -1}
		if err := checkRawNode(v); err != nil {
			return AtPath(pathLabel(sl), fmt.Errorf("%s: %w", errLabel(sl), err))
		}
	case slotMap:
		var m map[string]json.RawMessage
		if err := json.Unmarshal(v, &m); err != nil {
			return err
		}
		for _, key := range sortedKeys(m) {
			if jsonKind(m[key]) == "null" {
				continue
			}
			sl := childSlot{kw: kw, key: key, idx: -1}
			if err := checkRawNode(m[key]); err != nil {
				return AtPath(pathLabel(sl), fmt.Errorf("%s: %w", errLabel(sl), err))
			}
		}
	case slotList:
		var arr []json.RawMessage
		if err := json.Unmarshal(v, &arr); err != nil {
			return err
		}
		for i, el := range arr {
			if jsonKind(el) == "null" {
				continue
			}
			sl := childSlot{kw: kw, idx: i}
			if err := checkRawNode(el); err != nil {
				return AtPath(pathLabel(sl), fmt.Errorf("%s: %w", errLabel(sl), err))
			}
		}
	}
	return nil
}

// jsonKind names what a value is in the words the author wrote it in, with the article that
// makes it read as a phrase. "" for text that is not JSON at all.
func jsonKind(raw json.RawMessage) string {
	b := bytes.TrimSpace(raw)
	if len(b) == 0 {
		return ""
	}
	switch b[0] {
	case '{':
		return "an object"
	case '[':
		return "a list"
	case '"':
		return "a string"
	case 't', 'f':
		return "a boolean"
	case 'n':
		return "null"
	case '-', '0', '1', '2', '3', '4', '5', '6', '7', '8', '9':
		return "a number"
	}
	return ""
}

// accepts reports whether a keyword's declared kind admits a value of this JSON kind. An
// unknown kind ("default" takes any value) admits everything.
func accepts(kind, got string) bool {
	switch kind {
	case "string|array":
		return got == "a string" || got == "a list"
	case "object":
		return got == "an object"
	case "array":
		return got == "a list"
	case "number", "integer":
		return got == "a number"
	case "boolean":
		return got == "a boolean"
	case "string":
		return got == "a string"
	}
	return true
}

func kindWord(kind string) string {
	switch kind {
	case "string|array":
		return "a string or a list of strings"
	case "object":
		return "an object"
	case "array":
		return "a list"
	case "number":
		return "a number"
	case "integer":
		return "a whole number"
	case "boolean":
		return "a boolean"
	case "string":
		return "a string"
	}
	return "a value"
}

// readingOrder walks a schema's keywords the way a person reads them, so a document with two
// mistakes always reports the same one.
func readingOrder(raw map[string]json.RawMessage) []string {
	keys := sortedKeys(raw)
	slices.SortStableFunc(keys, func(a, b string) int {
		ra, rb := KeywordRank(a), KeywordRank(b)
		if ra < 0 {
			ra = len(keywordOrder)
		}
		if rb < 0 {
			rb = len(keywordOrder)
		}
		return ra - rb
	})
	return keys
}

func sortedKeys(raw map[string]json.RawMessage) []string {
	keys := make([]string, 0, len(raw))
	for k := range raw {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

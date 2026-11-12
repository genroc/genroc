package model

import (
	"encoding/json"
	"maps"
	"strings"
	"testing"

	"genroc/internal/numeric"
)

// An `x-` key at every structural position and on schema nodes, beside an `x-` key that is DATA
// (an output shape's own key), which must survive too but as part of the value.
const extendedDoc = `{
	"name": "p", "x-builder": {"zoom": 2, "big": 12345678901234567890123},
	"input_schema": {"type": "object", "x-s": 1, "properties": {"a": {"type": "string", "x-order": 3}}},
	"$defs": {"D": {"type": "object", "x-d": true}},
	"tasks": [
		{"id": "a", "x-pos": {"x": 10, "y": 20},
		 "action": {"type": "fetch", "method": "post", "url": "u", "x-a": "fetch",
		            "timeout": {"for": "1s", "x-t": 1}},
		 "on_error": [{"code": ["http.500"], "retry": {"retries": 1, "x-r": 1}, "goto": "end", "x-rule": 1}],
		 "switch": [{"case": "true", "goto": "next", "x-case": 1}, {"raise": {"code": "c", "message": "m", "x-f": 1}}]},
		{"id": "b", "action": {"type": "child_map", "children": {"k": {"name": "c", "x-e": 1}}}, "switch": "end"}
	],
	"output": {"x-id": "data, not an extension"}
}`

// The model never holds an `x-` key — only the document it was decoded from does, and that is what
// an apply stores. The one left in the marshal is DATA: a key of the output shape.
func TestXKeysAreSkippedByTheModelAndKeptInItsDocument(t *testing.T) {
	want := xKeys(t, []byte(extendedDoc))
	if len(want) != 13 {
		t.Fatalf("the fixture holds %d x- keys, want 13: %v", len(want), want)
	}
	for name, decode := range map[string]func([]byte, any) error{
		"at the door":  numeric.DecodeStrict,
		"a stored row": json.Unmarshal,
	} {
		t.Run(name, func(t *testing.T) {
			var d ProcessDefinition
			if err := decode([]byte(extendedDoc), &d); err != nil {
				t.Fatalf("decode: %v", err)
			}
			if got := xKeys(t, d.Document()); !maps.Equal(got, want) {
				t.Errorf("the document holds %v, want every key as written: %v", got, want)
			}
			out, err := json.Marshal(&d)
			if err != nil {
				t.Fatal(err)
			}
			if got := xKeys(t, out); len(got) != 1 || got[".output.x-id"] == "" {
				t.Errorf("the model marshals %v — only the output's own key is data, the rest never reach runtime", got)
			}
		})
	}
}

// Strictness belongs to the caller: the door refuses an unknown key, and a stored row carrying one
// (a field since removed, like a delay's `ms`) still decodes. Keeping `x-` keys must not move that.
func TestAnUnknownKeyIsRefusedAtTheDoorAndToleratedInAStoredRow(t *testing.T) {
	for where, doc := range map[string]string{
		"root":            `{"name":"p","zzz":1,"tasks":[{"id":"a","switch":"end"}]}`,
		"task":            `{"name":"p","tasks":[{"id":"a","zzz":1,"switch":"end"}]}`,
		"action":          `{"name":"p","tasks":[{"id":"a","action":{"type":"delay","ms":5},"switch":"end"}]}`,
		"child_map entry": `{"name":"p","tasks":[{"id":"a","action":{"type":"child_map","children":{"k":{"name":"c","zzz":1}}},"switch":"end"}]}`,
	} {
		t.Run(where, func(t *testing.T) {
			var strict ProcessDefinition
			err := numeric.DecodeStrict([]byte(doc), &strict)
			if err == nil || !strings.Contains(err.Error(), `unknown field "`) {
				t.Errorf("at the door: %v, want an unknown-field refusal the editor can place", err)
			}
			var stored ProcessDefinition
			if err := json.Unmarshal([]byte(doc), &stored); err != nil {
				t.Errorf("a stored row: %v — a removed field must not make a stored version unreadable", err)
			}
		})
	}
}

// xKeys maps the path of every `x-` key in a JSON document to its value, numbers exact.
func xKeys(t *testing.T, doc []byte) map[string]string {
	t.Helper()
	var v any
	if err := numeric.Decode(doc, &v); err != nil {
		t.Fatal(err)
	}
	out := map[string]string{}
	var walk func(n any, path string)
	walk = func(n any, path string) {
		switch m := n.(type) {
		case map[string]any:
			for k, c := range m {
				if strings.HasPrefix(k, "x-") {
					b, _ := json.Marshal(c)
					out[path+"."+k] = string(b)
				}
				walk(c, path+"."+k)
			}
		case []any:
			for i, c := range m {
				walk(c, path+"["+string(rune('0'+i))+"]")
			}
		}
	}
	walk(v, "")
	return out
}

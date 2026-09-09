package schema

import (
	"encoding/json"
	"strings"
	"testing"
)

// Every place mapChildren finds a sub-schema, checkRawNode must descend into as well — a slot
// it does not know is one whose errors report against the parent, which is the whole bug this
// check exists to fix.
func TestRawSlotsCoverEveryChildSlot(t *testing.T) {
	full := &node{
		Properties:           map[string]*node{"a": {}},
		Items:                &node{},
		AdditionalProperties: &node{},
		OneOf:                []*node{{}},
		AnyOf:                []*node{{}},
		AllOf:                []*node{{}},
		Defs:                 map[string]*node{"D": {}},
	}
	for sl := range children(full) {
		if rawSlots[sl.kw] == slotNone {
			t.Errorf("rawSlots has no entry for %q: a mistake inside it would report against its parent", sl.kw)
		}
	}
}

func TestCheckRawNode(t *testing.T) {
	cases := []struct {
		name string
		doc  string
		msg  string
		path string
	}{{
		name: "a property that is not a schema",
		doc:  `{"type":"object","properties":{"who":"type"}}`,
		msg:  "who: a schema must be an object, not a string",
		path: "properties.who",
	}, {
		name: "a keyword given the wrong kind of value",
		doc:  `{"properties":{"who":{"required":"name"}}}`,
		msg:  "who: required must be a list, not a string",
		path: "properties.who.required",
	}, {
		name: "a misspelled keyword deep in the document",
		doc:  `{"properties":{"rows":{"items":{"pattern":"^a"}}}}`,
		msg:  `rows: items: unsupported schema keyword "pattern"`,
		path: "properties.rows.items.pattern",
	}, {
		name: "a union arm that is not a schema",
		doc:  `{"oneOf":[{"type":"string"},3]}`,
		msg:  "oneOf[1]: a schema must be an object, not a number",
		path: "oneOf.1",
	}, {
		name: "a $defs entry that is not a schema",
		doc:  `{"$defs":{"User":["name"]}}`,
		msg:  "$defs.User: a schema must be an object, not a list",
		path: "$defs.User",
	}, {
		name: "the whole document",
		doc:  `"object"`,
		msg:  "a schema must be an object, not a string",
		path: "",
	}}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var n node
			err := json.Unmarshal([]byte(c.doc), &n)
			if err == nil {
				t.Fatalf("decoded a schema that is malformed at %s", c.path)
			}
			if err.Error() != c.msg {
				t.Errorf("message reads\n got: %q\nwant: %q", err.Error(), c.msg)
			}
			if got := PathOf(err); got != c.path {
				t.Errorf("path is %q, want %q: the editor underlines whatever this names", got, c.path)
			}
		})
	}
}

// A null sub-schema decodes today and checkDoc is what reports it. Rejecting it at decode
// would make an already-stored schema undecodable — see nullChildErr.
func TestCheckRawNodeToleratesNull(t *testing.T) {
	var n node
	if err := json.Unmarshal([]byte(`{"properties":{"who":null},"items":null}`), &n); err != nil {
		t.Fatalf("null sub-schema no longer decodes: %v", err)
	}
	err := checkDocRoot(&n)
	if err == nil || !strings.Contains(err.Error(), "null") {
		t.Errorf("checkDoc no longer reports the null property: %v", err)
	}
}

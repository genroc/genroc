package model

import (
	"bytes"
	"encoding/json"
	"reflect"
	"strconv"
	"strings"

	"genroc/internal/extension"
)

// decodeOwnLevel decodes one object into a struct whose fields are its keys, skipping `x-` keys
// and RECORDING any other key with no field rather than refusing it: a decoder cannot see whether
// its caller is strict, so numeric.DecodeStrict asks UnknownKeys afterwards, and a stored row
// holding a since-removed field still loads. specs/extension-keys.md.
func decodeOwnLevel(data []byte, into any) ([]string, error) {
	err := decodeThisLevelStrictly(data, into)
	if _, unknown := unknownField(err); !unknown {
		return nil, err
	}
	var members map[string]json.RawMessage
	if err := json.Unmarshal(data, &members); err != nil {
		return nil, err
	}
	for k := range members {
		if extension.Is(k) {
			delete(members, k)
		}
	}
	var unknown []string
	for {
		rest, err := json.Marshal(members)
		if err != nil {
			return nil, err
		}
		reflect.ValueOf(into).Elem().SetZero()
		err = decodeThisLevelStrictly(rest, into)
		key, ok := unknownField(err)
		if _, present := members[key]; !ok || !present {
			return unknown, err
		}
		unknown = append(unknown, key)
		delete(members, key)
	}
}

// Not numeric.DecodeStrict: that also asks the children's UnknownKeys, and a child's recorded key
// would read as this level's.
func decodeThisLevelStrictly(data []byte, into any) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	dec.DisallowUnknownFields()
	return dec.Decode(into)
}

func unknownField(err error) (string, bool) {
	if err == nil {
		return "", false
	}
	quoted, ok := strings.CutPrefix(err.Error(), "json: unknown field ")
	if !ok {
		return "", false
	}
	key, err := strconv.Unquote(quoted)
	return key, err == nil
}

func (d *ProcessDefinition) UnmarshalJSON(data []byte) error {
	type alias ProcessDefinition
	var a alias
	unknown, err := decodeOwnLevel(data, &a)
	if err != nil {
		return err
	}
	var doc bytes.Buffer
	if err := json.Compact(&doc, data); err != nil {
		return err
	}
	*d = ProcessDefinition(a)
	d.document, d.unknown = doc.Bytes(), unknown
	return nil
}

// Document is the JSON d was decoded from, `x-` keys included; nil for one built in Go.
func (d ProcessDefinition) Document() []byte { return d.document }

func (d ProcessDefinition) UnknownKeys() []string { return d.unknown }

func (t *Task) UnmarshalJSON(data []byte) error {
	type alias Task
	var a alias
	unknown, err := decodeOwnLevel(data, &a)
	if err != nil {
		return err
	}
	*t = Task(a)
	t.unknown = unknown
	return nil
}

func (t Task) UnknownKeys() []string { return t.unknown }

func (a *Action) UnmarshalJSON(data []byte) error {
	type alias Action
	var v alias
	unknown, err := decodeOwnLevel(data, &v)
	if err != nil {
		return err
	}
	*a = Action(v)
	a.unknown = unknown
	return nil
}

func (a Action) UnknownKeys() []string { return a.unknown }

func (c *ChildEntry) UnmarshalJSON(data []byte) error {
	type alias ChildEntry
	var a alias
	unknown, err := decodeOwnLevel(data, &a)
	if err != nil {
		return err
	}
	*c = ChildEntry(a)
	c.unknown = unknown
	return nil
}

func (c ChildEntry) UnknownKeys() []string { return c.unknown }

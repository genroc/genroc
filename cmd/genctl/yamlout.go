package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strings"

	"gopkg.in/yaml.v3"
)

// The one YAML rendering path, surviving the two ways YAML can lie about a value it was handed: a
// number that comes out quoted is a string, and a string that comes out bare may be read back as
// something else. Not yaml.Marshal, because numbers arrive as json.Number to keep large literals
// exact and json.Number is a string type, which the default encoder quotes.

// yamlBlock renders with keys sorted, which is the order they are looked up in.
func yamlBlock(v any) string { return yamlDoc(v, nil) }

// yamlDoc puts the keys named in `lead` first, in that order, and sorts the rest after them --
// a schema reads as what it IS before what it holds, which no encoder will do for a map.
// Applied at every level, so a nested definition reads the same way as the root.
func yamlDoc(v any, lead []string) string {
	var b bytes.Buffer
	enc := yaml.NewEncoder(&b)
	// Two, not yaml.Marshal's four: a schema is printed to be pasted into a definition, and
	// definitions are written at two.
	enc.SetIndent(2)
	if err := enc.Encode(yamlNode(v, lead)); err != nil {
		return ""
	}
	enc.Close()
	return strings.TrimRight(b.String(), "\n")
}

func printYAMLDoc(v any, lead []string) { fmt.Println(yamlDoc(v, lead)) }

func yamlNode(v any, lead []string) *yaml.Node {
	switch t := v.(type) {
	case json.Number:
		// The literal verbatim and UNTAGGED: the emitter writes the text it is given rather
		// than reformatting through float64, so 1.10 and a 30-digit integer survive.
		return &yaml.Node{Kind: yaml.ScalarNode, Value: t.String()}
	case map[string]any:
		n := &yaml.Node{Kind: yaml.MappingNode}
		for _, k := range orderKeys(t, lead) {
			// The key through Encode, not a bare scalar: the encoder quotes what YAML would
			// otherwise read as something else -- `n` and `yes` are booleans, `123` a number.
			key := &yaml.Node{}
			_ = key.Encode(k)
			n.Content = append(n.Content, key, yamlNode(t[k], lead))
		}
		return n
	case []any:
		n := &yaml.Node{Kind: yaml.SequenceNode}
		for _, e := range t {
			n.Content = append(n.Content, yamlNode(e, lead))
		}
		return n
	default:
		n := &yaml.Node{}
		if err := n.Encode(v); err != nil {
			return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!null", Value: "null"}
		}
		return n
	}
}

// orderKeys is `lead` first in the order given, then everything else sorted -- so an unlisted
// key shows up rather than disappearing.
func orderKeys(m map[string]any, lead []string) []string {
	out := make([]string, 0, len(m))
	seen := make(map[string]bool, len(lead))
	for _, k := range lead {
		if _, ok := m[k]; ok {
			out = append(out, k)
			seen[k] = true
		}
	}
	for _, k := range slices.Sorted(maps.Keys(m)) {
		if !seen[k] {
			out = append(out, k)
		}
	}
	return out
}

package main

import (
	"encoding/json"
	"fmt"

	"gopkg.in/yaml.v3"
)

// yamlToAny converts a YAML tree to JSON-native values keeping numeric literals exact:
// decoding into `any` floats big integers (a 54-digit id once left here as 1.2e+53).
// Scalars ride as json.Number; non-JSON literals (0x1F, 1_000, .inf) fall back to yaml.
func yamlToAny(n *yaml.Node) (any, error) {
	switch n.Kind {
	case yaml.DocumentNode:
		if len(n.Content) == 0 {
			return nil, nil
		}
		return yamlToAny(n.Content[0])

	case yaml.MappingNode:
		out := make(map[string]any, len(n.Content)/2)
		var merged []map[string]any
		for i := 0; i+1 < len(n.Content); i += 2 {
			var key string
			if err := n.Content[i].Decode(&key); err != nil {
				return nil, fmt.Errorf("line %d: object key must be a scalar: %w", n.Content[i].Line, err)
			}
			if key == mergeKey {
				m, err := mergeSource(n.Content[i+1])
				if err != nil {
					return nil, err
				}
				merged = append(merged, m)
				continue
			}
			v, err := yamlToAny(n.Content[i+1])
			if err != nil {
				return nil, err
			}
			out[key] = v
		}
		// An explicit key beats a merged one — YAML's own precedence, so there is no new
		// rule to learn. Applied after the loop because a merge may appear above the key
		// it is overridden by.
		for _, m := range merged {
			for k, v := range m {
				if _, ok := out[k]; !ok {
					out[k] = v
				}
			}
		}
		return out, nil

	case yaml.SequenceNode:
		out := make([]any, 0, len(n.Content))
		for _, c := range n.Content {
			v, err := yamlToAny(c)
			if err != nil {
				return nil, err
			}
			out = append(out, v)
		}
		return out, nil

	case yaml.AliasNode:
		return yamlToAny(n.Alias)

	case yaml.ScalarNode:
		if n.Tag == "!!int" || n.Tag == "!!float" {
			// A bare number is itself a valid JSON document, so this rejects
			// exactly the YAML spellings JSON cannot express.
			if json.Valid([]byte(n.Value)) {
				return json.Number(n.Value), nil
			}
		}
		var v any
		if err := n.Decode(&v); err != nil {
			return nil, fmt.Errorf("line %d: %w", n.Line, err)
		}
		return v, nil
	}

	var v any
	if err := n.Decode(&v); err != nil {
		return nil, err
	}
	return v, nil
}

// mergeKey is YAML's merge key. Without handling it here the alias landed under a literal
// "<<" field, which the server ignores as unknown and the canonical re-marshal strips — so
// a definition using anchors silently lost every merged key. yaml.v3's own decoder merges
// correctly, so the two readers of one file disagreed.
const mergeKey = "<<"

// mergeSource resolves a `<<` value to the map it contributes.
//
// The SEQUENCE form is refused rather than implemented: YAML 1.1 gives EARLIER entries
// precedence, the opposite of every other merge in use (`{...a, ...b}`, `{**a, **b}`, the
// CSS cascade), so `<<: [*base, *override]` would silently do the reverse of what it reads
// as. One anchor, or nesting, covers the same ground with no ambiguity.
func mergeSource(n *yaml.Node) (map[string]any, error) {
	if n.Kind == yaml.SequenceNode {
		return nil, fmt.Errorf("line %d: `<<` takes a single mapping - a sequence is refused "+
			"because YAML gives its EARLIER entries precedence, so it reads backwards; "+
			"merge into one anchor instead", n.Line)
	}
	target := n
	if target.Kind == yaml.AliasNode {
		target = target.Alias
	}
	if target.Kind != yaml.MappingNode {
		return nil, fmt.Errorf("line %d: `<<` needs a mapping, or an alias to one", n.Line)
	}
	v, err := yamlToAny(target)
	if err != nil {
		return nil, err
	}
	m, ok := v.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("line %d: `<<` needs a mapping", n.Line)
	}
	return m, nil
}

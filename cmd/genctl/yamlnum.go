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
		for i := 0; i+1 < len(n.Content); i += 2 {
			var key string
			if err := n.Content[i].Decode(&key); err != nil {
				return nil, fmt.Errorf("line %d: object key must be a scalar: %w", n.Content[i].Line, err)
			}
			v, err := yamlToAny(n.Content[i+1])
			if err != nil {
				return nil, err
			}
			out[key] = v
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

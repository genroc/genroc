package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"

	"genroc/internal/defdoc"
)

// ── input assembly (genctl run) ────────────────────────────────────────────────

// buildInput overlays --set on one base source, which must then be an object. present=false
// means no source at all, so the value is omitted entirely.
func buildInput(literal, file string, sets []string) (any, bool, error) {
	base, present, err := readBase(literal, file)
	if err != nil {
		return nil, false, err
	}
	if len(sets) > 0 {
		m, ok := base.(map[string]any)
		if base == nil {
			m, ok = map[string]any{}, true
		}
		if !ok {
			return nil, false, fmt.Errorf("--set needs the base to be an object, but it is %T", base)
		}
		for _, s := range sets {
			if err := applySet(m, s); err != nil {
				return nil, false, err
			}
		}
		base, present = m, true
	}
	return base, present, nil
}

// readBase takes file as a bare path, so the shell tab-completes it.
func readBase(literal, file string) (any, bool, error) {
	if literal != "" && file != "" {
		return nil, false, fmt.Errorf("provide the value inline or with -f, not both")
	}
	var data []byte
	switch {
	case file != "":
		b, err := os.ReadFile(file)
		if err != nil {
			return nil, false, err
		}
		data = b
	case literal == "-":
		b, err := io.ReadAll(os.Stdin)
		if err != nil {
			return nil, false, err
		}
		data = b
	case literal != "":
		data = []byte(literal)
	default:
		return nil, false, nil
	}
	v, err := parseRelaxed(data)
	if err != nil {
		return nil, false, fmt.Errorf("parse value: %w", err)
	}
	return v, true, nil
}

// parseRelaxed is YAML (a JSON superset, so {name: Sam} works too), with numeric literals kept
// exact by defdoc.
func parseRelaxed(data []byte) (any, error) {
	d, err := defdoc.Parse(data)
	if err != nil {
		return nil, err
	}
	return d.Value, nil
}

func applySet(m map[string]any, kv string) error {
	eq := strings.IndexByte(kv, '=')
	if eq < 0 {
		return fmt.Errorf("--set %q must be key=value", kv)
	}
	key, val := kv[:eq], kv[eq+1:]
	if key == "" {
		return fmt.Errorf("--set %q has an empty key", kv)
	}
	return setPath(m, strings.Split(key, "."), inferScalar(val))
}

func setPath(m map[string]any, path []string, val any) error {
	for i := 0; i < len(path)-1; i++ {
		child, ok := m[path[i]]
		if !ok {
			next := map[string]any{}
			m[path[i]], m = next, next
			continue
		}
		next, ok := child.(map[string]any)
		if !ok {
			return fmt.Errorf("--set: %q is already set to a non-object", strings.Join(path[:i+1], "."))
		}
		m = next
	}
	m[path[len(path)-1]] = val
	return nil
}

// inferScalar: true/false/null, then a JSON number kept as its literal, else the string as is.
// A value that must stay a string, or an array, needs --input.
func inferScalar(s string) any {
	switch s {
	case "true":
		return true
	case "false":
		return false
	case "null":
		return nil
	}
	// Not ParseInt/ParseFloat: they cap at int64 and round. json.Number accepts exactly JSON
	// number syntax, so any other word falls through to a string.
	var num json.Number
	if err := json.Unmarshal([]byte(s), &num); err == nil {
		return num
	}
	return s
}

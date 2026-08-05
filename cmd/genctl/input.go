package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"

	"gopkg.in/yaml.v3"
)

// ── input assembly (genctl run) ────────────────────────────────────────────────

// buildInput assembles an input/result from one base source (--input/--result literal, "-"
// stdin, or -f file) plus --set overrides (base must then be an object). present=false
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

// readBase resolves the base value from the mutually-exclusive literal (a JSON/YAML
// literal, or "-" for stdin) and file (a path — bare, so the shell tab-completes it)
// sources. Returns present=false when neither is set.
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

// parseRelaxed parses data as YAML — a JSON superset, so strict JSON works alongside
// shell-friendly forms ({name: Sam}). Yields JSON-native types with numeric literals
// preserved exactly (yamlToAny), so a long --set id reaches the server as written.
func parseRelaxed(data []byte) (any, error) {
	var node yaml.Node
	if err := yaml.Unmarshal(data, &node); err != nil {
		return nil, err
	}
	return yamlToAny(&node)
}

// applySet applies one "key=value" (or "a.b.c=value") override onto m, inferring
// the value's type and creating nested objects for dotted keys.
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

// setPath walks/creates the nested objects named by path and sets the final key.
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

// inferScalar maps a --set value string to a JSON-native scalar: true/false/null,
// then integer, then float, else the string unchanged. Use --input for values that
// must stay strings (e.g. "007") or for arrays / deep structures.
func inferScalar(s string) any {
	switch s {
	case "true":
		return true
	case "false":
		return false
	case "null":
		return nil
	}
	// Keep a number as its literal: ParseInt caps at int64 and ParseFloat rounds, which sent
	// `--set id=<54 digits>` as 1.23e+53. json.Number accepts exactly JSON number syntax,
	// so any other word still falls through to a plain string.
	var num json.Number
	if err := json.Unmarshal([]byte(s), &num); err == nil {
		return num
	}
	return s
}

package model

import (
	"fmt"
	"sort"
	"strconv"
	"strings"

	"genroc/internal/schema"
)

// ResolveConfig tries GENROC_<PROCESS>_<NAME>, then GENROC_GLOBAL_<NAME> (both upper-snaked,
// so the schema may use any case), then the default; a missing required var is an error.
// Values are coerced and validated against ConfigSchema, and never persisted.
func (d *ProcessDefinition) ResolveConfig(lookup func(string) (string, bool)) (map[string]any, error) {
	if d.ConfigSchema == nil {
		return map[string]any{}, nil
	}
	procPrefix := "GENROC_" + envToken(d.Name) + "_"
	const globalPrefix = "GENROC_GLOBAL_"
	required := make(map[string]bool, len(d.ConfigSchema.Required()))
	for _, r := range d.ConfigSchema.Required() {
		required[r] = true
	}
	props := d.ConfigSchema.Properties()
	out := make(map[string]any, len(props))
	for name, prop := range props {
		key := envToken(name) // declared name (any case) → UPPER_SNAKE env suffix
		raw, ok := lookup(procPrefix + key)
		if !ok || raw == "" {
			raw, ok = lookup(globalPrefix + key)
		}
		if !ok || raw == "" {
			if def := prop.Default(); def != nil {
				out[name] = def
				continue
			}
			if required[name] {
				return nil, fmt.Errorf("config %q is required but neither %s%s nor %s%s is set", name, procPrefix, key, globalPrefix, key)
			}
			continue
		}
		val, err := coerceConfigValue(name, propType(prop), raw, prop.IsSecret())
		if err != nil {
			return nil, err
		}
		out[name] = val
	}
	if _, err := d.ConfigSchema.Validate(out); err != nil {
		// Schema-validation errors (enum/range) can echo the offending value, so
		// scrub any secret values that reach the message.
		msg := err.Error()
		for _, sv := range d.SecretConfigValues(out) {
			msg = strings.ReplaceAll(msg, sv, "***")
		}
		return nil, fmt.Errorf("config: %s", msg)
	}
	return out, nil
}

// propType returns a primitive config property's single declared type, or "" (treated as
// string) when none is set.
func propType(prop schema.Schema) string {
	if t := prop.Type(); len(t) > 0 {
		return t[0]
	}
	return ""
}

// envToken upper-snakes a name for config env var names, splitting camel humps:
// apiKey -> API_KEY, URLPath -> URL_PATH.
func envToken(s string) string {
	runes := []rune(s)
	out := make([]byte, 0, len(runes)+4)
	sep := func() {
		if n := len(out); n > 0 && out[n-1] != '_' {
			out = append(out, '_')
		}
	}
	for i, r := range runes {
		switch {
		case r >= 'a' && r <= 'z':
			out = append(out, byte(r-('a'-'A')))
		case r >= '0' && r <= '9':
			out = append(out, byte(r))
		case r >= 'A' && r <= 'Z':
			// A camel boundary: after a lowercase/digit (apiKey), or the word after an
			// acronym (URLPath).
			if i > 0 {
				prev := runes[i-1]
				prevLowerDigit := (prev >= 'a' && prev <= 'z') || (prev >= '0' && prev <= '9')
				prevUpper := prev >= 'A' && prev <= 'Z'
				nextLower := i+1 < len(runes) && runes[i+1] >= 'a' && runes[i+1] <= 'z'
				if prevLowerDigit || (prevUpper && nextLower) {
					sep()
				}
			}
			out = append(out, byte(r))
		default:
			sep()
		}
	}
	for len(out) > 0 && out[len(out)-1] == '_' {
		out = out[:len(out)-1]
	}
	return string(out)
}

// SecretConfigValues returns the string forms of resolved secret:true config values, used
// to scrub secrets from config-resolution error messages.
func (d *ProcessDefinition) SecretConfigValues(resolved map[string]any) []string {
	if d.ConfigSchema == nil {
		return nil
	}
	var secrets []string
	for name, prop := range d.ConfigSchema.Properties() {
		if !prop.IsSecret() {
			continue
		}
		if v, ok := resolved[name]; ok {
			if s := schema.SecretString(v); s != "" {
				secrets = append(secrets, s)
			}
		}
	}
	// Longest-first so substring scrubbing redacts the most specific value before a
	// shorter one that is its prefix can pre-empt it and leave the tail exposed.
	sort.Slice(secrets, func(i, j int) bool { return len(secrets[i]) > len(secrets[j]) })
	return secrets
}

// coerceConfigValue never echoes a secret value in an error: it would reach the CLI, the
// instance's error and the logs.
func coerceConfigValue(name, typ, raw string, secret bool) (any, error) {
	shown := raw
	if secret {
		shown = "***"
	}
	switch typ {
	case "", "string":
		return raw, nil
	case "integer":
		n, err := strconv.ParseInt(raw, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("config %q: %q is not a valid integer", name, shown)
		}
		return n, nil
	case "number":
		f, err := strconv.ParseFloat(raw, 64)
		if err != nil {
			return nil, fmt.Errorf("config %q: %q is not a valid number", name, shown)
		}
		return f, nil
	case "boolean":
		b, err := strconv.ParseBool(raw)
		if err != nil {
			return nil, fmt.Errorf("config %q: %q is not a valid boolean (use true/false)", name, shown)
		}
		return b, nil
	default:
		return nil, fmt.Errorf("config %q: unsupported type %q", name, typ)
	}
}

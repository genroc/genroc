package main

import (
	"encoding/json"
	"reflect"
	"strings"
)

// configSchema is ui.yaml as JSON Schema, read off Config's tags. genrocspec runs
// `genroc-ui config-schema` to render the reference, as it runs `genroc -h`: the root module
// cannot import this one.
func configSchema() []byte {
	defs := map[string]any{}
	root := objectSchema(reflect.TypeFor[Config](), defs)
	root["$schema"] = "https://json-schema.org/draft/2020-12/schema"
	root["$defs"] = defs
	b, err := json.MarshalIndent(root, "", "  ")
	if err != nil {
		panic(err) // maps of strings and bools always marshal
	}
	return append(b, '\n')
}

func objectSchema(t reflect.Type, defs map[string]any) map[string]any {
	props := map[string]any{}
	var required []string
	for i := range t.NumField() {
		f := t.Field(i)
		name, _, _ := strings.Cut(f.Tag.Get("yaml"), ",")
		if name == "" || name == "-" {
			continue
		}
		p := typeSchema(f.Type, defs)
		p["description"] = f.Tag.Get("description")
		if d := f.Tag.Get("default"); d != "" {
			p["default"] = d
		}
		if f.Tag.Get("required") == "true" {
			required = append(required, name)
		}
		props[name] = p
	}
	s := map[string]any{"type": "object", "properties": props}
	if len(required) > 0 {
		s["required"] = required
	}
	return s
}

func typeSchema(t reflect.Type, defs map[string]any) map[string]any {
	switch t.Kind() {
	case reflect.Pointer:
		return typeSchema(t.Elem(), defs)
	case reflect.String:
		return map[string]any{"type": "string"}
	case reflect.Bool:
		return map[string]any{"type": "boolean"}
	case reflect.Slice:
		return map[string]any{"type": "array", "items": typeSchema(t.Elem(), defs)}
	case reflect.Map:
		return map[string]any{"type": "object", "additionalProperties": typeSchema(t.Elem(), defs)}
	case reflect.Struct:
		if _, ok := defs[t.Name()]; !ok {
			defs[t.Name()] = objectSchema(t, defs)
		}
		return map[string]any{"$ref": "#/$defs/" + t.Name()}
	}
	panic("configSchema: no schema for " + t.String())
}

// Package defschema projects the definition language into a JSON Schema. It lives beside the
// language rather than in internal/api because it describes a definition, not an endpoint: the
// API serves the bytes, the docs site publishes them, completion reads them.
// specs/language-server.md §5.
package defschema

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"sync"

	"genroc/internal/model"
	"genroc/internal/sources"

	"github.com/swaggest/jsonschema-go"
)

var (
	processSchemaOnce  sync.Once
	processSchemaBytes []byte
)

// ShapeDefName keeps the Shape type's generated def name stable as ModelShape after the
// type moved from package model to package shape (swaggest would otherwise name it
// ShapeShape). Hand-written $refs — the Action schema and Shape's own self-recursion —
// point at ModelShape, so this mapping lets them resolve without edits. Exported because the
// OpenAPI builder reflects the same types and needs the same name.
func ShapeDefName(_ reflect.Type, defaultDefName string) string {
	if defaultDefName == "ShapeShape" {
		return "ModelShape"
	}
	return defaultDefName
}

// Process returns the JSON Schema for a process definition, built once. It is what an editor
// loads (`# yaml-language-server: $schema=`), what GET /process-schema.json serves, and what
// completion reads to know which keys are legal where — see specs/language-server.md §5 for
// why completion uses this and diagnostics do not.
func Process() []byte {
	processSchemaOnce.Do(func() {
		processSchemaBytes = reflectSchema(model.ProcessDefinition{}, "processDefinitionSchema",
			jsonschema.InterceptDefName(ShapeDefName))
	})
	return processSchemaBytes
}

// Config returns the JSON Schema for a project's `.genroc`, reflected from the struct that reads
// it so a field added there reaches the editor with no edit here. The docs site publishes it
// (`# yaml-language-server: $schema=`) and the VS Code extension bundles it. Not cached: it is
// built once per generator run, and a second Once would be a second thing to allow.
func Config() []byte {
	return withoutNull(reflectSchema(sources.Config{}, "configSchema"))
}

// withoutNull drops `null` from every `type` list. The reflector makes a non-omitempty slice
// nullable, and the reader treats `command: null` as an empty command and refuses it -- nothing
// in a `.genroc` is legitimately null. Config only: the process schema's nullability is pinned
// against the server by its own tests.
func withoutNull(b []byte) []byte {
	var root map[string]any
	if err := json.Unmarshal(b, &root); err != nil {
		return b
	}
	var walk func(v any)
	walk = func(v any) {
		switch node := v.(type) {
		case map[string]any:
			if types, ok := node["type"].([]any); ok {
				kept := make([]any, 0, len(types))
				for _, t := range types {
					if t != "null" {
						kept = append(kept, t)
					}
				}
				if len(kept) == 1 {
					node["type"] = kept[0]
				} else {
					node["type"] = kept
				}
			}
			for _, child := range node {
				walk(child)
			}
		case []any:
			for _, child := range node {
				walk(child)
			}
		}
	}
	walk(root)
	out, _ := json.Marshal(root)
	return out
}

// reflectSchema is the projection both schemas share: a field is required unless its json tag
// says omitempty or it is a pointer, `description` tags become the text an editor shows, and
// every struct is closed to unknown keys because the reader is.
func reflectSchema(root any, what string, extra ...func(*jsonschema.ReflectContext)) []byte {
	r := jsonschema.Reflector{}
	r.DefaultOptions = append(r.DefaultOptions, extra...)
	r.DefaultOptions = append(r.DefaultOptions,
		jsonschema.InterceptProp(func(params jsonschema.InterceptPropParams) error {
			if !params.Processed || params.Field.Type == nil || params.ParentSchema == nil {
				return nil
			}
			tag := params.Field.Tag.Get("json")
			if strings.Contains(tag, "omitempty") || params.Field.Type.Kind() == reflect.Ptr {
				return nil
			}
			for _, r := range params.ParentSchema.Required {
				if r == params.Name {
					return nil
				}
			}
			params.ParentSchema.Required = append(params.ParentSchema.Required, params.Name)
			return nil
		}),
		jsonschema.InterceptProp(func(params jsonschema.InterceptPropParams) error {
			if !params.Processed || params.PropertySchema == nil {
				return nil
			}
			if desc := params.Field.Tag.Get("description"); desc != "" {
				params.PropertySchema.WithDescription(desc)
			}
			return nil
		}),
		jsonschema.InterceptSchema(closeStructs),
	)
	s, err := r.Reflect(root)
	if err != nil {
		panic(fmt.Sprintf("%s: %v", what, err))
	}
	b, _ := json.Marshal(s)
	return upgradeToDraft201909(b)
}

// closeStructs rejects unknown keys on every object reflected from a Go struct, because the server
// already does; without it the published schema accepted `on_eror:` on a task, a typo the editor
// passed and the server refused. A struct whose fields are open is left alone -- it carries its
// own additionalProperties, and a schema stricter than the server underlines working code.
// specs/language-server.md §5.
func closeStructs(params jsonschema.InterceptSchemaParams) (bool, error) {
	if !params.Processed || params.Schema == nil || params.Schema.AdditionalProperties != nil {
		return false, nil
	}
	if len(params.Schema.Properties) == 0 {
		return false, nil
	}
	params.Schema.WithAdditionalProperties(jsonschema.SchemaOrBool{TypeBoolean: new(bool)})
	return false, nil
}

// upgradeToDraft201909 rewrites a swaggest-generated schema to JSON Schema
// draft 2019-09 so that $ref and description can coexist on the same node
// (in draft-07, $ref silently swallows all sibling keywords).
func upgradeToDraft201909(b []byte) []byte {
	var root map[string]any
	if err := json.Unmarshal(b, &root); err != nil {
		return b
	}
	if defs, ok := root["definitions"]; ok {
		root["$defs"] = defs
		delete(root, "definitions")
	}
	root["$schema"] = "https://json-schema.org/draft/2019-09/schema"
	// Rewrite all $ref values from #/definitions/ to #/$defs/.
	rewriteRefs(root)
	out, _ := json.Marshal(root)
	return out
}

func rewriteRefs(v any) {
	switch node := v.(type) {
	case map[string]any:
		if ref, ok := node["$ref"].(string); ok {
			node["$ref"] = strings.ReplaceAll(ref, "#/definitions/", "#/$defs/")
		}
		for _, child := range node {
			rewriteRefs(child)
		}
	case []any:
		for _, child := range node {
			rewriteRefs(child)
		}
	}
}
